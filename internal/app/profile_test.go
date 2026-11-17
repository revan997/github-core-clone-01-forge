// Profiling driver for the four timed workloads. NOT part of the shipped
// entry: this is a test-only harness that drives the in-process handler over
// real loopback HTTP (httptest) plus direct handler calls, with pprof CPU
// and heap profiles. Run with FORGE_PROFILE=1; skipped otherwise so
// `go test ./...` stays fast.
//
//	FORGE_PROFILE=1 FORGE_PROFILE_DIR=./profiles go test ./internal/app/ -run TestProfileWorkloads -v -count=1
package app

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"

	"forge/internal/scenario"
	"forge/internal/store"
)

const profileToken = "profile-token"

func profileClient(t *testing.T) *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

func profileReq(t *testing.T, method, url string, body []byte, headers map[string]string) *http.Request {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+profileToken)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

func profileDo(t *testing.T, c *http.Client, req *http.Request, want int) []byte {
	t.Helper()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != want {
		t.Fatalf("%s %s: got status %d, want %d, body %.200s", req.Method, req.URL, resp.StatusCode, want, b)
	}
	return b
}

// buildPushBodyChain crafts a receive-pack body for one new commit on top
// of oldTip, WITHOUT touching the store (pure hash computation), so a whole
// sequential chain can be pre-built before the clock starts. Returns the
// body and the new tip.
func buildPushBodyChain(oldTip string, counter, n int) ([]byte, string) {
	content := bytes.Repeat([]byte(fmt.Sprintf("push-%d-0123456789abcdef", n)), 64) // >1 KiB
	content = content[:1024]
	blobOID := store.HashObject(store.ObjectBlob, content)
	treeBytes := store.BuildTreeBytes([]store.TreeEntry{{Mode: "100644", Name: "file.txt", OID: blobOID}})
	treeOID := store.HashObject(store.ObjectTree, treeBytes)
	ts := store.CommitEpoch + int64(counter)
	commit := fmt.Sprintf("tree %s\nparent %s\nauthor %s %d +0000\ncommitter %s %d +0000\n\npush %d\n",
		treeOID, oldTip, scenario.ForgeIdentity, ts, scenario.ForgeIdentity, ts, n)
	commitOID := store.HashObject(store.ObjectCommit, []byte(commit))
	pack := store.WritePack([]store.GitObject{
		{Type: store.ObjectCommit, Data: []byte(commit)},
		{Type: store.ObjectTree, Data: treeBytes},
		{Type: store.ObjectBlob, Data: content},
	})
	var body bytes.Buffer
	body.Write(store.PktLineString(oldTip + " " + commitOID + " refs/heads/main\x00report-status side-band-64k\n"))
	body.Write(store.PktFlush())
	body.Write(pack)
	return body.Bytes(), commitOID
}

type memSnap struct {
	mallocs    uint64
	totalAlloc uint64
	numGC      uint32
	pauseTotal uint64
	heapAlloc  uint64
}

func snapMem() memSnap {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return memSnap{mallocs: m.Mallocs, totalAlloc: m.TotalAlloc, numGC: m.NumGC, pauseTotal: m.PauseTotalNs, heapAlloc: m.HeapAlloc}
}

func (a memSnap) sub(b memSnap) memSnap {
	return memSnap{mallocs: a.mallocs - b.mallocs, totalAlloc: a.totalAlloc - b.totalAlloc, numGC: a.numGC - b.numGC, pauseTotal: a.pauseTotal - b.pauseTotal, heapAlloc: a.heapAlloc}
}

func TestProfileWorkloads(t *testing.T) {
	if os.Getenv("FORGE_PROFILE") != "1" {
		t.Skip("set FORGE_PROFILE=1 to run the profiling driver")
	}
	dir := os.Getenv("FORGE_PROFILE_DIR")
	if dir == "" {
		dir = "profiles"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAKEFUN_SHOP_TOKEN", profileToken)

	forge := NewForge()
	s := forge.Store
	owner := scenario.Viewer.Login
	repo := s.CreateRepo("bench", false, "")
	if repo == nil {
		t.Fatal("create repo")
	}
	// Seed a 200-commit repo (the clone workload's repo) via the store's own
	// commit builder, plus one issue for the read workload.
	oneK := bytes.Repeat([]byte("x"), 1024)
	for i := 0; i < 200; i++ {
		s.CreateCommitOnBranch(repo, "main", "file.txt", oneK, fmt.Sprintf("commit %d", i))
	}
	issue := s.CreateIssue(repo, "bench issue", "body", nil)
	tip := repo.Refs["refs/heads/main"]
	base := ""
	srv := httptest.NewServer(forge.Handler())
	defer srv.Close()
	base = srv.URL
	c := profileClient(t)
	ownerRepo := owner + "/bench"

	// Warm up: one of each op (fills caches, JIT-free warmup).
	{
		warmBody, _ := buildPushBodyChain(repo.Refs["refs/heads/main"], repo.CommitCounter+1, 0)
		profileDo(t, c, profileReq(t, "POST", base+"/"+ownerRepo+".git/git-receive-pack", warmBody, nil), 200)
		profileDo(t, c, profileReq(t, "GET", base+"/api/v3/repos/"+ownerRepo+"/issues/1", nil, nil), 200)
	}

	// Push workload bodies: all 3 rounds pre-built as ONE 600-commit chain
	// BEFORE the CPU profile starts (inputs are built before the clock
	// starts in the real bench too) — otherwise client-side pack
	// construction pollutes the server profile.
	pushBodies := make([][]byte, 600)
	{
		tip := repo.Refs["refs/heads/main"]
		counter := repo.CommitCounter
		for i := 0; i < 600; i++ {
			counter++
			body, newTip := buildPushBodyChain(tip, counter, 1000+i)
			pushBodies[i] = body
			tip = newTip
		}
	}

	// CPU profile across all four workloads, 3 rounds each.
	cpuFile, err := os.Create(filepath.Join(dir, "cpu.pprof"))
	if err != nil {
		t.Fatal(err)
	}
	if err := pprof.StartCPUProfile(cpuFile); err != nil {
		t.Fatal(err)
	}
	runtime.SetBlockProfileRate(1)
	runtime.SetMutexProfileFraction(1)
	type wl struct {
		name string
		ops  int
		run  func()
	}
	// Read workload (2000 sequential GETs of one issue).
	readRun := func() {
		for i := 0; i < 2000; i++ {
			profileDo(t, c, profileReq(t, "GET", base+"/api/v3/repos/"+ownerRepo+"/issues/"+fmt.Sprint(issue.Number), nil, nil), 200)
		}
	}
	// Write workload (200 issue creations).
	writeN := 0
	writeRun := func() {
		for i := 0; i < 200; i++ {
			writeN++
			b := []byte(fmt.Sprintf(`{"title":"w%d","body":"b"}`, writeN))
			profileDo(t, c, profileReq(t, "POST", base+"/api/v3/repos/"+ownerRepo+"/issues", b, nil), 201)
		}
	}
	// Push workload (200 sequential receive-packs per round).
	pushRound := 0
	pushRun := func() {
		bodies := pushBodies[pushRound*200 : (pushRound+1)*200]
		pushRound++
		for i := 0; i < 200; i++ {
			b := profileDo(t, c, profileReq(t, "POST", base+"/"+ownerRepo+".git/git-receive-pack", bodies[i], nil), 200)
			if !strings.Contains(string(b), "unpack ok") {
				t.Fatalf("push %d: no unpack ok: %.200s", i, b)
			}
		}
	}
	// Clone workload: 50 concurrent shallow clones (info/refs + ls-refs + fetch deepen 1).
	cloneRun := func() {
		var wg sync.WaitGroup
		errs := make(chan string, 50)
		for k := 0; k < 50; k++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// info/refs v2
				profileDo(t, c, profileReq(t, "GET", base+"/"+ownerRepo+".git/info/refs?service=git-upload-pack", nil, map[string]string{"Git-Protocol": "version=2"}), 200)
				// ls-refs
				var ls bytes.Buffer
				ls.Write(store.PktLineString("command=ls-refs\n"))
				ls.Write(store.PktLineString("agent=git/2.43.0\n"))
				ls.Write(store.PktDelim())
				ls.Write(store.PktLineString("peel\n"))
				ls.Write(store.PktLineString("unborn\n"))
				ls.Write(store.PktFlush())
				profileDo(t, c, profileReq(t, "POST", base+"/"+ownerRepo+".git/git-upload-pack", ls.Bytes(), nil), 200)
				// fetch deepen 1
				var fe bytes.Buffer
				fe.Write(store.PktLineString("command=fetch\n"))
				fe.Write(store.PktLineString("agent=git/2.43.0\n"))
				fe.Write(store.PktDelim())
				fe.Write(store.PktLineString("want " + repo.Refs["refs/heads/main"] + "\n"))
				fe.Write(store.PktLineString("deepen 1\n"))
				fe.Write(store.PktLineString("done\n"))
				fe.Write(store.PktFlush())
				b := profileDo(t, c, profileReq(t, "POST", base+"/"+ownerRepo+".git/git-upload-pack", fe.Bytes(), nil), 200)
				if len(b) < 100 {
					errs <- fmt.Sprintf("fetch body too short: %d", len(b))
				}
			}()
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			t.Fatal(e)
		}
	}
	_ = tip

	workloads := []wl{
		{"push", 200, pushRun},
		{"clone", 50, cloneRun},
		{"read", 2000, readRun},
		{"write", 200, writeRun},
	}
	results := map[string][]time.Duration{}
	for _, w := range workloads {
		for round := 0; round < 3; round++ {
			before := snapMem()
			start := time.Now()
			w.run()
			elapsed := time.Since(start)
			after := snapMem().sub(before)
			results[w.name] = append(results[w.name], elapsed)
			t.Logf("%s round %d: %s total, %.1f us/op, allocs/op %.1f, bytes/op %.0f, GCs %d, GC pause total %.1f ms",
				w.name, round, elapsed, float64(elapsed.Microseconds())/float64(w.ops),
				float64(after.mallocs)/float64(w.ops), float64(after.totalAlloc)/float64(w.ops),
				after.numGC, float64(after.pauseTotal)/1e6)
		}
	}
	pprof.StopCPUProfile()
	cpuFile.Close()
	runtime.SetBlockProfileRate(0)
	runtime.SetMutexProfileFraction(0)

	// Block + mutex profiles (lock contention under the 50-way clone).
	for _, p := range []struct {
		name string
		prof string
	}{
		{"block.pprof", "block"},
		{"mutex.pprof", "mutex"},
	} {
		f, err := os.Create(filepath.Join(dir, p.name))
		if err != nil {
			t.Fatal(err)
		}
		if pr := pprof.Lookup(p.prof); pr != nil {
			_ = pr.WriteTo(f, 0)
		}
		f.Close()
	}

	// Heap (alloc) profile.
	heapFile, err := os.Create(filepath.Join(dir, "heap.pprof"))
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	if err := pprof.WriteHeapProfile(heapFile); err != nil {
		t.Fatal(err)
	}
	heapFile.Close()

	// Direct handler timing (no network): read workload via Recorder, to
	// split net/http + loopback cost from handler CPU.
	rec := httptest.NewRecorder()
	directReq := func() *http.Request {
		req := httptest.NewRequest("GET", "/api/v3/repos/"+ownerRepo+"/issues/1", nil)
		req.Header.Set("Authorization", "Bearer "+profileToken)
		req.RequestURI = "/api/v3/repos/" + ownerRepo + "/issues/1"
		return req
	}
	// Warm the direct path once.
	forge.Handler().ServeHTTP(rec, directReq())
	start := time.Now()
	for i := 0; i < 2000; i++ {
		rec = httptest.NewRecorder()
		forge.Handler().ServeHTTP(rec, directReq())
		if rec.Code != 200 {
			t.Fatalf("direct read: status %d", rec.Code)
		}
	}
	directRead := time.Since(start)
	t.Logf("read direct-handler (no network): %s total, %.1f us/op", directRead, float64(directRead.Microseconds())/2000)

	// Direct clone fetch (no network, single goroutine): cache-hit path.
	fetchBody := func() []byte {
		var fe bytes.Buffer
		fe.Write(store.PktLineString("command=fetch\n"))
		fe.Write(store.PktDelim())
		fe.Write(store.PktLineString("want " + repo.Refs["refs/heads/main"] + "\n"))
		fe.Write(store.PktLineString("deepen 1\n"))
		fe.Write(store.PktLineString("done\n"))
		fe.Write(store.PktFlush())
		return fe.Bytes()
	}
	fb := fetchBody()
	start = time.Now()
	for i := 0; i < 50; i++ {
		rec = httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/"+ownerRepo+".git/git-upload-pack", bytes.NewReader(fb))
		req.Header.Set("Authorization", "Bearer "+profileToken)
		req.RequestURI = "/" + ownerRepo + ".git/git-upload-pack"
		forge.Handler().ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("direct fetch: status %d", rec.Code)
		}
	}
	directFetch := time.Since(start)
	t.Logf("clone fetch direct-handler x50 sequential (cache-hit path): %s total, %.1f us/op", directFetch, float64(directFetch.Microseconds())/50)

	// Direct push (no network): 200 receive-packs via Recorder.
	{
		tip := repo.Refs["refs/heads/main"]
		counter := repo.CommitCounter
		directBodies := make([][]byte, 200)
		for i := 0; i < 200; i++ {
			counter++
			body, newTip := buildPushBodyChain(tip, counter, 5000+i)
			directBodies[i] = body
			tip = newTip
		}
		start := time.Now()
		for i := 0; i < 200; i++ {
			rec = httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/"+ownerRepo+".git/git-receive-pack", bytes.NewReader(directBodies[i]))
			req.Header.Set("Authorization", "Bearer "+profileToken)
			req.RequestURI = "/" + ownerRepo + ".git/git-receive-pack"
			forge.Handler().ServeHTTP(rec, req)
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), "unpack ok") {
				t.Fatalf("direct push %d: status %d body %.100s", i, rec.Code, rec.Body.String())
			}
		}
		directPush := time.Since(start)
		t.Logf("push direct-handler (no network): %s total, %.1f us/op", directPush, float64(directPush.Microseconds())/200)
	}

	// Direct write (no network): 200 issue creations via Recorder.
	{
		start := time.Now()
		for i := 0; i < 200; i++ {
			rec = httptest.NewRecorder()
			b := []byte(fmt.Sprintf(`{"title":"d%d","body":"b"}`, i))
			req := httptest.NewRequest("POST", "/api/v3/repos/"+ownerRepo+"/issues", bytes.NewReader(b))
			req.Header.Set("Authorization", "Bearer "+profileToken)
			req.RequestURI = "/api/v3/repos/" + ownerRepo + "/issues"
			forge.Handler().ServeHTTP(rec, req)
			if rec.Code != 201 {
				t.Fatalf("direct write %d: status %d", i, rec.Code)
			}
		}
		directWrite := time.Since(start)
		t.Logf("write direct-handler (no network): %s total, %.1f us/op", directWrite, float64(directWrite.Microseconds())/200)
	}

	for name, ds := range results {
		t.Logf("%s rounds: %v", name, ds)
	}
}
