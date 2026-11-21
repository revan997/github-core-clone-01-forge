// Package store is a faithful Go port of the Node forge's store.ts: git objects,
// packs, pkt-lines, repos, issues, pull requests, and the deterministic clock.
//
// CONTRACT (see forge-go/README.md): no wall clock, no randomness. Every timestamp
// derives from a fixed epoch plus a per-process sequence, so two runs on the same
// instance export byte-identical state. Store methods assume the caller (the app
// dispatcher) holds Mu for the whole request — do NOT lock inside methods.
package store

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"forge/internal/scenario"
)

const ZeroOID = "0000000000000000000000000000000000000000"

// TimeBaseMs is Date.UTC(2025, 0, 1) — the deterministic clock epoch.
const TimeBaseMs = int64(1735689600000)

// CommitEpoch is the base for commit timestamps: CommitEpoch + repo.commitCounter.
const CommitEpoch = int64(1700000000)

type ObjectType string

const (
	ObjectCommit ObjectType = "commit"
	ObjectTree   ObjectType = "tree"
	ObjectBlob   ObjectType = "blob"
	ObjectTag    ObjectType = "tag"
)

type GitObject struct {
	Type ObjectType
	Data []byte
}

func IsObjectID(value string) bool {
	if len(value) != 40 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func HashObject(typ ObjectType, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", string(typ), len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// appendPktLen appends the 4-hex-digit pkt-line length prefix for length.
// Lengths above 0xffff cannot be expressed in 4 hex digits; those fall back
// to fmt.Sprintf, exactly as before (no caller passes such a payload —
// sideband chunks cap at 65515 — but the fallback keeps the behavior
// identical if one ever does).
func appendPktLen(out []byte, length int) []byte {
	if length > 0xffff {
		return append(out, fmt.Sprintf("%04x", length)...)
	}
	const hexDigits = "0123456789abcdef"
	return append(out, hexDigits[(length>>12)&0xf], hexDigits[(length>>8)&0xf], hexDigits[(length>>4)&0xf], hexDigits[length&0xf])
}

// PktLine encodes one pkt-line frame (4-hex-digit length prefix, payload included).
func PktLine(payload []byte) []byte {
	length := len(payload) + 4
	out := make([]byte, 0, length)
	out = appendPktLen(out, length)
	return append(out, payload...)
}

// PktLineString is PktLine for a UTF-8 string payload.
func PktLineString(payload string) []byte {
	length := len(payload) + 4
	out := make([]byte, 0, length)
	out = appendPktLen(out, length)
	return append(out, payload...)
}

func PktFlush() []byte { return []byte("0000") }

func PktDelim() []byte { return []byte("0001") }

type PktPacket struct {
	Kind    string // "data" | "flush" | "delim" | "response-end"
	Payload []byte
}

// DecodePktLines decodes pkt-lines; the returned offset is the byte just past the
// last packet (start of a pack).
func DecodePktLines(body []byte) (packets []PktPacket, offset int) {
	for offset+4 <= len(body) {
		length64, err := strconv.ParseInt(string(body[offset:offset+4]), 16, 32)
		if err != nil {
			break
		}
		length := int(length64)
		switch length {
		case 0:
			packets = append(packets, PktPacket{Kind: "flush", Payload: []byte{}})
			offset += 4
			continue
		case 1:
			packets = append(packets, PktPacket{Kind: "delim", Payload: []byte{}})
			offset += 4
			continue
		case 2:
			packets = append(packets, PktPacket{Kind: "response-end", Payload: []byte{}})
			offset += 4
			continue
		}
		if offset+length > len(body) {
			break
		}
		// Lengths 3 (and any other < 4) have no payload bytes in Node's
		// subarray semantics (start > end yields an empty slice).
		var payload []byte
		if length > 4 {
			payload = body[offset+4 : offset+length]
		} else {
			payload = []byte{}
		}
		packets = append(packets, PktPacket{Kind: "data", Payload: payload})
		offset += length
	}
	return packets, offset
}

// FindZlibEnd inflates the zlib stream at buffer[start:] and returns the offset
// just past the stream plus the inflated data. Errors on a corrupt stream.
//
// The zlib/flate stack buffers through bufio, which reads ahead past the end
// of the stream — so the consumed-input count is derived, not guessed: the
// underlying reader is a bytes.Reader behind a bufio.Reader, and bytes the
// bufio layer read from it, minus bytes still buffered unread, is exactly
// what flate consumed (flate stops at the stream end, so anything buffered
// past it is trailing pack data, never stream data). An earlier version fed
// the decoder one byte per Read call to make the count trivially observable;
// that cost ~3.3x the inflate time on a 1 KiB stream (median 40.6µs vs
// 12.3µs in-process A/B, identical ends/data for 0..100 KiB payloads, with
// and without a prefix offset). Defense-in-depth (as in the TS port): never
// trust the count blindly — an invalid value must fail loudly, never
// silently guess an end and corrupt every subsequent object in the pack.
func FindZlibEnd(buffer []byte, start int) (end int, data []byte, err error) {
	if start < 0 || start > len(buffer) {
		return 0, nil, errors.New("could not find the end of a zlib stream")
	}
	// One inflate: zlib itself stops at the stream end and validates the
	// trailing Adler-32 checksum during inflation, throwing on a corrupt or
	// truncated stream — exactly the error readPack surfaces as an unpack
	// failure.
	underlying := bytes.NewReader(buffer[start:])
	buf := bufio.NewReader(underlying)
	zr, err := zlib.NewReader(buf)
	if err != nil {
		return 0, nil, err
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		return 0, nil, err
	}
	consumed := (len(buffer) - start) - underlying.Len() - buf.Buffered()
	if consumed <= 0 || consumed > len(buffer)-start {
		return 0, nil, errors.New("could not find the end of a zlib stream")
	}
	return start + consumed, out, nil
}

type ParsedCommit struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func ParseCommit(data []byte) ParsedCommit {
	text := string(data)
	blank := strings.Index(text, "\n\n")
	var header, message string
	if blank >= 0 {
		header = text[:blank]
		message = text[blank+2:]
	} else {
		header = text
		message = ""
	}
	parsed := ParsedCommit{Message: message}
	for _, line := range strings.Split(header, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			parsed.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			parsed.Parents = append(parsed.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			parsed.Author = line[7:]
		case strings.HasPrefix(line, "committer "):
			parsed.Committer = line[10:]
		}
	}
	return parsed
}

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

func ParseTree(data []byte) []TreeEntry {
	entries := []TreeEntry{}
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], 0x20)
		if space < 0 {
			break
		}
		space += offset
		nul := bytes.IndexByte(data[space:], 0x00)
		if nul < 0 {
			break
		}
		nul += space
		if nul+21 > len(data) {
			break
		}
		entries = append(entries, TreeEntry{
			Mode: string(data[offset:space]),
			Name: string(data[space+1 : nul]),
			OID:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		offset = nul + 21
	}
	return entries
}

type ParsedTag struct {
	Object string
	Type   string
	Tag    string
}

func ParseTag(data []byte) ParsedTag {
	text := string(data)
	blank := strings.Index(text, "\n\n")
	header := text
	if blank >= 0 {
		header = text[:blank]
	}
	parsed := ParsedTag{}
	for _, line := range strings.Split(header, "\n") {
		switch {
		case strings.HasPrefix(line, "object "):
			parsed.Object = strings.TrimSpace(line[7:])
		case strings.HasPrefix(line, "type "):
			parsed.Type = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "tag "):
			parsed.Tag = strings.TrimSpace(line[4:])
		}
	}
	return parsed
}

// BuildTreeBytes serializes tree entries in git order (directories compare as if
// their name ends in '/').
func BuildTreeBytes(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		aKey := sorted[i].Name
		if sorted[i].Mode == "40000" {
			aKey += "/"
		}
		bKey := sorted[j].Name
		if sorted[j].Mode == "40000" {
			bKey += "/"
		}
		return aKey < bKey
	})
	var buf bytes.Buffer
	for _, entry := range sorted {
		fmt.Fprintf(&buf, "%s %s\x00", entry.Mode, entry.Name)
		raw, err := hex.DecodeString(entry.OID)
		if err == nil {
			buf.Write(raw)
		}
	}
	return buf.Bytes()
}

// CollectReachable returns all objects reachable from the roots (trees walked
// recursively, commits via parents).
func CollectReachable(roots []string, get func(oid string) *GitObject) map[string]bool {
	out := map[string]bool{}
	stack := append([]string{}, roots...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if out[oid] {
			continue
		}
		object := get(oid)
		if object == nil {
			continue
		}
		out[oid] = true
		switch object.Type {
		case ObjectCommit:
			commit := ParseCommit(object.Data)
			stack = append(stack, commit.Tree)
			stack = append(stack, commit.Parents...)
		case ObjectTree:
			for _, entry := range ParseTree(object.Data) {
				stack = append(stack, entry.OID)
			}
		case ObjectTag:
			stack = append(stack, ParseTag(object.Data).Object)
		}
	}
	return out
}

type ReadPackResult struct {
	Objects    map[string]GitObject
	ChecksumOK bool
}

var packTypeByName = map[ObjectType]int{
	ObjectCommit: 1,
	ObjectTree:   2,
	ObjectBlob:   3,
	ObjectTag:    4,
}

var packNameByType = map[int]ObjectType{
	1: ObjectCommit,
	2: ObjectTree,
	3: ObjectBlob,
	4: ObjectTag,
}

func applyDelta(base, delta []byte) ([]byte, error) {
	offset := 0
	// Reads beyond the delta end are corrupt input: Node would read
	// `undefined` (coercing to 0) here, but failing loudly is safer than
	// silently producing a wrong object or panicking; valid deltas never
	// hit this path.
	readByte := func() (byte, error) {
		if offset >= len(delta) {
			return 0, errors.New("delta truncated")
		}
		b := delta[offset]
		offset++
		return b, nil
	}
	readVarint := func() (int, error) {
		value := 0
		shift := uint(0)
		for {
			if offset >= len(delta) {
				// Mirrors the TS behavior: an out-of-range byte is undefined,
				// (undefined & 0x80) === 0, so the varint terminates.
				return value, nil
			}
			b, err := readByte()
			if err != nil {
				return 0, err
			}
			value |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				return value, nil
			}
		}
	}
	if _, err := readVarint(); err != nil { // base size (trusted: the result is checked against the delta's own size)
		return nil, err
	}
	resultSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	var chunks [][]byte
	produced := 0
	for offset < len(delta) {
		op, err := readByte()
		if err != nil {
			return nil, err
		}
		if op&0x80 != 0 {
			copyOffset := 0
			copySize := 0
			if op&0x01 != 0 {
				b, err := readByte()
				if err != nil {
					return nil, err
				}
				copyOffset = int(b)
			}
			if op&0x02 != 0 {
				b, err := readByte()
				if err != nil {
					return nil, err
				}
				copyOffset |= int(b) << 8
			}
			if op&0x04 != 0 {
				b, err := readByte()
				if err != nil {
					return nil, err
				}
				copyOffset |= int(b) << 16
			}
			if op&0x08 != 0 {
				b, err := readByte()
				if err != nil {
					return nil, err
				}
				copyOffset |= int(b) << 24
			}
			if op&0x10 != 0 {
				b, err := readByte()
				if err != nil {
					return nil, err
				}
				copySize = int(b)
			}
			if op&0x20 != 0 {
				b, err := readByte()
				if err != nil {
					return nil, err
				}
				copySize |= int(b) << 8
			}
			if op&0x40 != 0 {
				b, err := readByte()
				if err != nil {
					return nil, err
				}
				copySize |= int(b) << 16
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			// Node's Buffer.subarray clamps out-of-range ends; mirror that
			// (the final size check below still rejects the delta).
			start := copyOffset
			if start > len(base) {
				start = len(base)
			}
			end := copyOffset + copySize
			if end > len(base) {
				end = len(base)
			}
			chunks = append(chunks, base[start:end])
			produced += copySize
		} else if op != 0 {
			n := int(op)
			end := offset + n
			if end > len(delta) {
				end = len(delta)
			}
			chunks = append(chunks, delta[offset:end])
			offset += n
			produced += n
		} else {
			return nil, errors.New("invalid delta opcode 0")
		}
	}
	var result bytes.Buffer
	for _, c := range chunks {
		result.Write(c)
	}
	if result.Len() != resultSize || produced != resultSize {
		return nil, errors.New("delta result size mismatch")
	}
	return result.Bytes(), nil
}

// ReadPack parses a pack file (version 2 or 3, plain objects + OFS/REF deltas).
func ReadPack(pack []byte) (ReadPackResult, error) {
	if len(pack) < 12 || string(pack[0:4]) != "PACK" {
		return ReadPackResult{}, errors.New("not a packfile")
	}
	version := int(pack[4])<<24 | int(pack[5])<<16 | int(pack[6])<<8 | int(pack[7])
	if version != 2 && version != 3 {
		return ReadPackResult{}, fmt.Errorf("unsupported pack version %d", version)
	}
	count := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	objects := make(map[string]GitObject)
	type ofsBase struct {
		oid  string
		typ  ObjectType
		data []byte
	}
	ofsBases := make(map[int]ofsBase)
	offset := 12
	for i := 0; i < count; i++ {
		if offset >= len(pack) {
			return ReadPackResult{}, errors.New("unexpected end of packfile")
		}
		start := offset
		b := pack[offset]
		offset++
		typeNum := int(b>>4) & 0x7
		size := int(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			if offset >= len(pack) {
				return ReadPackResult{}, errors.New("unexpected end of packfile")
			}
			b = pack[offset]
			offset++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		var baseOID string
		var baseOffset *int
		if typeNum == 6 {
			if offset >= len(pack) {
				return ReadPackResult{}, errors.New("unexpected end of packfile")
			}
			b2 := pack[offset]
			offset++
			ofs := int(b2 & 0x7f)
			for b2&0x80 != 0 {
				if offset >= len(pack) {
					return ReadPackResult{}, errors.New("unexpected end of packfile")
				}
				b2 = pack[offset]
				offset++
				ofs = ((ofs + 1) << 7) | int(b2&0x7f)
			}
			bo := start - ofs
			baseOffset = &bo
		} else if typeNum == 7 {
			if offset+20 > len(pack) {
				return ReadPackResult{}, errors.New("unexpected end of packfile")
			}
			baseOID = hex.EncodeToString(pack[offset : offset+20])
			offset += 20
		}
		end, inflated, err := FindZlibEnd(pack, offset)
		if err != nil {
			return ReadPackResult{}, err
		}
		if len(inflated) != size {
			return ReadPackResult{}, errors.New("pack object size mismatch")
		}
		offset = end
		if typeNum >= 1 && typeNum <= 4 {
			typ := packNameByType[typeNum]
			oid := HashObject(typ, inflated)
			object := GitObject{Type: typ, Data: append([]byte{}, inflated...)}
			objects[oid] = object
			ofsBases[start] = ofsBase{oid: oid, typ: typ, data: object.Data}
		} else if typeNum == 6 || typeNum == 7 {
			var base *ofsBase
			if baseOffset != nil {
				if b, ok := ofsBases[*baseOffset]; ok {
					bb := b
					base = &bb
				}
			} else if baseOID != "" {
				if obj, ok := objects[baseOID]; ok {
					base = &ofsBase{oid: baseOID, typ: obj.Type, data: obj.Data}
				}
			}
			if base == nil {
				return ReadPackResult{}, errors.New("delta base not found")
			}
			data, err := applyDelta(base.data, inflated)
			if err != nil {
				return ReadPackResult{}, err
			}
			oid := HashObject(base.typ, data)
			object := GitObject{Type: base.typ, Data: data}
			objects[oid] = object
			ofsBases[start] = ofsBase{oid: oid, typ: base.typ, data: data}
		} else {
			return ReadPackResult{}, fmt.Errorf("unsupported pack object type %d", typeNum)
		}
	}
	checksumOK := false
	if len(pack) >= 20 {
		trailer := hex.EncodeToString(pack[len(pack)-20:])
		sum := sha1.Sum(pack[:len(pack)-20])
		checksumOK = trailer == hex.EncodeToString(sum[:])
	}
	return ReadPackResult{Objects: objects, ChecksumOK: checksumOK}, nil
}

// WritePack serializes objects as a version-2 pack (plain objects only, oid-sorted).
func WritePack(objects []GitObject) []byte {
	type keyed struct {
		oid    string
		object GitObject
	}
	keyedObjects := make([]keyed, len(objects))
	for i, object := range objects {
		keyedObjects[i] = keyed{oid: HashObject(object.Type, object.Data), object: object}
	}
	sort.Slice(keyedObjects, func(i, j int) bool { return keyedObjects[i].oid < keyedObjects[j].oid })
	var parts bytes.Buffer
	header := make([]byte, 12)
	copy(header[0:4], "PACK")
	header[4] = 0
	header[5] = 0
	header[6] = 0
	header[7] = 2
	n := len(keyedObjects)
	header[8] = byte(n >> 24)
	header[9] = byte(n >> 16)
	header[10] = byte(n >> 8)
	header[11] = byte(n)
	parts.Write(header)
	for _, k := range keyedObjects {
		typeNum := packTypeByName[k.object.Type]
		size := len(k.object.Data)
		headerBytes := []byte{byte((typeNum << 4) | (size & 0x0f))}
		remaining := size >> 4
		for remaining > 0 {
			headerBytes[len(headerBytes)-1] |= 0x80
			headerBytes = append(headerBytes, byte(remaining&0x7f))
			remaining >>= 7
		}
		parts.Write(headerBytes)
		var compressed bytes.Buffer
		zw := zlib.NewWriter(&compressed)
		zw.Write(k.object.Data)
		zw.Close()
		parts.Write(compressed.Bytes())
	}
	withoutTrailer := parts.Bytes()
	trailer := sha1.Sum(withoutTrailer)
	out := make([]byte, 0, len(withoutTrailer)+20)
	out = append(out, withoutTrailer...)
	out = append(out, trailer[:]...)
	return out
}

// GlobalID is base64url of "Kind:part:part" (no padding). Parts are strings
// or ints in every call site; other types fall back to fmt.Sprint, exactly
// as before.
func GlobalID(kind string, parts ...any) string {
	b := make([]byte, 0, 32)
	b = append(b, kind...)
	for _, p := range parts {
		b = append(b, ':')
		switch v := p.(type) {
		case string:
			b = append(b, v...)
		case int:
			b = strconv.AppendInt(b, int64(v), 10)
		case int64:
			b = strconv.AppendInt(b, v, 10)
		default:
			b = append(b, fmt.Sprint(v)...)
		}
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// ParseGlobalID decodes a global id; ok is false when it is not one.
// Node's base64url decoder also accepts padded input, so try the padded
// encoding as a fallback after the canonical unpadded one.
func ParseGlobalID(id string) (kind string, parts []string, ok bool) {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(id)
		if err != nil {
			return "", nil, false
		}
	}
	segments := strings.Split(string(raw), ":")
	if len(segments) < 2 {
		return "", nil, false
	}
	return segments[0], segments[1:], true
}

type Label struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type Comment struct {
	GlobalID  string `json:"-"`
	DBID      int    `json:"-"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type Issue struct {
	GlobalID  string `json:"-"`
	Number    int    `json:"number"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	State     string `json:"state"` // "open" | "closed"
	Labels    []Label
	Comments  []*Comment
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
	ClosedAt  *string `json:"closed_at"`
	// CachedJSON is the serialized REST shape cache (read workload); cleared by
	// every mutator. Empty means "not cached".
	CachedJSON string `json:"-"`
	// CachedBody is the serialized REST response bytes for CachedJSON (same
	// bytes, no per-request []byte(string) copy); cleared with CachedJSON.
	// The bytes are never mutated after creation.
	CachedBody []byte `json:"-"`
}

type PullRequest struct {
	GlobalID       string  `json:"-"`
	Number         int     `json:"number"`
	Title          string  `json:"title"`
	Body           string  `json:"body"`
	State          string  `json:"state"` // "open" | "closed"
	Merged         bool    `json:"merged"`
	MergeCommitSHA *string `json:"merge_commit_sha"`
	HeadRef        string  `json:"head_ref"`
	BaseRef        string  `json:"base_ref"`
	HeadSHA        string  `json:"head_sha"`
	BaseSHA        string  `json:"base_sha"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
}

// PullFile is one entry of a pull request's file diff.
type PullFile struct {
	Filename string `json:"filename"`
	Status   string `json:"status"` // "added" | "modified" | "removed"
}

type Repo struct {
	DBID          int    `json:"-"`
	CreatedSeq    string `json:"-"`
	Owner         string `json:"owner"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	IsPrivate     bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	Objects       map[string]GitObject
	Refs          map[string]string
	Issues        map[int]*Issue
	Pulls         map[int]*PullRequest
	Labels        map[string]Label
	NextNumber    int
	CommitCounter int
}

var DefaultLabels = []Label{
	{Name: "bug", Color: "d73a4a", Description: "Something isn't working"},
	{Name: "enhancement", Color: "a2eeef", Description: "New feature or request"},
	{Name: "documentation", Color: "0075ca", Description: "Improvements or additions to documentation"},
	{Name: "question", Color: "d876e3", Description: "Further information is requested"},
	{Name: "good first issue", Color: "7057ff", Description: "Good for newcomers"},
	{Name: "help wanted", Color: "008672", Description: "Extra attention is needed"},
}

type Store struct {
	// Mu guards the store. It is an RWMutex: the git-upload-pack (clone)
	// path is read-only, so concurrent clones take the read lock and run
	// in parallel; every other request takes the write lock, preserving
	// Node's run-to-completion semantics. Store methods assume the caller
	// (the app dispatcher) holds Mu (read or write) for the whole request.
	Mu sync.RWMutex

	Repos map[string]*Repo

	nextDBID       int
	requestCounter int64
	sequence       int
	nextCommentID  int
}

func NewStore() *Store {
	return &Store{Repos: make(map[string]*Repo), nextDBID: 1, nextCommentID: 1}
}

func (s *Store) NextRequestID() string {
	// Atomic: concurrent clone requests (read lock) each need a unique ID
	// without taking the write lock. The ID format is unchanged.
	n := atomic.AddInt64(&s.requestCounter, 1)
	// Same output as fmt.Sprintf("req-%08d", n): "req-" plus the decimal
	// counter zero-padded to 8 digits (longer counters print in full).
	// Hand-rolled: Sprintf's generic formatting was measurable per request.
	digits := strconv.FormatInt(n, 10)
	if len(digits) >= 8 {
		return "req-" + digits
	}
	var buf [12]byte
	copy(buf[:4], "req-")
	for i := 0; i < 8-len(digits); i++ {
		buf[4+i] = '0'
	}
	copy(buf[4+8-len(digits):], digits)
	return string(buf[:12])
}

// SeqTimestamp is the deterministic ISO timestamp: base + 1s per sequence step.
// It is formatted by hand (civil-from-days) instead of time.Format: same
// bytes, ~5x cheaper, and the only timestamp source in the store.
func (s *Store) SeqTimestamp() string {
	s.sequence++
	return seqTimestampString(s.sequence)
}

// seqTimestampString formats TimeBaseMs + seq seconds as
// "2006-01-02T15:04:05.000Z" (UTC, whole seconds). Verified byte-identical
// to time.Format over the full range in seq_timestamp_test.go.
func seqTimestampString(seq int) string {
	totalSec := TimeBaseMs/1000 + int64(seq)
	days := totalSec / 86400
	rem := totalSec % 86400
	hour := rem / 3600
	min := (rem % 3600) / 60
	sec := rem % 60
	year, month, day := civilFromDays(days)
	var buf [24]byte
	// "YYYY-MM-DDTHH:MM:SS.000Z"
	put4 := func(dst []byte, v int64) {
		dst[0] = byte('0' + v/1000)
		dst[1] = byte('0' + (v/100)%10)
		dst[2] = byte('0' + (v/10)%10)
		dst[3] = byte('0' + v%10)
	}
	put2 := func(dst []byte, v int64) {
		dst[0] = byte('0' + v/10)
		dst[1] = byte('0' + v%10)
	}
	put4(buf[0:4], year)
	buf[4] = '-'
	put2(buf[5:7], month)
	buf[7] = '-'
	put2(buf[8:10], day)
	buf[10] = 'T'
	put2(buf[11:13], hour)
	buf[13] = ':'
	put2(buf[14:16], min)
	buf[16] = ':'
	put2(buf[17:19], sec)
	buf[19] = '.'
	buf[20] = '0'
	buf[21] = '0'
	buf[22] = '0'
	buf[23] = 'Z'
	return string(buf[:])
}

// civilFromDays converts days since the Unix epoch to (year, month, day)
// (Howard Hinnant's civil-from-days algorithm; days >= 0 here).
func civilFromDays(z int64) (year, month, day int64) {
	z += 719468
	era := z / 146097
	doe := z - era*146097                                  // [0, 146096]
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365 // [0, 399]
	y := yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100) // [0, 365]
	mp := (5*doy + 2) / 153                  // [0, 11]
	d := doy - (153*mp+2)/5 + 1              // [1, 31]
	m := mp + 3
	if mp >= 10 {
		m = mp - 9
	}
	if m <= 2 {
		y++
	}
	return y, m, d
}

func repoKey(owner, name string) string { return owner + "/" + name }

func (s *Store) CreateRepo(name string, isPrivate bool, description string) *Repo {
	if _, exists := s.Repos[repoKey(scenario.Viewer.Login, name)]; exists {
		return nil
	}
	dbID := s.nextDBID
	s.nextDBID++
	repo := &Repo{
		DBID:          dbID,
		CreatedSeq:    s.SeqTimestamp(),
		Owner:         scenario.Viewer.Login,
		Name:          name,
		Description:   description,
		IsPrivate:     isPrivate,
		DefaultBranch: "main",
		Objects:       make(map[string]GitObject),
		Refs:          make(map[string]string),
		Issues:        make(map[int]*Issue),
		Pulls:         make(map[int]*PullRequest),
		Labels:        make(map[string]Label),
		NextNumber:    1,
		CommitCounter: 0,
	}
	for _, label := range DefaultLabels {
		repo.Labels[label.Name] = label
	}
	s.Repos[repoKey(repo.Owner, name)] = repo
	return repo
}

func (s *Store) GetRepo(owner, name string) *Repo {
	return s.Repos[repoKey(owner, name)]
}

func (s *Store) ListViewerRepos() []*Repo {
	out := []*Repo{}
	for _, repo := range s.Repos {
		if repo.Owner == scenario.Viewer.Login {
			out = append(out, repo)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DBID < out[j].DBID })
	return out
}

func (s *Store) DeleteRepo(owner, name string) bool {
	key := repoKey(owner, name)
	_, exists := s.Repos[key]
	if exists {
		delete(s.Repos, key)
	}
	return exists
}

func (s *Store) CreateIssue(repo *Repo, title, body string, labels []string) *Issue {
	number := repo.NextNumber
	repo.NextNumber++
	now := s.SeqTimestamp()
	issueLabels := []Label{}
	for _, name := range labels {
		if label, ok := repo.Labels[name]; ok {
			issueLabels = append(issueLabels, label)
		}
	}
	issue := &Issue{
		GlobalID:   GlobalID("Issue", repo.DBID, number),
		Number:     number,
		Title:      title,
		Body:       body,
		State:      "open",
		Labels:     issueLabels,
		Comments:   []*Comment{},
		CreatedAt:  now,
		UpdatedAt:  now,
		ClosedAt:   nil,
		CachedJSON: "",
	}
	repo.Issues[number] = issue
	return issue
}

// TouchIssue clears an issue's cached serialized shape after any mutation.
func (s *Store) TouchIssue(issue *Issue) {
	issue.CachedJSON = ""
	issue.CachedBody = nil
}

func (s *Store) AddComment(repo *Repo, issue *Issue, body string) *Comment {
	now := s.SeqTimestamp()
	comment := &Comment{
		GlobalID:  GlobalID("IssueComment", repo.DBID, issue.Number, s.nextCommentID),
		DBID:      s.nextCommentID,
		Body:      body,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.nextCommentID++
	issue.Comments = append(issue.Comments, comment)
	issue.UpdatedAt = now
	s.TouchIssue(issue)
	return comment
}

// CreatePull returns (pull, "", true) on success or (nil, message, false).
func (s *Store) CreatePull(repo *Repo, title, body, headRef, baseRef string) (*PullRequest, string, bool) {
	headOID, ok := repo.Refs["refs/heads/"+headRef]
	if !ok {
		return nil, "No such head branch: " + headRef, false
	}
	baseOID, ok := repo.Refs["refs/heads/"+baseRef]
	if !ok {
		return nil, "No such base branch: " + baseRef, false
	}
	number := repo.NextNumber
	repo.NextNumber++
	now := s.SeqTimestamp()
	pull := &PullRequest{
		GlobalID:       GlobalID("PullRequest", repo.DBID, number),
		Number:         number,
		Title:          title,
		Body:           body,
		State:          "open",
		Merged:         false,
		MergeCommitSHA: nil,
		HeadRef:        headRef,
		BaseRef:        baseRef,
		HeadSHA:        headOID,
		BaseSHA:        baseOID,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	repo.Pulls[number] = pull
	return pull, "", true
}

// CurrentBaseSha is the base branch's CURRENT tip (a PR's stored BaseSHA is the
// tip at creation time).
func (s *Store) CurrentBaseSha(repo *Repo, pull *PullRequest) string {
	if oid, ok := repo.Refs["refs/heads/"+pull.BaseRef]; ok {
		return oid
	}
	return pull.BaseSHA
}

// CurrentHeadSha is the head branch's CURRENT tip.
func (s *Store) CurrentHeadSha(repo *Repo, pull *PullRequest) string {
	if oid, ok := repo.Refs["refs/heads/"+pull.HeadRef]; ok {
		return oid
	}
	return pull.HeadSHA
}

func (s *Store) PullMergeable(repo *Repo, pull *PullRequest) bool {
	if pull.Merged || pull.State != "open" {
		return false
	}
	headSHA := s.CurrentHeadSha(repo, pull)
	baseSHA := s.CurrentBaseSha(repo, pull)
	head, ok := repo.Objects[headSHA]
	if !ok || head.Type != ObjectCommit {
		return false
	}
	base, ok := repo.Objects[baseSHA]
	if !ok || base.Type != ObjectCommit {
		return false
	}
	return s.IsAncestor(repo, baseSHA, headSHA) || headSHA == baseSHA
}

func (s *Store) PullFiles(repo *Repo, pull *PullRequest) []PullFile {
	headSHA := s.CurrentHeadSha(repo, pull)
	baseSHA := s.CurrentBaseSha(repo, pull)
	head, ok := repo.Objects[headSHA]
	if !ok || head.Type != ObjectCommit {
		return []PullFile{}
	}
	base, ok := repo.Objects[baseSHA]
	if !ok || base.Type != ObjectCommit {
		return []PullFile{}
	}
	headTree := s.FlattenTree(repo, ParseCommit(head.Data).Tree)
	baseTree := s.FlattenTree(repo, ParseCommit(base.Data).Tree)
	files := []PullFile{}
	for path, oid := range headTree {
		baseOID, exists := baseTree[path]
		if !exists {
			files = append(files, PullFile{Filename: path, Status: "added"})
		} else if baseOID != oid {
			files = append(files, PullFile{Filename: path, Status: "modified"})
		}
	}
	for path := range baseTree {
		if _, exists := headTree[path]; !exists {
			files = append(files, PullFile{Filename: path, Status: "removed"})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Filename < files[j].Filename })
	return files
}

func (s *Store) FlattenTree(repo *Repo, treeOID string) map[string]string {
	out := make(map[string]string)
	var walk func(oid, at string)
	walk = func(oid, at string) {
		object, ok := repo.Objects[oid]
		if !ok || object.Type != ObjectTree {
			return
		}
		for _, entry := range ParseTree(object.Data) {
			path := entry.Name
			if at != "" {
				path = at + "/" + entry.Name
			}
			if entry.Mode == "40000" {
				walk(entry.OID, path)
			} else {
				out[path] = entry.OID
			}
		}
	}
	walk(treeOID, "")
	return out
}

func (s *Store) IsAncestor(repo *Repo, ancestor, oid string) bool {
	if ancestor == oid {
		return true
	}
	seen := make(map[string]bool)
	queue := []string{oid}
	for len(queue) > 0 {
		current := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[current] {
			continue
		}
		seen[current] = true
		object, ok := repo.Objects[current]
		if !ok || object.Type != ObjectCommit {
			continue
		}
		for _, parent := range ParseCommit(object.Data).Parents {
			if parent == ancestor {
				return true
			}
			queue = append(queue, parent)
		}
	}
	return false
}

// MergePull returns (sha, "", true) on success or ("", message, false).
func (s *Store) MergePull(repo *Repo, pull *PullRequest) (string, string, bool) {
	if pull.Merged {
		return "", "Pull request already merged", false
	}
	if pull.State != "open" {
		return "", "Pull request is not open", false
	}
	if !s.PullMergeable(repo, pull) {
		return "", "Pull request is not mergeable", false
	}
	headSHA := s.CurrentHeadSha(repo, pull)
	baseSHA := s.CurrentBaseSha(repo, pull)
	head, ok := repo.Objects[headSHA]
	if !ok || head.Type != ObjectCommit {
		return "", "Head commit not found", false
	}
	headTree := ParseCommit(head.Data).Tree
	parents := []string{baseSHA, headSHA}
	if baseSHA == headSHA {
		parents = []string{baseSHA}
	}
	repo.CommitCounter++
	timestamp := CommitEpoch + int64(repo.CommitCounter)
	message := fmt.Sprintf("Merge pull request #%d from %s", pull.Number, pull.HeadRef)
	var sb strings.Builder
	sb.WriteString("tree " + headTree + "\n")
	for _, parent := range parents {
		sb.WriteString("parent " + parent + "\n")
	}
	fmt.Fprintf(&sb, "author %s %d +0000\n", scenario.ForgeIdentity, timestamp)
	fmt.Fprintf(&sb, "committer %s %d +0000\n", scenario.ForgeIdentity, timestamp)
	sb.WriteString("\n" + message)
	commitData := []byte(sb.String())
	sha := HashObject(ObjectCommit, commitData)
	repo.Objects[sha] = GitObject{Type: ObjectCommit, Data: commitData}
	repo.Refs["refs/heads/"+pull.BaseRef] = sha
	pull.Merged = true
	pull.State = "closed"
	pull.MergeCommitSHA = &sha
	pull.UpdatedAt = s.SeqTimestamp()
	return sha, "", true
}

// CreateCommitOnBranch creates or updates a file on a branch (REST contents).
// Returns the new commit sha.
func (s *Store) CreateCommitOnBranch(repo *Repo, branch, path string, content []byte, message string) string {
	ref := "refs/heads/" + branch
	current, hasCurrent := repo.Refs[ref]
	parentTree := make(map[string]string)
	parents := []string{}
	if hasCurrent {
		if object, ok := repo.Objects[current]; ok && object.Type == ObjectCommit {
			parents = append(parents, current)
			for filePath, oid := range s.FlattenTree(repo, ParseCommit(object.Data).Tree) {
				parentTree[filePath] = oid
			}
		}
	}
	blobOID := HashObject(ObjectBlob, content)
	repo.Objects[blobOID] = GitObject{Type: ObjectBlob, Data: append([]byte{}, content...)}
	parentTree[path] = blobOID
	treeOID := s.buildTreeFromFlat(repo, parentTree)
	repo.CommitCounter++
	timestamp := CommitEpoch + int64(repo.CommitCounter)
	var sb strings.Builder
	sb.WriteString("tree " + treeOID + "\n")
	for _, parent := range parents {
		sb.WriteString("parent " + parent + "\n")
	}
	fmt.Fprintf(&sb, "author %s %d +0000\n", scenario.ForgeIdentity, timestamp)
	fmt.Fprintf(&sb, "committer %s %d +0000\n", scenario.ForgeIdentity, timestamp)
	sb.WriteString("\n" + message)
	commitData := []byte(sb.String())
	sha := HashObject(ObjectCommit, commitData)
	repo.Objects[sha] = GitObject{Type: ObjectCommit, Data: commitData}
	repo.Refs[ref] = sha
	return sha
}

func (s *Store) buildTreeFromFlat(repo *Repo, flat map[string]string) string {
	type dirEntry struct {
		mode string
		oid  string
	}
	nested := make(map[string]map[string]dirEntry)
	root := make(map[string]dirEntry)
	nested[""] = root
	for path, oid := range flat {
		parts := strings.Split(path, "/")
		at := ""
		for i := 0; i < len(parts)-1; i++ {
			part := parts[i]
			next := part
			if at != "" {
				next = at + "/" + part
			}
			if _, ok := nested[next]; !ok {
				nested[next] = make(map[string]dirEntry)
			}
			parent := nested[at]
			if _, ok := parent[part]; !ok {
				parent[part] = dirEntry{mode: "40000", oid: ""}
			}
			at = next
		}
		nested[at][parts[len(parts)-1]] = dirEntry{mode: "100644", oid: oid}
	}
	var build func(dir string) string
	build = func(dir string) string {
		entries := []TreeEntry{}
		for name, value := range nested[dir] {
			if value.mode == "40000" {
				child := name
				if dir != "" {
					child = dir + "/" + name
				}
				entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: build(child)})
			} else {
				entries = append(entries, TreeEntry{Mode: value.mode, Name: name, OID: value.oid})
			}
		}
		bytes := BuildTreeBytes(entries)
		oid := HashObject(ObjectTree, bytes)
		repo.Objects[oid] = GitObject{Type: ObjectTree, Data: bytes}
		return oid
	}
	return build("")
}
