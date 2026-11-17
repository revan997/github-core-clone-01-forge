package store

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// object ids / hashing
// ---------------------------------------------------------------------------

func TestIsObjectID(t *testing.T) {
	if !IsObjectID("ce013625030ba8dba906f756967f9e9ca394464a") {
		t.Fatal("valid oid rejected")
	}
	if !IsObjectID(ZeroOID) {
		t.Fatal("zero oid rejected")
	}
	for _, bad := range []string{"", "ce013625030ba8dba906f756967f9e9ca394464", "ce013625030ba8dba906f756967f9e9ca394464aa",
		"CE013625030ba8dba906f756967f9e9ca394464a", "ge013625030ba8dba906f756967f9e9ca394464a"} {
		if IsObjectID(bad) {
			t.Fatalf("invalid oid accepted: %q", bad)
		}
	}
}

func TestHashObjectMatchesGit(t *testing.T) {
	// Values verified against `git hash-object` (see report).
	cases := []struct {
		typ  ObjectType
		data string
		want string
	}{
		{ObjectBlob, "", "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"},
		{ObjectBlob, "hello\n", "ce013625030ba8dba906f756967f9e9ca394464a"},
		{ObjectBlob, "hello world\n", "3b18e512dba79e4c8300dd08aeb37f8e728b8dad"},
		// A commit with no trailing newline after the message, exactly as
		// the Node store builds it; verified with git hash-object -t commit.
		{ObjectCommit, "tree 853694aae8816094a0d875fee7ea26278dbf5d0f\nauthor ada iversen <adaiversen@example.invalid> 1700000001 +0000\ncommitter ada iversen <adaiversen@example.invalid> 1700000001 +0000\n\ninit",
			"b00e14b5bd364281c3ce6f3101221a95c6c0eecd"},
	}
	for _, c := range cases {
		if got := HashObject(c.typ, []byte(c.data)); got != c.want {
			t.Fatalf("HashObject(%s, %q) = %s, want %s", c.typ, c.data, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// deterministic clock / request ids
// ---------------------------------------------------------------------------

func TestSeqTimestampSequence(t *testing.T) {
	s := NewStore()
	want := []string{
		"2025-01-01T00:00:01.000Z",
		"2025-01-01T00:00:02.000Z",
		"2025-01-01T00:00:03.000Z",
	}
	for i, w := range want {
		if got := s.SeqTimestamp(); got != w {
			t.Fatalf("SeqTimestamp #%d = %q, want %q", i+1, got, w)
		}
	}
}

func TestNextRequestID(t *testing.T) {
	s := NewStore()
	if got := s.NextRequestID(); got != "req-00000001" {
		t.Fatalf("first request id = %q", got)
	}
	if got := s.NextRequestID(); got != "req-00000002" {
		t.Fatalf("second request id = %q", got)
	}
}

// ---------------------------------------------------------------------------
// pkt-lines
// ---------------------------------------------------------------------------

func TestPktLineRoundTrip(t *testing.T) {
	if got := string(PktFlush()); got != "0000" {
		t.Fatalf("PktFlush = %q", got)
	}
	if got := string(PktDelim()); got != "0001" {
		t.Fatalf("PktDelim = %q", got)
	}
	if got := string(PktLineString("hello")); got != "0009hello" {
		t.Fatalf("PktLineString(hello) = %q", got)
	}
	if got := string(PktLine([]byte{})); got != "0004" {
		t.Fatalf("PktLine(empty) = %q", got)
	}
	body := append(append(append([]byte{}, PktLineString("foo")...), PktFlush()...), PktLineString("barbaz")...)
	body = append(body, []byte("PACKREST")...)
	packets, offset := DecodePktLines(body)
	if len(packets) != 3 {
		t.Fatalf("got %d packets, want 3", len(packets))
	}
	if packets[0].Kind != "data" || string(packets[0].Payload) != "foo" {
		t.Fatalf("packet 0 = %+v", packets[0])
	}
	if packets[1].Kind != "flush" {
		t.Fatalf("packet 1 kind = %q", packets[1].Kind)
	}
	if packets[2].Kind != "data" || string(packets[2].Payload) != "barbaz" {
		t.Fatalf("packet 2 = %+v", packets[2])
	}
	if offset != len(body)-len("PACKREST") {
		t.Fatalf("offset = %d, want %d (start of pack)", offset, len(body)-len("PACKREST"))
	}
	// delim + response-end markers decode.
	body2 := append(append([]byte{}, PktDelim()...), []byte("0002")...)
	packets2, _ := DecodePktLines(body2)
	if len(packets2) != 2 || packets2[0].Kind != "delim" || packets2[1].Kind != "response-end" {
		t.Fatalf("marker packets = %+v", packets2)
	}
}

// ---------------------------------------------------------------------------
// zlib stream end
// ---------------------------------------------------------------------------

func deflate(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestFindZlibEnd(t *testing.T) {
	payload := []byte(strings.Repeat("hello world, zlib stream end. ", 200))
	stream := deflate(t, payload)
	padded := append(append([]byte("PREFIX"), stream...), []byte("TRAILING")...)
	end, data, err := FindZlibEnd(padded, 6)
	if err != nil {
		t.Fatal(err)
	}
	if end != 6+len(stream) {
		t.Fatalf("end = %d, want %d", end, 6+len(stream))
	}
	if !bytes.Equal(data, payload) {
		t.Fatal("inflated data mismatch")
	}
	// Empty payload.
	empty := deflate(t, []byte{})
	end, data, err = FindZlibEnd(empty, 0)
	if err != nil {
		t.Fatal(err)
	}
	if end != len(empty) || len(data) != 0 {
		t.Fatalf("empty stream: end=%d len=%d", end, len(data))
	}
	// Corrupt / truncated streams must error, never guess.
	if _, _, err := FindZlibEnd([]byte{0x78, 0x9c, 0x01, 0x02}, 0); err == nil {
		t.Fatal("corrupt stream did not error")
	}
	trunc := stream[:len(stream)-2]
	if _, _, err := FindZlibEnd(trunc, 0); err == nil {
		t.Fatal("truncated stream did not error")
	}
	if _, _, err := FindZlibEnd(stream, len(stream)+1); err == nil {
		t.Fatal("out-of-range start did not error")
	}
}

// ---------------------------------------------------------------------------
// commit / tree / tag parsing
// ---------------------------------------------------------------------------

func TestParseCommit(t *testing.T) {
	text := "tree 853694aae8816094a0d875fee7ea26278dbf5d0f\n" +
		"parent aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n" +
		"parent bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n" +
		"author ada iversen <adaiversen@example.invalid> 1700000001 +0000\n" +
		"committer ada iversen <adaiversen@example.invalid> 1700000001 +0000\n\n" +
		"subject line\n\nbody text"
	c := ParseCommit([]byte(text))
	if c.Tree != "853694aae8816094a0d875fee7ea26278dbf5d0f" {
		t.Fatalf("tree = %q", c.Tree)
	}
	if len(c.Parents) != 2 || c.Parents[0] != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("parents = %v", c.Parents)
	}
	if c.Author != "ada iversen <adaiversen@example.invalid> 1700000001 +0000" {
		t.Fatalf("author = %q", c.Author)
	}
	if c.Message != "subject line\n\nbody text" {
		t.Fatalf("message = %q", c.Message)
	}
	// No blank line: whole text is the header, message is empty.
	c2 := ParseCommit([]byte("tree abc"))
	if c2.Tree != "abc" || c2.Message != "" {
		t.Fatalf("no-blank-line parse = %+v", c2)
	}
	// Empty message after the blank line.
	c3 := ParseCommit([]byte("tree abc\n\n"))
	if c3.Message != "" {
		t.Fatalf("empty message = %q", c3.Message)
	}
}

func TestParseTag(t *testing.T) {
	tag := ParseTag([]byte("object 0123456789abcdef0123456789abcdef01234567\ntype commit\ntag v1.0\ntagger x <x@y> 1 +0000\n\nmsg"))
	if tag.Object != "0123456789abcdef0123456789abcdef01234567" || tag.Type != "commit" || tag.Tag != "v1.0" {
		t.Fatalf("tag = %+v", tag)
	}
}

func TestBuildTreeBytesSortsDirectoriesAsNameSlash(t *testing.T) {
	// "a.txt" (file) vs "a" (directory): dir key "a/" sorts after "a.txt"
	// because '.' (0x2e) < '/' (0x2f).
	entries := []TreeEntry{
		{Mode: "40000", Name: "a", OID: "1111111111111111111111111111111111111111"},
		{Mode: "100644", Name: "a.txt", OID: "2222222222222222222222222222222222222222"},
		{Mode: "100644", Name: "b.txt", OID: "3333333333333333333333333333333333333333"},
	}
	got := BuildTreeBytes(entries)
	parsed := ParseTree(got)
	wantOrder := []string{"a.txt", "a", "b.txt"}
	if len(parsed) != 3 {
		t.Fatalf("parsed %d entries", len(parsed))
	}
	for i, w := range wantOrder {
		if parsed[i].Name != w {
			t.Fatalf("order[%d] = %q, want %q (full: %v)", i, parsed[i].Name, w, parsed)
		}
	}
	// Round-trip: oid bytes survive.
	if parsed[0].OID != "2222222222222222222222222222222222222222" {
		t.Fatalf("oid round-trip = %q", parsed[0].OID)
	}
}

// ---------------------------------------------------------------------------
// global ids
// ---------------------------------------------------------------------------

func TestGlobalID(t *testing.T) {
	if got := GlobalID("Issue", 1, 1); got != "SXNzdWU6MTox" {
		t.Fatalf("GlobalID(Issue,1,1) = %q", got)
	}
	if got := GlobalID("PullRequest", 1, 2); got != "UHVsbFJlcXVlc3Q6MToy" {
		t.Fatalf("GlobalID(PullRequest,1,2) = %q", got)
	}
	kind, parts, ok := ParseGlobalID("SXNzdWU6MTox")
	if !ok || kind != "Issue" || len(parts) != 2 || parts[0] != "1" || parts[1] != "1" {
		t.Fatalf("ParseGlobalID = %q %v %v", kind, parts, ok)
	}
	if _, _, ok := ParseGlobalID("!!!not-base64!!!"); ok {
		t.Fatal("garbage global id accepted")
	}
	if _, _, ok := ParseGlobalID(GlobalID("IssueComment", 1, 1, 1)); !ok {
		t.Fatal("round-trip global id rejected")
	}
	// No padding: base64url output never contains '='.
	if strings.Contains(GlobalID("Issue", 1, 1), "=") {
		t.Fatal("global id contains padding")
	}
}

// ---------------------------------------------------------------------------
// packs: write -> read round-trip, hand-built deltas, real git pack
// ---------------------------------------------------------------------------

func TestWriteReadPackRoundTrip(t *testing.T) {
	blob := GitObject{Type: ObjectBlob, Data: []byte("hello\n")}
	empty := GitObject{Type: ObjectBlob, Data: []byte{}}
	treeBytes := BuildTreeBytes([]TreeEntry{{Mode: "100644", Name: "README.md", OID: HashObject(ObjectBlob, []byte("hello\n"))}})
	tree := GitObject{Type: ObjectTree, Data: treeBytes}
	commitText := "tree " + HashObject(ObjectTree, treeBytes) + "\nauthor ada iversen <adaiversen@example.invalid> 1700000001 +0000\ncommitter ada iversen <adaiversen@example.invalid> 1700000001 +0000\n\ninit"
	commit := GitObject{Type: ObjectCommit, Data: []byte(commitText)}
	pack := WritePack([]GitObject{commit, blob, tree, empty})
	if string(pack[0:4]) != "PACK" {
		t.Fatal("pack magic missing")
	}
	if pack[7] != 2 {
		t.Fatalf("pack version = %d, want 2", pack[7])
	}
	// Trailer is the SHA-1 of everything before it.
	sum := sha1.Sum(pack[:len(pack)-20])
	if hex.EncodeToString(sum[:]) != hex.EncodeToString(pack[len(pack)-20:]) {
		t.Fatal("pack trailer checksum mismatch")
	}
	res, err := ReadPack(pack)
	if err != nil {
		t.Fatal(err)
	}
	if !res.ChecksumOK {
		t.Fatal("checksum not OK on our own pack")
	}
	if len(res.Objects) != 4 {
		t.Fatalf("read %d objects, want 4", len(res.Objects))
	}
	for _, obj := range []GitObject{commit, blob, tree, empty} {
		oid := HashObject(obj.Type, obj.Data)
		got, ok := res.Objects[oid]
		if !ok {
			t.Fatalf("object %s missing after round-trip", oid)
		}
		if got.Type != obj.Type || !bytes.Equal(got.Data, obj.Data) {
			t.Fatalf("object %s mismatch: %+v", oid, got)
		}
	}
	// Malformed packs error; a pack with a damaged trailer parses but
	// must report a bad checksum.
	if _, err := ReadPack([]byte("NOPE")); err == nil {
		t.Fatal("non-pack accepted")
	}
	res2, err := ReadPack(append([]byte{}, pack[:len(pack)-5]...))
	if err != nil {
		t.Fatalf("truncated-trailer pack errored: %v", err)
	}
	if res2.ChecksumOK {
		t.Fatal("truncated pack reported checksum OK")
	}
}

// packObjectHeader encodes a pack object header (type + size varint).
func packObjectHeader(typeNum, size int) []byte {
	out := []byte{byte((typeNum << 4) | (size & 0x0f))}
	remaining := size >> 4
	for remaining > 0 {
		out[len(out)-1] |= 0x80
		out = append(out, byte(remaining&0x7f))
		remaining >>= 7
	}
	return out
}

// encodeOfsOffset encodes a negative OFS-delta offset per git's scheme.
func encodeOfsOffset(offset int) []byte {
	buf := []byte{byte(offset & 0x7f)}
	offset >>= 7
	for offset > 0 {
		offset--
		buf = append([]byte{byte(0x80 | (offset & 0x7f))}, buf...)
		offset >>= 7
	}
	return buf
}

// buildDeltaPack builds a pack with one base blob plus one delta object
// (type 6 OFS or type 7 REF) producing target from base.
func buildDeltaPack(t *testing.T, useOfs bool) []byte {
	t.Helper()
	base := []byte("hello world\n")
	// delta: base size 12, result size 22, copy 6 @0, insert "brave new ",
	// copy 6 @6. Result: "hello brave new world\n".
	delta := []byte{0x0c, 0x16, 0x90, 0x06, 0x0a}
	delta = append(delta, []byte("brave new ")...)
	delta = append(delta, 0x91, 0x06, 0x06)
	var pack bytes.Buffer
	header := make([]byte, 12)
	copy(header[0:4], "PACK")
	header[7] = 2
	header[11] = 2
	pack.Write(header)
	baseStart := pack.Len()
	pack.Write(packObjectHeader(3, len(base)))
	pack.Write(deflate(t, base))
	deltaStart := pack.Len()
	if useOfs {
		pack.Write(packObjectHeader(6, len(delta)))
		pack.Write(encodeOfsOffset(deltaStart - baseStart))
	} else {
		pack.Write(packObjectHeader(7, len(delta)))
		raw, _ := hex.DecodeString(HashObject(ObjectBlob, base))
		pack.Write(raw)
	}
	pack.Write(deflate(t, delta))
	sum := sha1.Sum(pack.Bytes())
	pack.Write(sum[:])
	return pack.Bytes()
}

func TestReadPackDeltas(t *testing.T) {
	for _, useOfs := range []bool{false, true} {
		name := "REF"
		if useOfs {
			name = "OFS"
		}
		t.Run(name, func(t *testing.T) {
			res, err := ReadPack(buildDeltaPack(t, useOfs))
			if err != nil {
				t.Fatal(err)
			}
			if !res.ChecksumOK {
				t.Fatal("checksum not OK")
			}
			baseOID := HashObject(ObjectBlob, []byte("hello world\n"))
			targetOID := HashObject(ObjectBlob, []byte("hello brave new world\n"))
			if _, ok := res.Objects[baseOID]; !ok {
				t.Fatal("base blob missing")
			}
			got, ok := res.Objects[targetOID]
			if !ok {
				t.Fatalf("delta result %s missing (have %v)", targetOID, keys(res.Objects))
			}
			if got.Type != ObjectBlob || string(got.Data) != "hello brave new world\n" {
				t.Fatalf("delta result = %+v", got)
			}
		})
	}
}

func keys(m map[string]GitObject) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestReadPackGitDelta(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=ada iversen",
			"GIT_AUTHOR_EMAIL=adaiversen@example.invalid",
			"GIT_COMMITTER_NAME=ada iversen",
			"GIT_COMMITTER_EMAIL=adaiversen@example.invalid")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", ".")
	body := strings.Repeat("line of similar content for delta compression\n", 200)
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "v1")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte(body+"one more line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "v2")
	run("gc", "-q")
	packs, err := filepath.Glob(filepath.Join(dir, ".git", "objects", "pack", "*.pack"))
	if err != nil || len(packs) == 0 {
		t.Fatalf("no pack produced: %v %v", packs, err)
	}
	pack, err := os.ReadFile(packs[0])
	if err != nil {
		t.Fatal(err)
	}
	res, err := ReadPack(pack)
	if err != nil {
		t.Fatal(err)
	}
	if !res.ChecksumOK {
		t.Fatal("git pack checksum not OK")
	}
	if len(res.Objects) < 4 {
		t.Fatalf("git pack yielded %d objects, want >= 4", len(res.Objects))
	}
	// Both blob versions must be present with exact content.
	for _, want := range []string{body, body + "one more line\n"} {
		oid := HashObject(ObjectBlob, []byte(want))
		got, ok := res.Objects[oid]
		if !ok {
			t.Fatalf("git blob %s missing", oid)
		}
		if string(got.Data) != want {
			t.Fatalf("git blob %s content mismatch", oid)
		}
	}
}

func TestCollectReachable(t *testing.T) {
	blob := GitObject{Type: ObjectBlob, Data: []byte("x")}
	tree := GitObject{Type: ObjectTree, Data: BuildTreeBytes([]TreeEntry{{Mode: "100644", Name: "f", OID: HashObject(ObjectBlob, []byte("x"))}})}
	commit := GitObject{Type: ObjectCommit, Data: []byte("tree " + HashObject(ObjectTree, tree.Data) + "\n\nc")}
	objects := map[string]GitObject{
		HashObject(ObjectBlob, []byte("x")):        blob,
		HashObject(ObjectTree, tree.Data):          tree,
		HashObject(ObjectCommit, commit.Data):      commit,
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef": {Type: ObjectBlob, Data: []byte("unreached")},
	}
	reached := CollectReachable([]string{HashObject(ObjectCommit, commit.Data)}, func(oid string) *GitObject {
		if o, ok := objects[oid]; ok {
			return &o
		}
		return nil
	})
	if len(reached) != 3 {
		t.Fatalf("reachable = %v, want 3 objects", keys(reachedObjects(reached)))
	}
	if reached["deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"] {
		t.Fatal("unreachable object collected")
	}
}

func reachedObjects(m map[string]bool) map[string]GitObject {
	out := map[string]GitObject{}
	for k := range m {
		out[k] = GitObject{}
	}
	return out
}

// ---------------------------------------------------------------------------
// store flows
// ---------------------------------------------------------------------------

func TestRepoCRUD(t *testing.T) {
	s := NewStore()
	if got := s.GetRepo("adaiversen", "demo"); got != nil {
		t.Fatal("GetRepo found a repo that does not exist")
	}
	r1 := s.CreateRepo("demo", false, "demo repo")
	if r1 == nil {
		t.Fatal("CreateRepo returned nil")
	}
	if r1.Owner != "adaiversen" || r1.Name != "demo" || r1.DefaultBranch != "main" || r1.DBID != 1 {
		t.Fatalf("repo = %+v", r1)
	}
	if len(r1.Labels) != len(DefaultLabels) {
		t.Fatalf("labels = %d, want %d", len(r1.Labels), len(DefaultLabels))
	}
	if dup := s.CreateRepo("demo", false, "again"); dup != nil {
		t.Fatal("duplicate CreateRepo did not return nil")
	}
	r2 := s.CreateRepo("other", true, "")
	if r2 == nil || r2.DBID != 2 {
		t.Fatalf("second repo dbid = %v", r2)
	}
	list := s.ListViewerRepos()
	if len(list) != 2 || list[0].DBID != 1 || list[1].DBID != 2 {
		t.Fatalf("ListViewerRepos = %v", list)
	}
	if !s.DeleteRepo("adaiversen", "other") {
		t.Fatal("DeleteRepo failed")
	}
	if s.DeleteRepo("adaiversen", "other") {
		t.Fatal("second DeleteRepo reported success")
	}
	if s.GetRepo("adaiversen", "other") != nil {
		t.Fatal("deleted repo still present")
	}
}

// The reference flow, run identically through the Node store
// (/tmp/node-flow.mjs) and here. Expected SHAs are the Node values.
func TestRepoIssueCommentPullMergeFlowMatchesNode(t *testing.T) {
	s := NewStore()
	repo := s.CreateRepo("demo", false, "demo repo")
	if repo == nil {
		t.Fatal("CreateRepo returned nil")
	}
	c1 := s.CreateCommitOnBranch(repo, "main", "README.md", []byte("hello\n"), "init")
	if c1 != "b00e14b5bd364281c3ce6f3101221a95c6c0eecd" {
		t.Fatalf("c1 = %s, want b00e14b5bd364281c3ce6f3101221a95c6c0eecd (Node)", c1)
	}
	repo.Refs["refs/heads/feature"] = c1
	c2 := s.CreateCommitOnBranch(repo, "feature", "src/app.ts", []byte("console.log(1);\n"), "add app")
	if c2 != "312a4f690e0409cef2a1bcc2fd40ad5fa712fb2a" {
		t.Fatalf("c2 = %s, want 312a4f690e0409cef2a1bcc2fd40ad5fa712fb2a (Node)", c2)
	}
	// Nested tree: feature tip flattens to both files.
	flat := s.FlattenTree(repo, ParseCommit(repo.Objects[c2].Data).Tree)
	if len(flat) != 2 || flat["README.md"] == "" || flat["src/app.ts"] == "" {
		t.Fatalf("flatten feature tree = %v", flat)
	}
	issue := s.CreateIssue(repo, "bug", "broken", []string{"bug", "nope"})
	if issue.Number != 1 {
		t.Fatalf("issue number = %d", issue.Number)
	}
	if len(issue.Labels) != 1 || issue.Labels[0].Name != "bug" {
		t.Fatalf("issue labels = %+v (unknown label 'nope' must be dropped)", issue.Labels)
	}
	if issue.GlobalID != "SXNzdWU6MTox" {
		t.Fatalf("issue global id = %q", issue.GlobalID)
	}
	// Labels are copies: mutating the issue label must not touch the repo label.
	issue.Labels[0].Color = "changed"
	if repo.Labels["bug"].Color != "d73a4a" {
		t.Fatal("issue label is not a copy of the repo label")
	}
	issue.CachedJSON = "cached"
	comment := s.AddComment(repo, issue, "looking")
	if comment.GlobalID != "SXNzdWVDb21tZW50OjE6MTox" || comment.DBID != 1 {
		t.Fatalf("comment = %+v", comment)
	}
	if issue.CachedJSON != "" {
		t.Fatal("AddComment did not clear CachedJSON")
	}
	if issue.UpdatedAt == issue.CreatedAt {
		t.Fatal("AddComment did not bump UpdatedAt")
	}
	pull, msg, ok := s.CreatePull(repo, "add app", "please", "feature", "main")
	if !ok {
		t.Fatalf("CreatePull failed: %s", msg)
	}
	if pull.Number != 2 || pull.GlobalID != "UHVsbFJlcXVlc3Q6MToy" {
		t.Fatalf("pull = %+v", pull)
	}
	if pull.HeadSHA != c2 || pull.BaseSHA != c1 {
		t.Fatalf("pull shas = %s / %s", pull.HeadSHA, pull.BaseSHA)
	}
	if !s.PullMergeable(repo, pull) {
		t.Fatal("pull should be mergeable (base is ancestor of head)")
	}
	if !s.IsAncestor(repo, c1, c2) || s.IsAncestor(repo, c2, c1) {
		t.Fatal("IsAncestor wrong")
	}
	files := s.PullFiles(repo, pull)
	if len(files) != 1 || files[0].Filename != "src/app.ts" || files[0].Status != "added" {
		t.Fatalf("PullFiles = %+v", files)
	}
	mergeSHA, msg, ok := s.MergePull(repo, pull)
	if !ok {
		t.Fatalf("MergePull failed: %s", msg)
	}
	if mergeSHA != "c37d7ff5d53f5103281cdd92a2550c3317e726d8" {
		t.Fatalf("merge sha = %s, want c37d7ff5d53f5103281cdd92a2550c3317e726d8 (Node)", mergeSHA)
	}
	merged := ParseCommit(repo.Objects[mergeSHA].Data)
	if merged.Tree != ParseCommit(repo.Objects[c2].Data).Tree {
		t.Fatal("merge commit tree is not the head tree")
	}
	if len(merged.Parents) != 2 || merged.Parents[0] != c1 || merged.Parents[1] != c2 {
		t.Fatalf("merge parents = %v", merged.Parents)
	}
	if merged.Message != "Merge pull request #2 from feature" {
		t.Fatalf("merge message = %q", merged.Message)
	}
	if repo.Refs["refs/heads/main"] != mergeSHA {
		t.Fatal("base ref not updated to merge commit")
	}
	if !pull.Merged || pull.State != "closed" || pull.MergeCommitSHA == nil || *pull.MergeCommitSHA != mergeSHA {
		t.Fatalf("pull after merge = %+v", pull)
	}
	if s.PullMergeable(repo, pull) {
		t.Fatal("merged pull still mergeable")
	}
	if _, _, ok := s.MergePull(repo, pull); ok {
		t.Fatal("second merge succeeded")
	}
	// nextNumber is shared across issues + pulls.
	issue2 := s.CreateIssue(repo, "second", "", nil)
	if issue2.Number != 3 {
		t.Fatalf("second issue number = %d, want 3 (shared counter)", issue2.Number)
	}
}

func TestCreatePullErrors(t *testing.T) {
	s := NewStore()
	repo := s.CreateRepo("demo", false, "")
	s.CreateCommitOnBranch(repo, "main", "f.txt", []byte("x"), "init")
	if _, msg, ok := s.CreatePull(repo, "t", "", "ghost", "main"); ok || msg != "No such head branch: ghost" {
		t.Fatalf("bad head: ok=%v msg=%q", ok, msg)
	}
	if _, msg, ok := s.CreatePull(repo, "t", "", "main", "ghost"); ok || msg != "No such base branch: ghost" {
		t.Fatalf("bad base: ok=%v msg=%q", ok, msg)
	}
}

func TestMergePullNotMergeable(t *testing.T) {
	s := NewStore()
	repo := s.CreateRepo("demo", false, "")
	base := s.CreateCommitOnBranch(repo, "main", "f.txt", []byte("a"), "base")
	repo.Refs["refs/heads/feature"] = base
	// Diverge both branches from the same base: head is not a descendant
	// of the (advanced) base, so the pull is not mergeable.
	s.CreateCommitOnBranch(repo, "main", "f.txt", []byte("b"), "advance base")
	s.CreateCommitOnBranch(repo, "feature", "g.txt", []byte("c"), "feature work")
	pull, _, ok := s.CreatePull(repo, "t", "", "feature", "main")
	if !ok {
		t.Fatal("CreatePull failed")
	}
	if s.PullMergeable(repo, pull) {
		t.Fatal("diverged pull reported mergeable")
	}
	if _, msg, ok := s.MergePull(repo, pull); ok || msg != "Pull request is not mergeable" {
		t.Fatalf("merge of diverged pull: ok=%v msg=%q", ok, msg)
	}
	// Same-tip pull (head == base) merges with a single parent.
	repo2 := s.CreateRepo("demo2", false, "")
	tip := s.CreateCommitOnBranch(repo2, "main", "f.txt", []byte("a"), "base")
	repo2.Refs["refs/heads/same"] = tip
	pull2, _, ok := s.CreatePull(repo2, "t", "", "same", "main")
	if !ok {
		t.Fatal("CreatePull failed")
	}
	sha, _, ok := s.MergePull(repo2, pull2)
	if !ok {
		t.Fatal("same-tip merge failed")
	}
	merged := ParseCommit(repo2.Objects[sha].Data)
	if len(merged.Parents) != 1 || merged.Parents[0] != tip {
		t.Fatalf("same-tip merge parents = %v, want [%s]", merged.Parents, tip)
	}
}

func TestCreateCommitOnBranchUpdatesFile(t *testing.T) {
	s := NewStore()
	repo := s.CreateRepo("demo", false, "")
	c1 := s.CreateCommitOnBranch(repo, "main", "a/b/c.txt", []byte("v1"), "one")
	c2 := s.CreateCommitOnBranch(repo, "main", "a/b/c.txt", []byte("v2"), "two")
	if c1 == c2 {
		t.Fatal("updating a file produced the same commit")
	}
	flat := s.FlattenTree(repo, ParseCommit(repo.Objects[c2].Data).Tree)
	if got := repo.Objects[flat["a/b/c.txt"]]; string(got.Data) != "v2" {
		t.Fatalf("updated blob = %q", got.Data)
	}
	parents := ParseCommit(repo.Objects[c2].Data).Parents
	if len(parents) != 1 || parents[0] != c1 {
		t.Fatalf("second commit parents = %v", parents)
	}
}

func TestTouchIssue(t *testing.T) {
	s := NewStore()
	repo := s.CreateRepo("demo", false, "")
	issue := s.CreateIssue(repo, "t", "", nil)
	issue.CachedJSON = "something"
	s.TouchIssue(issue)
	if issue.CachedJSON != "" {
		t.Fatal("TouchIssue did not clear the cache")
	}
}
