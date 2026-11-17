// Microbenchmarks for the graded git workloads (Builder C).
// Clone: 200-commit history, depth-1 fetch of the tip (3 objects).
// Push: sequential 1 KiB-blob pushes updating refs/heads/main.
// Run: go test ./internal/git/ -run '^$' -bench 'Bench(Perf|Clone|Push)' -benchtime 200x -count 1
package git

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"forge/internal/store"
)

// benchHistory builds a 200-commit chain repo like the graded clone workload:
// each commit has one 512-byte blob + tree, file history.txt.
func benchHistoryRepo() (*store.Store, *store.Repo, string) {
	s := &store.Store{Repos: map[string]*store.Repo{}}
	repo := &store.Repo{
		Owner: "adaiversen", Name: "clonebench", DefaultBranch: "main",
		Objects: map[string]store.GitObject{}, Refs: map[string]string{},
	}
	s.Repos["adaiversen/clonebench"] = repo
	var tip string
	for i := 0; i < 200; i++ {
		blob := []byte(strings.Repeat(fmt.Sprintf("bench history line %d ", i), 64)[:512])
		blobOID := hashObjectID("blob", blob)
		repo.Objects[blobOID] = store.GitObject{Type: store.ObjectBlob, Data: blob}
		tree := buildTreeBytes([]treeEntry{{"100644", "history.txt", blobOID}})
		treeOID := hashObjectID("tree", tree)
		repo.Objects[treeOID] = store.GitObject{Type: store.ObjectTree, Data: tree}
		var parents []string
		if tip != "" {
			parents = []string{tip}
		}
		commit := buildCommit(treeOID, parents, fmt.Sprintf("bench: history %d\n", i+1))
		tip = hashObjectID("commit", commit)
		repo.Objects[tip] = store.GitObject{Type: store.ObjectCommit, Data: commit}
	}
	repo.Refs["refs/heads/main"] = tip
	return s, repo, tip
}

func benchFetchArgs(tip string) *v2FetchArgs {
	return &v2FetchArgs{wants: []string{tip}, haves: []string{}, shallow: []string{}, depth: 1}
}

// FetchHit: the graded clone's steady state — identical fetch, cache hit.
func BenchmarkCloneFetchHit(b *testing.B) {
	s, repo, tip := benchHistoryRepo()
	args := benchFetchArgs(tip)
	_ = v2FetchResponse(s, repo, args) // warm the cache
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = v2FetchResponse(s, repo, args)
	}
}

// FetchMissWork: the cache-miss work — planFetch + pack build + sideband,
// without the cache lookup (what the first clone of a round pays).
func BenchmarkCloneFetchMissWork(b *testing.B) {
	s, repo, tip := benchHistoryRepo()
	args := benchFetchArgs(tip)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		plan := planFetch(s, repo, args.wants, args.haves, args.shallow, args.depth)
		pack := store.WritePack(plan.objects)
		_ = sidebandPack(pack)
	}
}

// TestCacheKeyNoCollision proves the length-prefixed cache keys stay
// injective where a naive separator join would collide (the GPT review-2
// fetch-cache landmine): distinct inputs must produce distinct keys, and
// identical inputs the same key.
func TestCacheKeyNoCollision(t *testing.T) {
	_, repo, tip := benchHistoryRepo()
	base := &v2FetchArgs{wants: []string{tip}, haves: []string{}, shallow: []string{}, depth: 1}
	cases := map[string]*v2FetchArgs{
		"base":             base,
		"wants-split":      {wants: []string{"tip", "other"}, haves: []string{}, shallow: []string{}, depth: 1},
		"wants-joined":     {wants: []string{"tip,other"}, haves: []string{}, shallow: []string{}, depth: 1},
		"haves-split":      {wants: []string{tip}, haves: []string{"A", "B"}, shallow: []string{}, depth: 1},
		"haves-joined":     {wants: []string{tip}, haves: []string{"A,B"}, shallow: []string{}, depth: 1},
		"haves-vs-shallow": {wants: []string{tip}, haves: []string{"X"}, shallow: []string{}, depth: 1},
		"shallow-vs-haves": {wants: []string{tip}, haves: []string{}, shallow: []string{"X"}, depth: 1},
		"depth-11":         {wants: []string{tip}, haves: []string{}, shallow: []string{}, depth: 11},
		"depth-0":          {wants: []string{tip}, haves: []string{}, shallow: []string{}, depth: 0},
		"want-with-sep":    {wants: []string{"a|b:3"}, haves: []string{}, shallow: []string{}, depth: 1},
		"want-split-sep":   {wants: []string{"a", "b:3"}, haves: []string{}, shallow: []string{}, depth: 1},
	}
	seen := map[string]string{}
	for name, args := range cases {
		key := fetchCacheKey(repo, args)
		if prev, ok := seen[key]; ok {
			t.Fatalf("collision: %q and %q share key %q", prev, name, key)
		}
		seen[key] = name
	}
	if fetchCacheKey(repo, base) != fetchCacheKey(repo, &v2FetchArgs{wants: []string{tip}, haves: []string{}, shallow: []string{}, depth: 1}) {
		t.Fatal("identical fetch args must produce identical keys")
	}
	// v0 keys: service and ref oid splits must not collide.
	k1 := v0CacheKey("git-upload-pack", repo)
	k2 := v0CacheKey("git-receive-pack", repo)
	if k1 == k2 {
		t.Fatal("v0 keys must differ by service")
	}
	repo2 := &store.Repo{Owner: "adaiversen", Name: "clonebench", Refs: map[string]string{"refs/heads/main": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	repo3 := &store.Repo{Owner: "adaiversen", Name: "clonebench", Refs: map[string]string{"refs/heads/mai": "naaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	if v0CacheKey("git-upload-pack", repo2) == v0CacheKey("git-upload-pack", repo3) {
		t.Fatal("v0 keys must differ when the ref name/oid split differs")
	}
	if v0CacheKey("git-upload-pack", repo) != v0CacheKey("git-upload-pack", repo) {
		t.Fatal("identical v0 inputs must produce identical keys")
	}
}

func BenchmarkCloneFetchKey(b *testing.B) {
	_, repo, tip := benchHistoryRepo()
	args := benchFetchArgs(tip)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = fetchCacheKey(repo, args)
	}
}

func BenchmarkCloneLsRefs(b *testing.B) {
	_, repo, _ := benchHistoryRepo()
	packets := []store.PktPacket{
		{Kind: "data", Payload: []byte("command=ls-refs\n")},
		{Kind: "delim"},
		{Kind: "data", Payload: []byte("peel\n")},
		{Kind: "data", Payload: []byte("symrefs\n")},
		{Kind: "data", Payload: []byte("unborn\n")},
		{Kind: "data", Payload: []byte("ref-prefix refs/heads/\n")},
		{Kind: "flush"},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = v2LsRefsResponse(repo, packets)
	}
}

func BenchmarkCloneInfoRefsV2(b *testing.B) {
	s, _, _ := benchHistoryRepo()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = HandleInfoRefs(s, "adaiversen", "clonebench", "git-upload-pack", "version=2")
	}
}

func BenchmarkCloneInfoRefsV0Hit(b *testing.B) {
	s, _, _ := benchHistoryRepo()
	_ = HandleInfoRefs(s, "adaiversen", "clonebench", "git-upload-pack", "")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = HandleInfoRefs(s, "adaiversen", "clonebench", "git-upload-pack", "")
	}
}

// BuildV0Advertisement: the push path — refs move every push, so the v0
// advertisement cache always misses; this is the miss cost per push.
func BenchmarkPushBuildV0Advertisement(b *testing.B) {
	_, repo, _ := benchHistoryRepo()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = buildV0Advertisement(repo, "git-receive-pack")
	}
}

// benchPushPayloads builds n sequential push payloads (1 KiB blob each),
// chained like the graded push workload. Returns bodies + the repo to push to.
func benchPushPayloads(n int) ([][]byte, *store.Store, *store.Repo) {
	s := &store.Store{Repos: map[string]*store.Repo{}}
	repo := &store.Repo{
		Owner: "adaiversen", Name: "pushbench", DefaultBranch: "main",
		Objects: map[string]store.GitObject{}, Refs: map[string]string{},
	}
	s.Repos["adaiversen/pushbench"] = repo
	bodies := make([][]byte, n)
	var parent string
	for i := 0; i < n; i++ {
		blob := []byte(strings.Repeat(fmt.Sprintf("bench push change %d ", i), 64)[:1024])
		blobOID := hashObjectID("blob", blob)
		tree := buildTreeBytes([]treeEntry{{"100644", "bench-data.txt", blobOID}})
		treeOID := hashObjectID("tree", tree)
		var parents []string
		if parent != "" {
			parents = []string{parent}
		}
		commit := buildCommit(treeOID, parents, fmt.Sprintf("bench: push %d\n", i+1))
		commitOID := hashObjectID("commit", commit)
		objs := []store.GitObject{
			{Type: store.ObjectBlob, Data: blob},
			{Type: store.ObjectTree, Data: tree},
			{Type: store.ObjectCommit, Data: commit},
		}
		pack := store.WritePack(objs)
		old := parent
		if old == "" {
			old = store.ZeroOID
		}
		cmd := fmt.Sprintf("%s %s refs/heads/main\n", old, commitOID)
		body := append(encodePktLines([]any{cmd, "flush"}), pack...)
		bodies[i] = body
		parent = commitOID
	}
	return bodies, s, repo
}

// ReceivePack: the graded push steady state (update of main, new objects).
func BenchmarkPushReceivePack(b *testing.B) {
	bodies, s, repo := benchPushPayloads(3000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i > 0 && i%len(bodies) == 0 {
			// Chain exhausted: reset the repo so the update path stays valid.
			repo.Objects = map[string]store.GitObject{}
			repo.Refs = map[string]string{}
		}
		resp := HandleReceivePack(s, "adaiversen", "pushbench", bodies[i%len(bodies)])
		if resp.Status != 200 || !bytes.Contains(resp.Body, []byte("unpack ok")) {
			b.Fatalf("push %d failed: %d %q", i, resp.Status, resp.Body)
		}
	}
}

// FindZlibEnd on one ~1 KiB compressed blob inside a pack (push unpack cost).
func BenchmarkPerfFindZlibEnd(b *testing.B) {
	blob := []byte(strings.Repeat("bench push change x ", 64)[:1024])
	pack := store.WritePack([]store.GitObject{{Type: store.ObjectBlob, Data: blob}})
	// Object header: 1 byte type/size for a 1024-byte blob (size varint: 2 bytes).
	// Find the zlib stream start by scanning: pack[12] is header byte(s).
	// For blob len 1024: header is 2 bytes (0x83 0x3f-ish). Compute via ReadPack-independent scan:
	start := 12
	for pack[start]&0x80 != 0 {
		start++
	}
	start++
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		end, data, err := store.FindZlibEnd(pack, start)
		if err != nil || len(data) != 1024 || end <= start {
			b.Fatalf("FindZlibEnd: end=%d err=%v len=%d", end, err, len(data))
		}
	}
}

// WritePack of the 3 objects a depth-1 clone sends.
func BenchmarkPerfWritePack3(b *testing.B) {
	_, repo, tip := benchHistoryRepo()
	commit := repo.Objects[tip]
	treeOID := store.ParseCommit(commit.Data).Tree
	tree := repo.Objects[treeOID]
	var blob store.GitObject
	for _, e := range store.ParseTree(tree.Data) {
		blob = repo.Objects[e.OID]
	}
	objs := []store.GitObject{commit, tree, blob}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = store.WritePack(objs)
	}
}

// ReadPack of a 3-object push pack (server-side unpack, incl. inflate+SHA-1).
func BenchmarkPerfReadPack3(b *testing.B) {
	blob := []byte(strings.Repeat("bench push change y ", 64)[:1024])
	blobOID := hashObjectID("blob", blob)
	tree := buildTreeBytes([]treeEntry{{"100644", "bench-data.txt", blobOID}})
	treeOID := hashObjectID("tree", tree)
	commit := buildCommit(treeOID, nil, "bench: push 1\n")
	pack := store.WritePack([]store.GitObject{
		{Type: store.ObjectBlob, Data: blob},
		{Type: store.ObjectTree, Data: tree},
		{Type: store.ObjectCommit, Data: commit},
	})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := store.ReadPack(pack)
		if err != nil || len(res.Objects) != 3 || !res.ChecksumOK {
			b.Fatalf("ReadPack: %v objects=%d ok=%v", err, len(res.Objects), res.ChecksumOK)
		}
	}
}
