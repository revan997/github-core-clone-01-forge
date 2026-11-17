// Package git is a faithful Go port of the Node forge's git.ts: the smart-HTTP
// git surface — info/refs (v0 + v2), upload-pack (v2 + v0 fetch negotiation)
// and receive-pack. The Node spec is the ground truth; this port reproduces
// its response bytes, status codes and error strings exactly.
//
// Determinism: no wall clock, no randomness. Every response is a pure
// function of the repo state plus the request. Where the spec iterates a
// Map (insertion-ordered), this code iterates an explicitly sorted or
// insertion-ordered structure — Go maps randomize iteration, so no map
// iteration order ever reaches the wire.
package git

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"forge/internal/store"
)

// GitResponse is one smart-HTTP answer.
type GitResponse struct {
	Status      int
	ContentType string
	Body        []byte
}

const agent = "makefun-forge/1.0"

const uploadPackCaps = "symref=HEAD:refs/heads/main agent=makefun-forge/1.0 object-format=sha1 multi_ack side-band-64k thin-pack ofs-delta shallow deepen-since deepen-not filter no-progress include-tag allow-tip-sha1-in-want allow-reachable-sha1-in-want"

const receivePackCaps = "report-status side-band-64k delete-refs agent=makefun-forge/1.0 object-format=sha1"

func jsonResponse(status int, value any) GitResponse {
	body, _ := json.Marshal(value)
	return GitResponse{Status: status, ContentType: "application/json; charset=utf-8", Body: body}
}

func textResponse(status int, text string) GitResponse {
	return GitResponse{Status: status, ContentType: "text/plain; charset=utf-8", Body: []byte(text)}
}

// requireRepo returns the repo, or a non-nil error response (404 JSON) when
// the repo does not exist. Mirrors git.ts requireRepo.
func requireRepo(s *store.Store, owner, repoName string) (*store.Repo, *GitResponse) {
	repo := s.GetRepo(owner, repoName)
	if repo == nil {
		resp := jsonResponse(404, map[string]string{"message": "Not Found"})
		return nil, &resp
	}
	return repo, nil
}

// namedRef is one ref, name-sorted wherever it reaches the wire.
type namedRef struct {
	name string
	oid  string
}

// sortedRefs returns all refs of a repo, sorted by name.
func sortedRefs(repo *store.Repo) []namedRef {
	out := make([]namedRef, 0, len(repo.Refs))
	for name, oid := range repo.Refs {
		out = append(out, namedRef{name: name, oid: oid})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// sortedRefEntries returns the repo's refs as sorted [name, oid] pairs — the
// canonical encoding used by the cache keys.
func sortedRefEntries(repo *store.Repo) [][2]string {
	refs := sortedRefs(repo)
	out := make([][2]string, len(refs))
	for i, r := range refs {
		out[i] = [2]string{r.name, r.oid}
	}
	return out
}

// peelToCommit follows a tag object to its commit (or returns the oid itself
// when it is a commit). "" means "does not peel to a commit". Mirrors git.ts
// peelToCommit, including the 8-step chain cap.
func peelToCommit(repo *store.Repo, oid string) string {
	current := oid
	for i := 0; i < 8; i++ {
		object, ok := repo.Objects[current]
		if !ok {
			return ""
		}
		if object.Type == store.ObjectCommit {
			return current
		}
		if object.Type == store.ObjectTag {
			current = store.ParseTag(object.Data).Object
			continue
		}
		return ""
	}
	return ""
}

func hasObject(repo *store.Repo, oid string) bool {
	_, ok := repo.Objects[oid]
	return ok
}

func containsStr(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// sortedKeys returns a map's keys in sorted order (deterministic iteration).
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// cacheKeyBuilder builds collision-free cache keys from length-prefixed
// fields: every string is written as "<len>:<bytes>|", every int as
// "<digits>|", and every list as its count followed by its elements, so no
// value can alias a different split of values — ["tip","other"] and
// ["tip,other"] key differently, as do ["A","B"] and ["A,B"] (the GPT
// review-2 fetch-cache collision). It is injective over the fixed field
// sequence of each call site, and far cheaper than encoding/json: no
// reflection, a single strings.Builder allocation.
type cacheKeyBuilder struct {
	sb strings.Builder
}

func (k *cacheKeyBuilder) str(s string) {
	k.sb.WriteString(strconv.Itoa(len(s)))
	k.sb.WriteByte(':')
	k.sb.WriteString(s)
	k.sb.WriteByte('|')
}

func (k *cacheKeyBuilder) num(n int) {
	k.sb.WriteString(strconv.Itoa(n))
	k.sb.WriteByte('|')
}

func (k *cacheKeyBuilder) strs(ss []string) {
	k.num(len(ss))
	for _, s := range ss {
		k.str(s)
	}
}

func (k *cacheKeyBuilder) refs(refs [][2]string) {
	k.num(len(refs))
	for _, r := range refs {
		k.str(r[0])
		k.str(r[1])
	}
}

// v0CacheKey is the collision-free key for a v0 advertisement: service plus
// the repo's sorted refs fingerprint.
func v0CacheKey(service string, repo *store.Repo) string {
	var k cacheKeyBuilder
	k.str(service)
	k.refs(sortedRefEntries(repo))
	return k.sb.String()
}

// ---------------------------------------------------------------------------------------------
// per-repo bounded caches (v0 advertisements + v2 fetch responses), mirroring the
// spec's WeakMap<Repo, Map<string, Buffer>> caches (32 entries per repo, FIFO
// eviction of the oldest-inserted key; a hit does not refresh position).
// Go has no WeakMap: entries are keyed by repo pointer and live for the
// process. Repos are few and each cache holds at most 32 response buffers.
// ---------------------------------------------------------------------------------------------

type boundedCache struct {
	entries map[string][]byte
	order   []string
}

func newBoundedCache() *boundedCache {
	return &boundedCache{entries: make(map[string][]byte)}
}

func (c *boundedCache) get(key string) ([]byte, bool) {
	body, ok := c.entries[key]
	return body, ok
}

func (c *boundedCache) put(key string, value []byte) {
	if _, ok := c.entries[key]; !ok {
		if len(c.entries) >= 32 {
			oldest := c.order[0]
			delete(c.entries, oldest)
			c.order = c.order[1:]
		}
		c.order = append(c.order, key)
	}
	c.entries[key] = value
}

var gitCacheMu sync.Mutex
var v0AdvertisementCaches = map[*store.Repo]*boundedCache{}
var fetchResponseCaches = map[*store.Repo]*boundedCache{}

func cacheFor(caches map[*store.Repo]*boundedCache, repo *store.Repo) *boundedCache {
	c, ok := caches[repo]
	if !ok {
		c = newBoundedCache()
		caches[repo] = c
	}
	return c
}

// ---------------------------------------------------------------------------------------------
// info/refs
// ---------------------------------------------------------------------------------------------

func buildV0Advertisement(repo *store.Repo, service string) []byte {
	caps := receivePackCaps
	if service == "git-upload-pack" {
		caps = uploadPackCaps
	}
	var parts [][]byte
	parts = append(parts, store.PktLineString("# service="+service+"\n"), store.PktFlush())
	refs := sortedRefs(repo)
	headTip, hasHead := repo.Refs["refs/heads/main"]
	if len(refs) == 0 {
		parts = append(parts, store.PktLineString(store.ZeroOID+" capabilities^{}\x00"+caps+"\n"))
	} else {
		first := make([]namedRef, 0, len(refs)+1)
		if hasHead {
			first = append(first, namedRef{name: "HEAD", oid: headTip})
		}
		first = append(first, refs...)
		for index, ref := range first {
			suffix := ""
			if index == 0 {
				suffix = "\x00" + caps
			}
			parts = append(parts, store.PktLineString(ref.oid+" "+ref.name+suffix+"\n"))
			if object, ok := repo.Objects[ref.oid]; ok && object.Type == store.ObjectTag && strings.HasPrefix(ref.name, "refs/tags/") {
				target := store.ParseTag(object.Data).Object
				parts = append(parts, store.PktLineString(target+" "+ref.name+"^{}\n"))
			}
		}
	}
	parts = append(parts, store.PktFlush())
	return bytes.Join(parts, nil)
}

func v0Advertisement(repo *store.Repo, service string) []byte {
	key := v0CacheKey(service, repo)
	gitCacheMu.Lock()
	defer gitCacheMu.Unlock()
	cache := cacheFor(v0AdvertisementCaches, repo)
	if hit, ok := cache.get(key); ok {
		return hit
	}
	body := buildV0Advertisement(repo, service)
	cache.put(key, body)
	return body
}

// The v2 advertisement is a fixed constant: build it once, not per request.
// Built lazily (not at package init) so importing this package never touches
// the store.
var v2AdvertisementOnce sync.Once
var v2AdvertisementBytes []byte

func v2Advertisement() []byte {
	v2AdvertisementOnce.Do(func() {
		v2AdvertisementBytes = bytes.Join([][]byte{
			store.PktLineString("version 2\n"),
			store.PktLineString("agent=" + agent + "\n"),
			store.PktLineString("ls-refs=unborn\n"),
			store.PktLineString("fetch=shallow wait-for-done\n"),
			store.PktLineString("server-option\n"),
			store.PktLineString("object-format=sha1\n"),
			store.PktFlush(),
		}, nil)
	})
	return v2AdvertisementBytes
}

// HandleInfoRefs answers GET /<owner>/<repo>.git/info/refs?service=... — v2
// when the request carries `Git-Protocol: version=2` for upload-pack,
// otherwise v0. Receive-pack info/refs is always v0.
func HandleInfoRefs(s *store.Store, owner, repoName, service string, gitProtocol string) GitResponse {
	repo, errResp := requireRepo(s, owner, repoName)
	if errResp != nil {
		return *errResp
	}
	wantsV2 := service == "git-upload-pack" && strings.Contains(gitProtocol, "version=2")
	var body []byte
	if wantsV2 {
		body = v2Advertisement()
	} else {
		body = v0Advertisement(repo, service)
	}
	return GitResponse{Status: 200, ContentType: "application/x-" + service + "-advertisement", Body: body}
}

// ObjectIDOf is the SHA-1 of an object the entry stores. Re-exported for the
// REST layer.
func ObjectIDOf(typ store.ObjectType, data []byte) string {
	return store.HashObject(typ, data)
}

// invalidRefChars reports whether a ref name contains a character the spec's
// receive-pack validation rejects: JS /[\s\u0000-\u001f]/.
func invalidRefChars(ref string) bool {
	for _, r := range ref {
		if r < 0x20 || unicode.IsSpace(r) || r == '\uFEFF' {
			return true
		}
	}
	return false
}
