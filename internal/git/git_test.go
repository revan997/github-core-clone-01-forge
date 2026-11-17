package git

// Tests for the Go port of git.ts. The fixture repo and every golden byte
// string come from the Node spec run in /tmp/golden/gen.ts (goldens.json):
// the same blob/tree/commit/tag bytes, so the same object ids, and the same
// request bytes the kit's git-client.ts builds.
//
// Pack bytes are NOT compared byte-for-byte against Node: Node's zlib and
// Go's compress/zlib may deflate differently. Packs are compared decoded
// (object ids + object bytes) via the test-local plain-pack reader below,
// exactly as the kit checks do (readPack + SHA-1 recompute). Everything
// without compression (info/refs, ls-refs, push reports, error texts) is
// compared byte-for-byte.
//
// NOTE: these tests exercise store helpers (DecodePktLines, ReadPack,
// WritePack, CollectReachable, ParseCommit/ParseTag, HashObject, IsAncestor)
// through the handlers. Until the store package is implemented those
// helpers panic, so handler tests fail with "not implemented" — that is
// expected and reported as pending-store, not as a git-package failure.
// Pure helpers in this package (sidebandPack, fetchCacheKey, parseJSInt)
// run without the store.

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"testing"

	"forge/internal/store"
)

// ---------------------------------------------------------------------------------------------
// fixture: the same objects the Node golden script built
// ---------------------------------------------------------------------------------------------

const (
	fixtureOwner = "adaiversen"
	fixtureRepo  = "golden"
	fixtureIdent = "Conformance <conformance@make.fun>"
	fixtureTs    = 1700000000

	oidBlob1 = "3b18e512dba79e4c8300dd08aeb37f8e728b8dad"
	oidBlob2 = "1c59427adc4b205a270d8f810310394962e79a8b"
	oidBlob3 = "f9d1e1bf149e7fbecb4f3775e9f340fb1254bc8e"
	oidTree1 = "ebaa691b5554f29ac9d4f37811a1da6f24d376a1"
	oidTree2 = "96b0cc92738caa6b4cc1f953fecec4e61ee2903c"
	oidTree3 = "82b14b92fee62f54d8b1b4f61968e8c9e26b32c6"
	oidC1    = "ff8581eb26589be2993130ebaa3cf0045f43f1a2"
	oidC2    = "b1199b5f8e59b5b4538dee772c4901d29f307de3"
	oidC3    = "4ce14fa6650618489c2a9986d90ebc7695f1dba9"
	oidTag1  = "7efc4f66f4498ed6bba722c2d90dcd108648c263"
	oidZero  = store.ZeroOID
	oidGhost = "3333333333333333333333333333333333333333"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex: %v", err)
	}
	return b
}

// hexBytes decodes hex without a *testing.T (fixture builders only; panics
// on invalid input, which never happens for constant oids).
func hexBytes(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// hashObjectID is the test-local SHA-1 object id (independent of store.HashObject).
func hashObjectID(typ string, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

type treeEntry struct {
	mode string
	name string
	oid  string
}

// buildTreeBytes serializes tree entries in git order (same rule as the kit).
func buildTreeBytes(entries []treeEntry) []byte {
	sorted := append([]treeEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool {
		ai, aj := sorted[i], sorted[j]
		ki, kj := ai.name, aj.name
		if ai.mode == "40000" {
			ki += "/"
		}
		if aj.mode == "40000" {
			kj += "/"
		}
		return ki < kj
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		fmt.Fprintf(&buf, "%s %s\x00", e.mode, e.name)
		buf.Write(hexBytes(e.oid))
	}
	return buf.Bytes()
}

func buildCommit(tree string, parents []string, message string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "tree %s\n", tree)
	for _, p := range parents {
		fmt.Fprintf(&b, "parent %s\n", p)
	}
	fmt.Fprintf(&b, "author %s %d +0000\n", fixtureIdent, fixtureTs)
	fmt.Fprintf(&b, "committer %s %d +0000\n", fixtureIdent, fixtureTs)
	b.WriteString("\n")
	b.WriteString(message)
	return []byte(b.String())
}

func buildTag(object, tag, message string) []byte {
	return []byte(fmt.Sprintf("object %s\ntype commit\ntag %s\ntagger %s %d +0000\n\n%s",
		object, tag, fixtureIdent, fixtureTs, message))
}

// fixtureObjects returns the fixture's objects (oid -> object), built from
// the same literal bytes as the Node golden script.
func fixtureObjects(t *testing.T) map[string]store.GitObject {
	t.Helper()
	blob1 := []byte("hello world\n")
	blob2 := []byte("second file\n")
	blob3 := []byte("third file, revised\n")
	type objSpec struct {
		typ  string
		data []byte
	}
	specs := map[string]objSpec{
		oidBlob1: {"blob", blob1},
		oidBlob2: {"blob", blob2},
		oidBlob3: {"blob", blob3},
		oidTree1: {"tree", buildTreeBytes([]treeEntry{{"100644", "a.txt", oidBlob1}})},
		oidTree2: {"tree", buildTreeBytes([]treeEntry{{"100644", "a.txt", oidBlob1}, {"100644", "b.txt", oidBlob2}})},
		oidTree3: {"tree", buildTreeBytes([]treeEntry{{"100644", "a.txt", oidBlob1}, {"100644", "b.txt", oidBlob3}})},
		oidC1:    {"commit", buildCommit(oidTree1, nil, "first commit\n")},
		oidC2:    {"commit", buildCommit(oidTree2, []string{oidC1}, "second commit\n")},
		oidC3:    {"commit", buildCommit(oidTree3, []string{oidC2}, "third commit\n")},
		oidTag1:  {"tag", buildTag(oidC2, "v1", "version 1\n")},
	}
	objs := make(map[string]store.GitObject, len(specs))
	for oid, spec := range specs {
		// Sanity: the locally computed oids must equal the Node fixture oids.
		if got := hashObjectID(spec.typ, spec.data); got != oid {
			t.Fatalf("fixture oid mismatch: %s != %s", got, oid)
		}
		objs[oid] = store.GitObject{Type: store.ObjectType(spec.typ), Data: spec.data}
	}
	return objs
}

func fixtureRefs() map[string]string {
	return map[string]string{
		"refs/heads/main":    oidC3,
		"refs/heads/feature": oidC2,
		"refs/tags/v1":       oidTag1,
	}
}

// newGoldenStore builds a store holding the fixture repo, via struct literals
// (no store methods — they may still be stubs).
func newGoldenStore(t *testing.T) *store.Store {
	t.Helper()
	repo := &store.Repo{
		Owner:         fixtureOwner,
		Name:          fixtureRepo,
		Description:   "golden fixture",
		DefaultBranch: "main",
		Objects:       fixtureObjects(t),
		Refs:          fixtureRefs(),
	}
	return &store.Store{Repos: map[string]*store.Repo{fixtureOwner + "/" + fixtureRepo: repo}}
}

func newEmptyStore(t *testing.T) *store.Store {
	t.Helper()
	repo := &store.Repo{
		Owner:         fixtureOwner,
		Name:          "empty",
		DefaultBranch: "main",
		Objects:       map[string]store.GitObject{},
		Refs:          map[string]string{},
	}
	return &store.Store{Repos: map[string]*store.Repo{fixtureOwner + "/empty": repo}}
}

func newPushStore(t *testing.T) *store.Store {
	t.Helper()
	repo := &store.Repo{
		Owner:         fixtureOwner,
		Name:          "pushy",
		DefaultBranch: "main",
		Objects:       map[string]store.GitObject{},
		Refs:          map[string]string{},
	}
	return &store.Store{Repos: map[string]*store.Repo{fixtureOwner + "/pushy": repo}}
}

func pushRepo(t *testing.T, s *store.Store) *store.Repo {
	t.Helper()
	repo := s.GetRepo(fixtureOwner, "pushy")
	if repo == nil {
		t.Fatal("pushy repo missing")
	}
	return repo
}

// ---------------------------------------------------------------------------------------------
// test-local wire helpers (pkt-lines, packs) — independent of the store package
// ---------------------------------------------------------------------------------------------

func testPktLine(payload []byte) []byte {
	out := make([]byte, 4, len(payload)+4)
	copy(out, fmt.Sprintf("%04x", len(payload)+4))
	return append(out, payload...)
}

func testPktLineString(s string) []byte { return testPktLine([]byte(s)) }

// encodePktLines mirrors the kit's encodePktLines: strings are data packets,
// "flush"/"delim" are control packets.
func encodePktLines(items []any) []byte {
	var out []byte
	for _, item := range items {
		switch v := item.(type) {
		case string:
			if v == "flush" {
				out = append(out, []byte("0000")...)
			} else if v == "delim" {
				out = append(out, []byte("0001")...)
			} else {
				out = append(out, testPktLineString(v)...)
			}
		case []byte:
			out = append(out, testPktLine(v)...)
		}
	}
	return out
}

type testPacket struct {
	kind    string // "data" | "flush" | "delim" | "response-end"
	payload []byte
}

// decodeTestPktLines decodes pkt-lines the way store.DecodePktLines does:
// stops at the first non-pkt-line byte and reports the offset past the last
// packet (the start of a trailing pack).
func decodeTestPktLines(body []byte) ([]testPacket, int) {
	var packets []testPacket
	offset := 0
	for offset+4 <= len(body) {
		hdr := string(body[offset : offset+4])
		isHex := true
		for _, c := range hdr {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				isHex = false
			}
		}
		if !isHex {
			break
		}
		length, err := strconv.ParseInt(hdr, 16, 32)
		if err != nil {
			break
		}
		if length == 0 {
			packets = append(packets, testPacket{kind: "flush"})
			offset += 4
			continue
		}
		if length == 1 {
			packets = append(packets, testPacket{kind: "delim"})
			offset += 4
			continue
		}
		if length == 2 {
			packets = append(packets, testPacket{kind: "response-end"})
			offset += 4
			continue
		}
		if offset+int(length) > len(body) {
			break
		}
		packets = append(packets, testPacket{kind: "data", payload: body[offset+4 : offset+int(length)]})
		offset += int(length)
	}
	return packets, offset
}

// testPackObjectHeader encodes one object's pack header (type/size varint).
func testPackObjectHeader(typ store.ObjectType, size int) []byte {
	codes := map[store.ObjectType]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
	code := codes[typ]
	var out []byte
	remaining := size
	first := byte((code << 4) | (remaining & 0x0f))
	remaining >>= 4
	for remaining > 0 {
		out = append(out, first|0x80)
		first = byte(remaining & 0x7f)
		remaining >>= 7
	}
	out = append(out, first)
	return out
}

// buildPack writes a version-2 pack of plain objects, oid-sorted (like the
// kit's writePack and the store's WritePack).
func buildPack(t *testing.T, objs []store.GitObject) []byte {
	t.Helper()
	type keyed struct {
		oid string
		obj store.GitObject
	}
	keyedList := make([]keyed, 0, len(objs))
	for _, obj := range objs {
		keyedList = append(keyedList, keyed{hashObjectID(string(obj.Type), obj.Data), obj})
	}
	sort.Slice(keyedList, func(i, j int) bool { return keyedList[i].oid < keyedList[j].oid })
	var buf bytes.Buffer
	buf.WriteString("PACK")
	_ = binary.Write(&buf, binary.BigEndian, uint32(2))
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(keyedList)))
	for _, k := range keyedList {
		buf.Write(testPackObjectHeader(k.obj.Type, len(k.obj.Data)))
		var zb bytes.Buffer
		w := zlib.NewWriter(&zb)
		if _, err := w.Write(k.obj.Data); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		buf.Write(zb.Bytes())
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

// inflateAt inflates the zlib stream at pack[start:] and returns the data and
// the offset just past the stream. The end is found by binary search: a
// truncated stream errors, a longer slice inflates fine (trailing bytes are
// ignored), so success is monotonic in the end offset.
func inflateAt(t *testing.T, pack []byte, start int) ([]byte, int) {
	t.Helper()
	lo, hi := start, len(pack)
	for lo < hi {
		mid := (lo + hi) / 2
		r, err := zlib.NewReader(bytes.NewReader(pack[start:mid]))
		if err != nil {
			lo = mid + 1
			continue
		}
		data, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			lo = mid + 1
			continue
		}
		hi = mid
		_ = data
	}
	// lo is the minimal end that inflates; verify it.
	r, err := zlib.NewReader(bytes.NewReader(pack[start:lo]))
	if err != nil {
		t.Fatalf("inflateAt: no zlib stream end found at %d: %v", start, err)
	}
	data, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatalf("inflateAt: inflate failed at %d: %v", start, err)
	}
	return data, lo
}

// readPackObjects parses a plain-object pack (the server only writes plain
// objects) and returns oid -> object. Fails loudly on deltas.
func readPackObjects(t *testing.T, pack []byte) map[string]store.GitObject {
	t.Helper()
	if len(pack) < 32 || string(pack[:4]) != "PACK" {
		t.Fatalf("not a packfile (%d bytes)", len(pack))
	}
	version := binary.BigEndian.Uint32(pack[4:8])
	if version != 2 && version != 3 {
		t.Fatalf("unsupported pack version %d", version)
	}
	count := binary.BigEndian.Uint32(pack[8:12])
	trailer := pack[len(pack)-20:]
	sum := sha1.Sum(pack[:len(pack)-20])
	if !bytes.Equal(trailer, sum[:]) {
		t.Fatalf("pack checksum mismatch")
	}
	typeNames := map[int]store.ObjectType{1: "commit", 2: "tree", 3: "blob", 4: "tag"}
	objects := map[string]store.GitObject{}
	offset := 12
	for i := uint32(0); i < count; i++ {
		start := offset
		b := pack[offset]
		offset++
		typeNum := (b >> 4) & 0x7
		size := int(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			b = pack[offset]
			offset++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		if typeNum == 6 || typeNum == 7 {
			t.Fatalf("test pack reader: delta at offset %d not supported", start)
		}
		typ, ok := typeNames[int(typeNum)]
		if !ok {
			t.Fatalf("unknown pack type %d at offset %d", typeNum, start)
		}
		data, next := inflateAt(t, pack, offset)
		if len(data) != size {
			t.Fatalf("pack object size mismatch at %d: %d != %d", start, len(data), size)
		}
		offset = next
		oid := hashObjectID(string(typ), data)
		objects[oid] = store.GitObject{Type: typ, Data: data}
	}
	if offset != len(pack)-20 {
		t.Fatalf("pack has %d stray bytes before trailer", len(pack)-20-offset)
	}
	return objects
}

func objectOIDs(objs map[string]store.GitObject) []string {
	out := make([]string, 0, len(objs))
	for oid := range objs {
		out = append(out, oid)
	}
	sort.Strings(out)
	return out
}

// parseV2Fetch decodes a v2 fetch response into section lines (with the
// "section:" prefix the golden tables use) and the reassembled pack bytes.
func parseV2Fetch(t *testing.T, body []byte) (lines []string, pack []byte) {
	t.Helper()
	packets, _ := decodeTestPktLines(body)
	section := ""
	var chunks [][]byte
	for _, p := range packets {
		if p.kind == "flush" || p.kind == "delim" {
			section = ""
			continue
		}
		if p.kind != "data" {
			continue
		}
		if section == "" {
			section = strings.TrimSuffix(string(p.payload), "\n")
			lines = append(lines, "section:"+section)
			continue
		}
		if section == "packfile" {
			if len(p.payload) < 1 || p.payload[0] != 1 {
				t.Fatalf("packfile section: bad sideband band %v", p.payload[:1])
			}
			chunks = append(chunks, p.payload[1:])
			continue
		}
		lines = append(lines, section+":"+strings.TrimSuffix(string(p.payload), "\n"))
	}
	return lines, bytes.Join(chunks, nil)
}

// parseV0Fetch decodes a v0 fetch response into control lines and pack bytes.
func parseV0Fetch(t *testing.T, body []byte) (lines []string, pack []byte) {
	t.Helper()
	packets, _ := decodeTestPktLines(body)
	var chunks [][]byte
	for _, p := range packets {
		if p.kind != "data" {
			continue
		}
		band := p.payload[0]
		if band == 1 || band == 2 || band == 3 {
			if band == 1 {
				chunks = append(chunks, p.payload[1:])
			}
			continue
		}
		lines = append(lines, strings.TrimSuffix(string(p.payload), "\n"))
	}
	return lines, bytes.Join(chunks, nil)
}

// ---------------------------------------------------------------------------------------------
// request builders (mirror the kit's git-client.ts exactly)
// ---------------------------------------------------------------------------------------------

func fetchV2Body(wants, haves, shallow []string, depth *int) []byte {
	items := []any{"command=fetch\n", "agent=makefun-git-client/1.0\n", "object-format=sha1\n", "delim",
		"thin-pack\n", "no-progress\n", "ofs-delta\n"}
	for _, w := range wants {
		items = append(items, "want "+w+"\n")
	}
	for _, h := range haves {
		items = append(items, "have "+h+"\n")
	}
	for _, s := range shallow {
		items = append(items, "shallow "+s+"\n")
	}
	if depth != nil {
		items = append(items, fmt.Sprintf("deepen %d\n", *depth))
	}
	items = append(items, "done\n", "flush")
	return encodePktLines(items)
}

func fetchV0Body(wants, haves, shallow []string, depth *int) []byte {
	items := []any{}
	first := true
	for _, w := range wants {
		if first {
			items = append(items, "want "+w+" side-band-64k multi_ack thin-pack no-progress ofs-delta\n")
		} else {
			items = append(items, "want "+w+"\n")
		}
		first = false
	}
	for _, s := range shallow {
		items = append(items, "shallow "+s+"\n")
	}
	if depth != nil {
		items = append(items, fmt.Sprintf("deepen %d\n", *depth))
	}
	items = append(items, "flush")
	for _, h := range haves {
		items = append(items, "have "+h+"\n")
	}
	items = append(items, "done\n", "flush")
	return encodePktLines(items)
}

func lsRefsBody(args ...string) []byte {
	items := []any{"command=ls-refs\n", "agent=makefun-git-client/1.0\n", "object-format=sha1\n", "delim"}
	for _, a := range args {
		items = append(items, a)
	}
	items = append(items, "flush")
	return encodePktLines(items)
}

type pushCmd struct{ oldOID, newOID, ref string }

func pushBody(t *testing.T, commands []pushCmd, pack []byte, caps string) []byte {
	t.Helper()
	items := []any{}
	for i, c := range commands {
		prefix := c.oldOID + " " + c.newOID + " " + c.ref
		if i == 0 && caps != "" {
			items = append(items, prefix+"\x00"+caps+"\n")
		} else {
			items = append(items, prefix+"\n")
		}
	}
	items = append(items, "flush")
	return append(encodePktLines(items), pack...)
}

const pushCaps = "report-status side-band-64k ofs-delta agent=makefun-git-client/1.0"

// pushObjects builds a push body carrying the given fixture objects.
func pushObjects(t *testing.T, s *store.Store, repoName string, commands []pushCmd, oids []string, caps string) []byte {
	t.Helper()
	repo := s.GetRepo(fixtureOwner, repoName)
	if repo == nil {
		t.Fatalf("repo %s missing", repoName)
	}
	// Objects for the pack come from the golden fixture (the sender's view),
	// not the server store.
	fixture := fixtureObjects(t)
	var objs []store.GitObject
	for _, oid := range oids {
		obj, ok := fixture[oid]
		if !ok {
			t.Fatalf("fixture has no object %s", oid)
		}
		objs = append(objs, obj)
	}
	return pushBody(t, commands, buildPack(t, objs), caps)
}

// ---------------------------------------------------------------------------------------------
// assertions
// ---------------------------------------------------------------------------------------------

func assertResponse(t *testing.T, resp GitResponse, wantHex string, wantStatus int, wantCT string) {
	t.Helper()
	if resp.Status != wantStatus {
		t.Fatalf("status: got %d, want %d (body %q)", resp.Status, wantStatus, resp.Body)
	}
	if resp.ContentType != wantCT {
		t.Fatalf("content-type: got %q, want %q", resp.ContentType, wantCT)
	}
	want, err := hex.DecodeString(wantHex)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(resp.Body, want) {
		t.Fatalf("body mismatch:\n got %x\nwant %x", resp.Body, want)
	}
}

func assertFetchV2(t *testing.T, resp GitResponse, wantLines, wantOids []string) {
	t.Helper()
	if resp.Status != 200 {
		t.Fatalf("fetch v2 status %d: %q", resp.Status, resp.Body)
	}
	if resp.ContentType != "application/x-git-upload-pack-result" {
		t.Fatalf("fetch v2 content-type %q", resp.ContentType)
	}
	lines, pack := parseV2Fetch(t, resp.Body)
	if strings.Join(lines, "|") != strings.Join(wantLines, "|") {
		t.Fatalf("fetch v2 lines:\n got %q\nwant %q", lines, wantLines)
	}
	got := objectOIDs(readPackObjects(t, pack))
	if strings.Join(got, "|") != strings.Join(wantOids, "|") {
		t.Fatalf("fetch v2 pack objects:\n got %v\nwant %v", got, wantOids)
	}
}

func assertFetchV0(t *testing.T, resp GitResponse, wantLines, wantOids []string) {
	t.Helper()
	if resp.Status != 200 {
		t.Fatalf("fetch v0 status %d: %q", resp.Status, resp.Body)
	}
	if resp.ContentType != "application/x-git-upload-pack-result" {
		t.Fatalf("fetch v0 content-type %q", resp.ContentType)
	}
	lines, pack := parseV0Fetch(t, resp.Body)
	if strings.Join(lines, "|") != strings.Join(wantLines, "|") {
		t.Fatalf("fetch v0 lines:\n got %q\nwant %q", lines, wantLines)
	}
	got := objectOIDs(readPackObjects(t, pack))
	if strings.Join(got, "|") != strings.Join(wantOids, "|") {
		t.Fatalf("fetch v0 pack objects:\n got %v\nwant %v", got, wantOids)
	}
}

func assertTextError(t *testing.T, resp GitResponse, wantStatus int, wantBody string) {
	t.Helper()
	if resp.Status != wantStatus {
		t.Fatalf("status: got %d, want %d (body %q)", resp.Status, wantStatus, resp.Body)
	}
	if resp.ContentType != "text/plain; charset=utf-8" {
		t.Fatalf("content-type: got %q", resp.ContentType)
	}
	if string(resp.Body) != wantBody {
		t.Fatalf("body: got %q, want %q", resp.Body, wantBody)
	}
}

func assertRefs(t *testing.T, repo *store.Repo, want map[string]string) {
	t.Helper()
	if len(repo.Refs) != len(want) {
		t.Fatalf("refs: got %v, want %v", repo.Refs, want)
	}
	for k, v := range want {
		if repo.Refs[k] != v {
			t.Fatalf("ref %s: got %q, want %q", k, repo.Refs[k], v)
		}
	}
}

// ---------------------------------------------------------------------------------------------
// info/refs
// ---------------------------------------------------------------------------------------------

func TestInfoRefsV0Upload(t *testing.T) {
	s := newGoldenStore(t)
	resp := HandleInfoRefs(s, fixtureOwner, fixtureRepo, "git-upload-pack", "")
	assertResponse(t, resp, goldenInfoV0Upload, 200, "application/x-git-upload-pack-advertisement")
}

func TestInfoRefsV0Receive(t *testing.T) {
	s := newGoldenStore(t)
	resp := HandleInfoRefs(s, fixtureOwner, fixtureRepo, "git-receive-pack", "")
	assertResponse(t, resp, goldenInfoV0Receive, 200, "application/x-git-receive-pack-advertisement")
}

func TestInfoRefsV2Upload(t *testing.T) {
	s := newGoldenStore(t)
	resp := HandleInfoRefs(s, fixtureOwner, fixtureRepo, "git-upload-pack", "version=2")
	assertResponse(t, resp, goldenInfoV2Upload, 200, "application/x-git-upload-pack-advertisement")
	// A v2 header on receive-pack must NOT switch it to v2 (receive-pack is v0-only).
	resp = HandleInfoRefs(s, fixtureOwner, fixtureRepo, "git-receive-pack", "version=2")
	assertResponse(t, resp, goldenInfoV0Receive, 200, "application/x-git-receive-pack-advertisement")
	// No version=2 in the header -> v0.
	resp = HandleInfoRefs(s, fixtureOwner, fixtureRepo, "git-upload-pack", "")
	assertResponse(t, resp, goldenInfoV0Upload, 200, "application/x-git-upload-pack-advertisement")
}

func TestInfoRefsEmptyRepo(t *testing.T) {
	s := newEmptyStore(t)
	resp := HandleInfoRefs(s, fixtureOwner, "empty", "git-upload-pack", "")
	assertResponse(t, resp, goldenInfoV0UploadEmpty, 200, "application/x-git-upload-pack-advertisement")
	resp = HandleInfoRefs(s, fixtureOwner, "empty", "git-receive-pack", "")
	assertResponse(t, resp, goldenInfoV0ReceiveEmpty, 200, "application/x-git-receive-pack-advertisement")
	resp = HandleInfoRefs(s, fixtureOwner, "empty", "git-upload-pack", "version=2")
	assertResponse(t, resp, goldenInfoV2Upload, 200, "application/x-git-upload-pack-advertisement")
}

func TestInfoRefsUnknownRepo(t *testing.T) {
	s := newGoldenStore(t)
	resp := HandleInfoRefs(s, fixtureOwner, "nope", "git-upload-pack", "")
	if resp.Status != 404 || resp.ContentType != "application/json; charset=utf-8" || string(resp.Body) != `{"message":"Not Found"}` {
		t.Fatalf("unknown repo: %+v body %q", resp, resp.Body)
	}
}

func TestInfoRefsCacheStable(t *testing.T) {
	s := newGoldenStore(t)
	first := HandleInfoRefs(s, fixtureOwner, fixtureRepo, "git-upload-pack", "")
	second := HandleInfoRefs(s, fixtureOwner, fixtureRepo, "git-upload-pack", "")
	if !bytes.Equal(first.Body, second.Body) {
		t.Fatal("repeated info/refs differs (cache must return identical bytes)")
	}
	// A ref change must invalidate the cached advertisement.
	repo := s.GetRepo(fixtureOwner, fixtureRepo)
	repo.Refs["refs/heads/other"] = oidC1
	third := HandleInfoRefs(s, fixtureOwner, fixtureRepo, "git-upload-pack", "")
	if bytes.Equal(first.Body, third.Body) {
		t.Fatal("info/refs did not change after a ref was added")
	}
	if !bytes.Contains(third.Body, []byte("refs/heads/other")) {
		t.Fatal("new ref missing from advertisement")
	}
}

// ---------------------------------------------------------------------------------------------
// ls-refs (v2)
// ---------------------------------------------------------------------------------------------

func TestLsRefs(t *testing.T) {
	s := newGoldenStore(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"all", []string{"peel\n", "symrefs\n", "unborn\n"}, goldenLsrefsAll},
		{"plain", nil, goldenLsrefsPlain},
		{"heads", []string{"peel\n", "symrefs\n", "unborn\n", "ref-prefix refs/heads/\n"}, goldenLsrefsHeads},
		{"tags", []string{"peel\n", "symrefs\n", "unborn\n", "ref-prefix refs/tags/\n"}, goldenLsrefsTags},
		{"none", []string{"peel\n", "symrefs\n", "unborn\n", "ref-prefix refs/nope/\n"}, goldenLsrefsNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := HandleUploadPack(s, fixtureOwner, fixtureRepo, lsRefsBody(c.args...))
			assertResponse(t, resp, c.want, 200, "application/x-git-upload-pack-result")
		})
	}
}

func TestLsRefsEmptyRepo(t *testing.T) {
	s := newEmptyStore(t)
	resp := HandleUploadPack(s, fixtureOwner, "empty", lsRefsBody("peel\n", "symrefs\n", "unborn\n"))
	assertResponse(t, resp, goldenLsrefsEmpty, 200, "application/x-git-upload-pack-result")
	resp = HandleUploadPack(s, fixtureOwner, "empty", lsRefsBody())
	assertResponse(t, resp, goldenLsrefsEmptyNounborn, 200, "application/x-git-upload-pack-result")
}

// ---------------------------------------------------------------------------------------------
// upload-pack: v2 fetch
// ---------------------------------------------------------------------------------------------

func TestFetchV2(t *testing.T) {
	s := newGoldenStore(t)
	one, two := 1, 2
	cases := []struct {
		name         string
		wants, haves []string
		shallow      []string
		depth        *int
		wantLines    []string
		wantOids     []string
	}{
		{"full", []string{oidC3, oidTag1}, nil, nil, nil, goldenFetchV2FullLines, goldenFetchV2FullOids},
		{"full-main-only", []string{oidC3}, nil, nil, nil, goldenFetchV2FullMainOnlyLines, goldenFetchV2FullMainOnlyOids},
		{"incremental", []string{oidC3}, []string{oidC2}, nil, nil, goldenFetchV2IncrementalLines, goldenFetchV2IncrementalOids},
		{"nothing-new", []string{oidC3}, []string{oidC3}, nil, nil, goldenFetchV2NothingNewLines, goldenFetchV2NothingNewOids},
		{"shallow1", []string{oidC3}, nil, nil, &one, goldenFetchV2Shallow1Lines, goldenFetchV2Shallow1Oids},
		{"deepen2", []string{oidC3}, []string{oidC3}, []string{oidC3}, &two, goldenFetchV2Deepen2Lines, goldenFetchV2Deepen2Oids},
		{"shallow-new-tip", []string{oidC3}, []string{oidC2}, []string{oidC2}, &one, goldenFetchV2ShallowNewTipLines, goldenFetchV2ShallowNewTipOids},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := HandleUploadPack(s, fixtureOwner, fixtureRepo, fetchV2Body(c.wants, c.haves, c.shallow, c.depth))
			assertFetchV2(t, resp, c.wantLines, c.wantOids)
		})
	}
}

func TestFetchV2Errors(t *testing.T) {
	s := newGoldenStore(t)
	resp := HandleUploadPack(s, fixtureOwner, fixtureRepo, fetchV2Body([]string{"1111111111111111111111111111111111111111"}, nil, nil, nil))
	assertTextError(t, resp, 400, "want 1111111111111111111111111111111111111111 is not known")
	resp = HandleUploadPack(s, fixtureOwner, fixtureRepo, fetchV2Body(nil, nil, nil, nil))
	assertTextError(t, resp, 400, "fetch without wants")
}

func TestFetchV2CacheHit(t *testing.T) {
	s := newGoldenStore(t)
	body := fetchV2Body([]string{oidC3}, nil, nil, nil)
	first := HandleUploadPack(s, fixtureOwner, fixtureRepo, body)
	second := HandleUploadPack(s, fixtureOwner, fixtureRepo, body)
	if !bytes.Equal(first.Body, second.Body) {
		t.Fatal("identical v2 fetches returned different bytes (cache must hit)")
	}
}

// ---------------------------------------------------------------------------------------------
// upload-pack: v0 fetch
// ---------------------------------------------------------------------------------------------

func TestFetchV0(t *testing.T) {
	s := newGoldenStore(t)
	one := 1
	cases := []struct {
		name         string
		wants, haves []string
		shallow      []string
		depth        *int
		wantLines    []string
		wantOids     []string
	}{
		{"full", []string{oidC3, oidTag1}, nil, nil, nil, goldenFetchV0FullLines, goldenFetchV0FullOids},
		{"incremental", []string{oidC3}, []string{oidC2}, nil, nil, goldenFetchV0IncrementalLines, goldenFetchV0IncrementalOids},
		{"nothing-new", []string{oidC3}, []string{oidC3}, nil, nil, goldenFetchV0NothingNewLines, goldenFetchV0NothingNewOids},
		{"shallow1", []string{oidC3}, nil, nil, &one, goldenFetchV0Shallow1Lines, goldenFetchV0Shallow1Oids},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := HandleUploadPack(s, fixtureOwner, fixtureRepo, fetchV0Body(c.wants, c.haves, c.shallow, c.depth))
			assertFetchV0(t, resp, c.wantLines, c.wantOids)
		})
	}
}

func TestFetchV0Errors(t *testing.T) {
	s := newGoldenStore(t)
	// No wants at all, first data packet is "done" -> malformed request.
	resp := HandleUploadPack(s, fixtureOwner, fixtureRepo, fetchV0Body(nil, nil, nil, nil))
	assertTextError(t, resp, 400, "malformed upload-pack request")
	// A "want " line with an empty oid reaches v0 and reports no want lines.
	resp = HandleUploadPack(s, fixtureOwner, fixtureRepo, encodePktLines([]any{"want \n", "flush"}))
	assertTextError(t, resp, 400, "no want lines")
	resp = HandleUploadPack(s, fixtureOwner, fixtureRepo, fetchV0Body([]string{"2222222222222222222222222222222222222222"}, nil, nil, nil))
	assertTextError(t, resp, 400, "want 2222222222222222222222222222222222222222 is not known")
}

func TestUploadPackMalformed(t *testing.T) {
	s := newGoldenStore(t)
	resp := HandleUploadPack(s, fixtureOwner, fixtureRepo, []byte{})
	assertTextError(t, resp, 400, "empty upload-pack request")
	resp = HandleUploadPack(s, fixtureOwner, fixtureRepo, []byte("zzzznot-a-pkt-line"))
	assertTextError(t, resp, 400, "empty upload-pack request")
	resp = HandleUploadPack(s, fixtureOwner, fixtureRepo, encodePktLines([]any{"command=bogus\n", "flush"}))
	assertTextError(t, resp, 400, "malformed upload-pack request")
	resp = HandleUploadPack(s, fixtureOwner, "nope", fetchV2Body([]string{oidC3}, nil, nil, nil))
	if resp.Status != 404 || string(resp.Body) != `{"message":"Not Found"}` {
		t.Fatalf("unknown repo upload-pack: %+v body %q", resp, resp.Body)
	}
}

// ---------------------------------------------------------------------------------------------
// receive-pack: the golden sequence, replayed on one store in Node order
// ---------------------------------------------------------------------------------------------

func TestReceivePackSequence(t *testing.T) {
	s := newPushStore(t)
	repo := pushRepo(t, s)

	// 1. create main at c1.
	resp := HandleReceivePack(s, fixtureOwner, "pushy",
		pushObjects(t, s, "pushy", []pushCmd{{oidZero, oidC1, "refs/heads/main"}}, []string{oidC1, oidTree1, oidBlob1}, pushCaps))
	assertResponse(t, resp, goldenPushCreateMain, 200, "application/x-git-receive-pack-result")
	assertRefs(t, repo, map[string]string{"refs/heads/main": oidC1})

	// 2. fast-forward main c1 -> c2.
	resp = HandleReceivePack(s, fixtureOwner, "pushy",
		pushObjects(t, s, "pushy", []pushCmd{{oidC1, oidC2, "refs/heads/main"}}, []string{oidC2, oidTree2, oidBlob2}, pushCaps))
	assertResponse(t, resp, goldenPushUpdateMain, 200, "application/x-git-receive-pack-result")
	assertRefs(t, repo, map[string]string{"refs/heads/main": oidC2})

	// 3. create tag v1.
	resp = HandleReceivePack(s, fixtureOwner, "pushy",
		pushObjects(t, s, "pushy", []pushCmd{{oidZero, oidTag1, "refs/tags/v1"}}, []string{oidTag1}, pushCaps))
	assertResponse(t, resp, goldenPushCreateTag, 200, "application/x-git-receive-pack-result")
	assertRefs(t, repo, map[string]string{"refs/heads/main": oidC2, "refs/tags/v1": oidTag1})

	// 4. non-fast-forward main c2 -> c1: refused, ref unchanged, objects kept.
	resp = HandleReceivePack(s, fixtureOwner, "pushy",
		pushObjects(t, s, "pushy", []pushCmd{{oidC2, oidC1, "refs/heads/main"}}, []string{oidC1, oidTree1, oidBlob1}, pushCaps))
	assertResponse(t, resp, goldenPushNonFf, 200, "application/x-git-receive-pack-result")
	assertRefs(t, repo, map[string]string{"refs/heads/main": oidC2, "refs/tags/v1": oidTag1})

	// 5. mixed: main c2 -> c3 ok, feature c2 -> c1 refused.
	resp = HandleReceivePack(s, fixtureOwner, "pushy",
		pushObjects(t, s, "pushy", []pushCmd{
			{oidC2, oidC3, "refs/heads/main"},
			{oidC2, oidC1, "refs/heads/feature"},
		}, []string{oidC3, oidTree3, oidBlob3, oidC1, oidTree1, oidBlob1}, pushCaps))
	assertResponse(t, resp, goldenPushMixed, 200, "application/x-git-receive-pack-result")
	assertRefs(t, repo, map[string]string{"refs/heads/main": oidC3, "refs/tags/v1": oidTag1})

	// 6. create feature at c1 (empty pack: c1 is already stored).
	resp = HandleReceivePack(s, fixtureOwner, "pushy",
		pushObjects(t, s, "pushy", []pushCmd{{oidZero, oidC1, "refs/heads/feature"}}, nil, pushCaps))
	assertResponse(t, resp, goldenPushCreateFeature, 200, "application/x-git-receive-pack-result")
	assertRefs(t, repo, map[string]string{"refs/heads/main": oidC3, "refs/tags/v1": oidTag1, "refs/heads/feature": oidC1})

	// 7. delete feature (no pack at all).
	resp = HandleReceivePack(s, fixtureOwner, "pushy",
		pushBody(t, []pushCmd{{oidC1, oidZero, "refs/heads/feature"}}, nil, pushCaps))
	assertResponse(t, resp, goldenPushDeleteFeature, 200, "application/x-git-receive-pack-result")
	assertRefs(t, repo, map[string]string{"refs/heads/main": oidC3, "refs/tags/v1": oidTag1})

	// 8. delete a ref that does not exist.
	resp = HandleReceivePack(s, fixtureOwner, "pushy",
		pushBody(t, []pushCmd{{oidZero, oidZero, "refs/heads/gone"}}, nil, pushCaps))
	assertResponse(t, resp, goldenPushDeleteMissing, 200, "application/x-git-receive-pack-result")
	assertRefs(t, repo, map[string]string{"refs/heads/main": oidC3, "refs/tags/v1": oidTag1})

	// 9. invalid ref name (objects still unpack).
	resp = HandleReceivePack(s, fixtureOwner, "pushy",
		pushObjects(t, s, "pushy", []pushCmd{{oidZero, oidC1, "badref"}}, []string{oidC1, oidTree1, oidBlob1}, pushCaps))
	assertResponse(t, resp, goldenPushInvalidRef, 200, "application/x-git-receive-pack-result")
	assertRefs(t, repo, map[string]string{"refs/heads/main": oidC3, "refs/tags/v1": oidTag1})

	// 10. update with no pack at all -> unpacker error.
	resp = HandleReceivePack(s, fixtureOwner, "pushy",
		pushBody(t, []pushCmd{{oidZero, oidC2, "refs/heads/other"}}, nil, pushCaps))
	assertResponse(t, resp, goldenPushNoPackUpdate, 200, "application/x-git-receive-pack-result")
	assertRefs(t, repo, map[string]string{"refs/heads/main": oidC3, "refs/tags/v1": oidTag1})

	// 11. corrupt trailer -> pack checksum mismatch.
	good := pushObjects(t, s, "pushy", []pushCmd{{oidC3, oidC3, "refs/heads/main"}}, []string{oidC3, oidTree3, oidBlob3}, pushCaps)
	bad := append([]byte(nil), good...)
	bad[len(bad)-1] ^= 0xff
	resp = HandleReceivePack(s, fixtureOwner, "pushy", bad)
	assertResponse(t, resp, goldenPushBadChecksum, 200, "application/x-git-receive-pack-result")
	assertRefs(t, repo, map[string]string{"refs/heads/main": oidC3, "refs/tags/v1": oidTag1})

	// 12. truncated pack (last 10 bytes cut) -> pack checksum mismatch.
	resp = HandleReceivePack(s, fixtureOwner, "pushy", good[:len(good)-10])
	assertResponse(t, resp, goldenPushTruncatedPack, 200, "application/x-git-receive-pack-result")
	assertRefs(t, repo, map[string]string{"refs/heads/main": oidC3, "refs/tags/v1": oidTag1})

	// 13-15. malformed command lists.
	resp = HandleReceivePack(s, fixtureOwner, "pushy", encodePktLines([]any{"not a command\n", "flush"}))
	assertTextError(t, resp, 400, "malformed push command")
	resp = HandleReceivePack(s, fixtureOwner, "pushy", encodePktLines([]any{"flush"}))
	assertTextError(t, resp, 400, "malformed push command list")
	resp = HandleReceivePack(s, fixtureOwner, "pushy", encodePktLines([]any{oidZero + " " + oidC1 + " refs/heads/x\n"}))
	assertTextError(t, resp, 400, "malformed push command list")

	// 16. no side-band-64k negotiated -> plain (unframed) report.
	resp = HandleReceivePack(s, fixtureOwner, "pushy",
		pushObjects(t, s, "pushy", []pushCmd{{oidZero, oidC1, "refs/heads/nosb"}}, []string{oidC1, oidTree1, oidBlob1}, "report-status"))
	assertResponse(t, resp, goldenPushNoSideband, 200, "application/x-git-receive-pack-result")
	assertRefs(t, repo, map[string]string{"refs/heads/main": oidC3, "refs/tags/v1": oidTag1, "refs/heads/nosb": oidC1})

	// 17. no-op update with an (empty) pack -> ok.
	resp = HandleReceivePack(s, fixtureOwner, "pushy",
		pushObjects(t, s, "pushy", []pushCmd{{oidC3, oidC3, "refs/heads/main"}}, nil, pushCaps))
	assertResponse(t, resp, goldenPushNoopWithPack, 200, "application/x-git-receive-pack-result")

	// 18. update to an object the store does not hold (empty pack) -> ng.
	resp = HandleReceivePack(s, fixtureOwner, "pushy",
		pushObjects(t, s, "pushy", []pushCmd{{oidC3, oidGhost, "refs/heads/main"}}, nil, pushCaps))
	assertResponse(t, resp, goldenPushMissingObjectWithPack, 200, "application/x-git-receive-pack-result")
	assertRefs(t, repo, map[string]string{"refs/heads/main": oidC3, "refs/tags/v1": oidTag1, "refs/heads/nosb": oidC1})

	// 19. create at an object the store does not hold (empty pack) -> ng.
	resp = HandleReceivePack(s, fixtureOwner, "pushy",
		pushObjects(t, s, "pushy", []pushCmd{{oidZero, oidGhost, "refs/heads/ghost"}}, nil, pushCaps))
	assertResponse(t, resp, goldenPushCreateMissingObject, 200, "application/x-git-receive-pack-result")
	assertRefs(t, repo, map[string]string{"refs/heads/main": oidC3, "refs/tags/v1": oidTag1, "refs/heads/nosb": oidC1})
}

func TestReceivePackUnknownRepo(t *testing.T) {
	s := newPushStore(t)
	resp := HandleReceivePack(s, fixtureOwner, "nope",
		pushObjects(t, s, "pushy", []pushCmd{{oidZero, oidC1, "refs/heads/main"}}, []string{oidC1, oidTree1, oidBlob1}, pushCaps))
	if resp.Status != 404 || string(resp.Body) != `{"message":"Not Found"}` {
		t.Fatalf("unknown repo receive-pack: %+v body %q", resp, resp.Body)
	}
}

// TestReceiveThenFetchRoundTrip pushes the fixture history, then fetches it
// back and verifies the objects and refs match the fixture exactly.
func TestReceiveThenFetchRoundTrip(t *testing.T) {
	s := newPushStore(t)
	repo := pushRepo(t, s)
	resp := HandleReceivePack(s, fixtureOwner, "pushy",
		pushObjects(t, s, "pushy", []pushCmd{{oidZero, oidC3, "refs/heads/main"}}, []string{
			oidC1, oidC2, oidC3, oidTree1, oidTree2, oidTree3, oidBlob1, oidBlob2, oidBlob3,
		}, pushCaps))
	if resp.Status != 200 || !bytes.Contains(resp.Body, []byte("ok refs/heads/main")) {
		t.Fatalf("setup push failed: %+v body %q", resp, resp.Body)
	}
	resp = HandleReceivePack(s, fixtureOwner, "pushy",
		pushObjects(t, s, "pushy", []pushCmd{{oidZero, oidTag1, "refs/tags/v1"}}, []string{oidTag1}, pushCaps))
	if resp.Status != 200 || !bytes.Contains(resp.Body, []byte("ok refs/tags/v1")) {
		t.Fatalf("tag push failed: %+v body %q", resp, resp.Body)
	}
	// Fetch everything back and compare object-for-object with the fixture.
	fetched := HandleUploadPack(s, fixtureOwner, "pushy", fetchV2Body([]string{oidC3, oidTag1}, nil, nil, nil))
	lines, pack := parseV2Fetch(t, fetched.Body)
	if len(lines) == 0 || lines[0] != "section:acknowledgments" {
		t.Fatalf("unexpected fetch sections: %v", lines)
	}
	got := readPackObjects(t, pack)
	fixture := fixtureObjects(t)
	if len(got) != len(fixture) {
		t.Fatalf("fetched %d objects, want %d", len(got), len(fixture))
	}
	for oid, want := range fixture {
		have, ok := got[oid]
		if !ok {
			t.Fatalf("fetched pack missing %s", oid)
		}
		if have.Type != want.Type || !bytes.Equal(have.Data, want.Data) {
			t.Fatalf("object %s differs", oid)
		}
	}
	assertRefs(t, repo, map[string]string{"refs/heads/main": oidC3, "refs/tags/v1": oidTag1})
}

// ---------------------------------------------------------------------------------------------
// pure helpers (no store dependency — these run even before the store lands)
// ---------------------------------------------------------------------------------------------

func TestObjectIDOf(t *testing.T) {
	if got := ObjectIDOf("blob", []byte("hello world\n")); got != oidBlob1 {
		t.Fatalf("ObjectIDOf blob: got %s, want %s", got, oidBlob1)
	}
	if got := ObjectIDOf("commit", buildCommit(oidTree1, nil, "first commit\n")); got != oidC1 {
		t.Fatalf("ObjectIDOf commit: got %s, want %s", got, oidC1)
	}
}

func TestSidebandPack(t *testing.T) {
	// Empty pack -> one band-1 packet carrying just the band byte.
	empty := sidebandPack(nil)
	packets, _ := decodeTestPktLines(empty)
	if len(packets) != 1 || packets[0].kind != "data" || len(packets[0].payload) != 1 || packets[0].payload[0] != 1 {
		t.Fatalf("empty sideband pack: %+v", packets)
	}
	// A large pack splits into <=65520-byte pkt-lines, all band 1.
	big := bytes.Repeat([]byte("x"), 200000)
	framed := sidebandPack(big)
	packets, _ = decodeTestPktLines(framed)
	var reassembled []byte
	for _, p := range packets {
		if p.kind != "data" {
			t.Fatalf("unexpected packet kind %s", p.kind)
		}
		if len(p.payload) > 65516 {
			t.Fatalf("sideband packet payload %d bytes, over 65516", len(p.payload))
		}
		if p.payload[0] != 1 {
			t.Fatalf("sideband band %d, want 1", p.payload[0])
		}
		reassembled = append(reassembled, p.payload[1:]...)
	}
	if !bytes.Equal(reassembled, big) {
		t.Fatal("sideband round-trip mismatch")
	}
	if len(packets) != 4 { // ceil(200000/65515)
		t.Fatalf("expected 4 sideband packets, got %d", len(packets))
	}
}

func TestParseJSInt(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"2", 2, true},
		{"2x", 2, true},
		{"  12 ", 12, true},
		{"0x10", 0, true},
		{"", 0, false},
		{"abc", 0, false},
		{"-3", -3, true},
		{"+7", 7, true},
	}
	for _, c := range cases {
		got, ok := parseJSInt(c.in)
		if got != c.want || ok != c.ok {
			t.Fatalf("parseJSInt(%q) = (%d, %v), want (%d, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestFetchCacheKeyCollision is the regression test for the GPT review-2
// fetch-cache collision: the key must distinguish haves ["tip","other"]
// from ["tip,other"], and likewise for wants/shallow.
func TestFetchCacheKeyCollision(t *testing.T) {
	repo := &store.Repo{Objects: map[string]store.GitObject{}, Refs: map[string]string{}}
	a := &v2FetchArgs{wants: []string{"w"}, haves: []string{"tip", "other"}, shallow: []string{}, depth: 0}
	b := &v2FetchArgs{wants: []string{"w"}, haves: []string{"tip,other"}, shallow: []string{}, depth: 0}
	if fetchCacheKey(repo, a) == fetchCacheKey(repo, b) {
		t.Fatal("fetch cache key collides for haves [tip,other] vs [tip,other]")
	}
	c := &v2FetchArgs{wants: []string{"w"}, haves: []string{"tip", "other"}, shallow: []string{}, depth: 0}
	if fetchCacheKey(repo, a) != fetchCacheKey(repo, c) {
		t.Fatal("identical fetch args key differently")
	}
	d := &v2FetchArgs{wants: []string{"w"}, haves: []string{"tip", "other"}, shallow: []string{}, depth: 1}
	if fetchCacheKey(repo, a) == fetchCacheKey(repo, d) {
		t.Fatal("fetch cache key ignores depth")
	}
	e := &v2FetchArgs{wants: []string{"w"}, haves: []string{"tip", "other"}, shallow: []string{"x"}, depth: 0}
	if fetchCacheKey(repo, a) == fetchCacheKey(repo, e) {
		t.Fatal("fetch cache key ignores shallow")
	}
}
