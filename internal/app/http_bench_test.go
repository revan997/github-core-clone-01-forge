// HTTP-level benchmarks (Builder C): full net/http stack + real loopback TCP,
// lean Go client, so the measured time is the SERVER's cost, not the Node
// kit client's JS cost. Setup mirrors the graded workloads:
//   - clone: 200-commit history repo, depth-1 fetch of the tip, 50 concurrent
//   - push: sequential 1 KiB pushes updating refs/heads/main
//
// Run: go test ./internal/app/ -run '^$' -bench 'BenchmarkHTTP(Clone|Push)' -benchtime 3x -count 1
package app

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"forge/internal/store"
)

const benchToken = "bench-token"

func benchHashObject(typ store.ObjectType, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", string(typ), len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func benchTreeBytes(entries [][3]string) []byte {
	var buf bytes.Buffer
	for _, e := range entries {
		fmt.Fprintf(&buf, "%s %s\x00", e[0], e[1])
		raw, _ := hex.DecodeString(e[2])
		buf.Write(raw)
	}
	return buf.Bytes()
}

func benchCommit(tree string, parents []string, msg string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "tree %s\n", tree)
	for _, p := range parents {
		fmt.Fprintf(&b, "parent %s\n", p)
	}
	fmt.Fprintf(&b, "author Conformance <conformance@make.fun> 1700000000 +0000\ncommitter Conformance <conformance@make.fun> 1700000000 +0000\n\n%s", msg)
	return []byte(b.String())
}

type benchServer struct {
	t      *testing.B
	base   string
	client *http.Client
	forge  *Forge
}

func startBenchServer(b *testing.B) *benchServer {
	b.Helper()
	f := NewForge()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	go http.Serve(ln, f.Handler())
	tr := &http.Transport{MaxConnsPerHost: 200, MaxIdleConnsPerHost: 200, DisableKeepAlives: false}
	client := &http.Client{Transport: tr, Timeout: 30 * time.Second}
	return &benchServer{t: b, base: "http://" + ln.Addr().String(), client: client, forge: f}
}

func (s *benchServer) do(method, path string, body []byte, headers map[string]string) (int, []byte) {
	s.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, s.base+path, rd)
	if err != nil {
		s.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+benchToken)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatal(err)
	}
	return resp.StatusCode, b
}

// setupCloneRepo builds the 200-commit history repo directly in the store
// (equivalent to the graded setup push; setup is untimed in grading).
func (s *benchServer) setupCloneRepo() string {
	repo := &store.Repo{
		Owner: "adaiversen", Name: "clonebench", DefaultBranch: "main",
		Objects: map[string]store.GitObject{}, Refs: map[string]string{},
	}
	s.forge.Store.Repos["adaiversen/clonebench"] = repo
	var tip string
	for i := 0; i < 200; i++ {
		blob := []byte(strings.Repeat(fmt.Sprintf("bench history line %d ", i), 64)[:512])
		blobOID := benchHashObject(store.ObjectBlob, blob)
		repo.Objects[blobOID] = store.GitObject{Type: store.ObjectBlob, Data: blob}
		tree := benchTreeBytes([][3]string{{"100644", "history.txt", blobOID}})
		treeOID := benchHashObject(store.ObjectTree, tree)
		repo.Objects[treeOID] = store.GitObject{Type: store.ObjectTree, Data: tree}
		var parents []string
		if tip != "" {
			parents = []string{tip}
		}
		commit := benchCommit(treeOID, parents, fmt.Sprintf("bench: history %d\n", i+1))
		tip = benchHashObject(store.ObjectCommit, commit)
		repo.Objects[tip] = store.GitObject{Type: store.ObjectCommit, Data: commit}
	}
	repo.Refs["refs/heads/main"] = tip
	return tip
}

func pktLine(s string) []byte {
	return []byte(fmt.Sprintf("%04x%s", len(s)+4, s))
}

// cloneOnce performs one depth-1 clone exactly as the kit client does:
// info/refs v2, ls-refs, fetch. Returns the fetch response size.
func (s *benchServer) cloneOnce(tip string) int {
	st, _ := s.do("GET", "/adaiversen/clonebench.git/info/refs?service=git-upload-pack", nil,
		map[string]string{"Git-Protocol": "version=2"})
	if st != 200 {
		s.t.Fatalf("info/refs: %d", st)
	}
	lsBody := bytes.Join([][]byte{
		pktLine("command=ls-refs\n"), []byte("0001"),
		pktLine("peel\n"), pktLine("symrefs\n"), pktLine("unborn\n"),
		pktLine("ref-prefix refs/heads/\n"), []byte("0000"),
	}, nil)
	st, _ = s.do("POST", "/adaiversen/clonebench.git/git-upload-pack", lsBody,
		map[string]string{"Content-Type": "application/x-git-upload-pack-request"})
	if st != 200 {
		s.t.Fatalf("ls-refs: %d", st)
	}
	fetchBody := bytes.Join([][]byte{
		pktLine("command=fetch\n"), []byte("0001"),
		pktLine("want " + tip + "\n"), pktLine("deepen 1\n"), pktLine("done\n"), []byte("0000"),
	}, nil)
	st, body := s.do("POST", "/adaiversen/clonebench.git/git-upload-pack", fetchBody,
		map[string]string{"Content-Type": "application/x-git-upload-pack-request"})
	if st != 200 {
		s.t.Fatalf("fetch: %d", st)
	}
	return len(body)
}

// BenchmarkHTTPCloneRound: one graded clone round — 50 concurrent clones.
func BenchmarkHTTPCloneRound(b *testing.B) {
	srv := startBenchServer(b)
	tip := srv.setupCloneRepo()
	// warmup: 5 clones (populates the fetch cache, as in grading)
	for i := 0; i < 5; i++ {
		srv.cloneOnce(tip)
	}
	b.ResetTimer()
	for round := 0; round < b.N; round++ {
		var wg sync.WaitGroup
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				srv.cloneOnce(tip)
			}()
		}
		wg.Wait()
	}
}

// BenchmarkHTTPCloneSingle: one sequential clone (per-clone latency, no contention).
func BenchmarkHTTPCloneSingle(b *testing.B) {
	srv := startBenchServer(b)
	tip := srv.setupCloneRepo()
	for i := 0; i < 5; i++ {
		srv.cloneOnce(tip)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		srv.cloneOnce(tip)
	}
}

// BenchmarkHTTPCloneFetchOnly: just the fetch POST, 50 concurrent (isolates the
// fetch-cache-hit request from info/refs + ls-refs).
func BenchmarkHTTPCloneFetchOnly(b *testing.B) {
	srv := startBenchServer(b)
	tip := srv.setupCloneRepo()
	fetchBody := bytes.Join([][]byte{
		pktLine("command=fetch\n"), []byte("0001"),
		pktLine("want " + tip + "\n"), pktLine("deepen 1\n"), pktLine("done\n"), []byte("0000"),
	}, nil)
	for i := 0; i < 5; i++ {
		srv.do("POST", "/adaiversen/clonebench.git/git-upload-pack", fetchBody,
			map[string]string{"Content-Type": "application/x-git-upload-pack-request"})
	}
	b.ResetTimer()
	for round := 0; round < b.N; round++ {
		var wg sync.WaitGroup
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				srv.do("POST", "/adaiversen/clonebench.git/git-upload-pack", fetchBody,
					map[string]string{"Content-Type": "application/x-git-upload-pack-request"})
			}()
		}
		wg.Wait()
	}
}

// setupPushRepo + push payloads for the HTTP push bench.
func (s *benchServer) pushPayloads(n int) [][]byte {
	repo := &store.Repo{
		Owner: "adaiversen", Name: "pushbench", DefaultBranch: "main",
		Objects: map[string]store.GitObject{}, Refs: map[string]string{},
	}
	s.forge.Store.Repos["adaiversen/pushbench"] = repo
	bodies := make([][]byte, n)
	var parent string
	for i := 0; i < n; i++ {
		blob := []byte(strings.Repeat(fmt.Sprintf("bench push change %d ", i), 64)[:1024])
		blobOID := benchHashObject(store.ObjectBlob, blob)
		tree := benchTreeBytes([][3]string{{"100644", "bench-data.txt", blobOID}})
		treeOID := benchHashObject(store.ObjectTree, tree)
		var parents []string
		if parent != "" {
			parents = []string{parent}
		}
		commit := benchCommit(treeOID, parents, fmt.Sprintf("bench: push %d\n", i+1))
		commitOID := benchHashObject(store.ObjectCommit, commit)
		pack := store.WritePack([]store.GitObject{
			{Type: store.ObjectBlob, Data: blob},
			{Type: store.ObjectTree, Data: tree},
			{Type: store.ObjectCommit, Data: commit},
		})
		old := parent
		if old == "" {
			old = store.ZeroOID
		}
		cmd := fmt.Sprintf("%s %s refs/heads/main\n", old, commitOID)
		body := append(bytes.Join([][]byte{pktLine(cmd), []byte("0000")}, nil), pack...)
		bodies[i] = body
		parent = commitOID
	}
	return bodies
}

// BenchmarkHTTPPushRound: one graded push round — 200 sequential pushes,
// each preceded by its info/refs (as the kit client does).
func BenchmarkHTTPPushRound(b *testing.B) {
	srv := startBenchServer(b)
	bodies := srv.pushPayloads(3000)
	idx := 0
	pushOnce := func() {
		st, _ := srv.do("GET", "/adaiversen/pushbench.git/info/refs?service=git-receive-pack", nil, nil)
		if st != 200 {
			b.Fatalf("push info/refs: %d", st)
		}
		st, resp := srv.do("POST", "/adaiversen/pushbench.git/git-receive-pack", bodies[idx],
			map[string]string{"Content-Type": "application/x-git-receive-pack-request"})
		if st != 200 || !bytes.Contains(resp, []byte("unpack ok")) {
			b.Fatalf("push %d: %d %q", idx, st, resp)
		}
		idx++
	}
	for i := 0; i < 5; i++ {
		pushOnce()
	}
	b.ResetTimer()
	for round := 0; round < b.N; round++ {
		for i := 0; i < 200; i++ {
			pushOnce()
		}
	}
}
