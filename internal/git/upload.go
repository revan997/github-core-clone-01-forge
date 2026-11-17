package git

import (
	"bytes"
	"math"
	"strings"

	"forge/internal/store"
)

// ---------------------------------------------------------------------------------------------
// fetch: shared object selection
// ---------------------------------------------------------------------------------------------

type fetchPlan struct {
	// included holds the commits selected (depth-limited when a depth was given).
	included map[string]bool
	// boundary holds the deepest included layer, for the shallow-info section.
	boundary map[string]bool
	// objects to send, in a stable order (writePack re-sorts by oid, so the
	// order never reaches the wire — it is kept deterministic anyway).
	objects []store.GitObject
	// acks holds haves that are ancestors of (or equal to) a want.
	acks []string
}

// repoGetter adapts a repo's object map to the store.CollectReachable lookup
// shape (nil for a missing object, which CollectReachable skips).
func repoGetter(repo *store.Repo) func(string) *store.GitObject {
	return func(id string) *store.GitObject {
		if o, ok := repo.Objects[id]; ok {
			oo := o
			return &oo
		}
		return nil
	}
}

// commitsWithinDepth returns the commits within `depth` edges of the wants
// (depth 1 = the tips only; depth 0 = null in the spec = all history).
// The queue cursor (not shift()) keeps the same FIFO order, depth and
// boundary behavior in linear time.
func commitsWithinDepth(repo *store.Repo, wants []string, depth int) (included map[string]bool, includedOrder []string, boundary map[string]bool) {
	included = map[string]bool{}
	boundary = map[string]bool{}
	type queued struct {
		oid   string
		level int
	}
	queue := make([]queued, 0, len(wants))
	for _, oid := range wants {
		queue = append(queue, queued{oid: oid, level: 1})
	}
	seen := map[string]bool{}
	cursor := 0
	for cursor < len(queue) {
		cur := queue[cursor]
		cursor++
		if seen[cur.oid] {
			continue
		}
		seen[cur.oid] = true
		object, ok := repo.Objects[cur.oid]
		if !ok || object.Type != store.ObjectCommit {
			continue
		}
		included[cur.oid] = true
		includedOrder = append(includedOrder, cur.oid)
		if depth != 0 && cur.level >= depth {
			boundary[cur.oid] = true
			continue
		}
		for _, parent := range store.ParseCommit(object.Data).Parents {
			queue = append(queue, queued{oid: parent, level: cur.level + 1})
		}
	}
	return included, includedOrder, boundary
}

// objectsForCommits returns the objects for a set of commits: each commit plus
// its whole tree, never its parents. Each distinct tree is walked once per
// fetch (treeCache) and its reachable set reused.
func objectsForCommits(repo *store.Repo, commits []string, treeCache map[string]map[string]bool) (map[string]store.GitObject, []string) {
	out := map[string]store.GitObject{}
	var order []string
	add := func(oid string, object store.GitObject) {
		if _, ok := out[oid]; !ok {
			order = append(order, oid)
		}
		out[oid] = object
	}
	for _, oid := range commits {
		commit, ok := repo.Objects[oid]
		if !ok {
			continue
		}
		add(oid, commit)
		treeOID := store.ParseCommit(commit.Data).Tree
		reachable := treeCache[treeOID]
		if reachable == nil {
			reachable = store.CollectReachable([]string{treeOID}, repoGetter(repo))
			treeCache[treeOID] = reachable
		}
		for _, reachableOID := range sortedKeys(reachable) {
			if object, ok := repo.Objects[reachableOID]; ok {
				add(reachableOID, object)
			}
		}
	}
	return out, order
}

func planFetch(s *store.Store, repo *store.Repo, wants, haves, shallow []string, depth int) fetchPlan {
	// A want may be a tag object: send the tag itself, and walk from what it points at.
	type tagObject struct {
		oid    string
		object store.GitObject
	}
	var tagObjects []tagObject
	var commitWants []string
	for _, want := range wants {
		object, ok := repo.Objects[want]
		if !ok {
			continue
		}
		if object.Type == store.ObjectTag {
			tagObjects = append(tagObjects, tagObject{oid: want, object: object})
			if target := peelToCommit(repo, want); target != "" {
				commitWants = append(commitWants, target)
			}
		} else if object.Type == store.ObjectCommit {
			commitWants = append(commitWants, want)
		}
	}
	included, includedOrder, boundary := commitsWithinDepth(repo, commitWants, depth)
	// Request-local cache: each distinct tree's reachable set, walked once.
	treeCache := map[string]map[string]bool{}
	wantedObjects, wantedOrder := objectsForCommits(repo, includedOrder, treeCache)
	for _, tag := range tagObjects {
		if _, ok := wantedObjects[tag.oid]; !ok {
			wantedOrder = append(wantedOrder, tag.oid)
		}
		wantedObjects[tag.oid] = tag.object
	}
	// What the client already holds. A shallow client holds only its boundary
	// commits and their trees — never the history below the boundary — so its
	// haves are counted the same way: each held commit plus its tree, no
	// parents. A full client holds the whole history below its haves.
	isShallow := depth != 0 || len(shallow) > 0
	haveObjects := map[string]store.GitObject{}
	addHeldCommit := func(oid string) {
		object, ok := repo.Objects[oid]
		if !ok {
			return
		}
		if object.Type != store.ObjectCommit {
			haveObjects[oid] = object
			return
		}
		haveObjects[oid] = object
		treeOID := store.ParseCommit(object.Data).Tree
		reachable := treeCache[treeOID]
		if reachable == nil {
			reachable = store.CollectReachable([]string{treeOID}, repoGetter(repo))
			treeCache[treeOID] = reachable
		}
		for _, reachableOID := range sortedKeys(reachable) {
			if found, ok := repo.Objects[reachableOID]; ok {
				haveObjects[reachableOID] = found
			}
		}
	}
	if isShallow {
		for _, oid := range append(append([]string{}, haves...), shallow...) {
			addHeldCommit(oid)
		}
	} else {
		for _, have := range haves {
			object, ok := repo.Objects[have]
			if !ok {
				continue
			}
			if object.Type == store.ObjectCommit {
				for _, reachableOID := range sortedKeys(store.CollectReachable([]string{have}, repoGetter(repo))) {
					if found, ok := repo.Objects[reachableOID]; ok {
						haveObjects[reachableOID] = found
					}
				}
			} else {
				haveObjects[have] = object
			}
		}
	}
	var objects []store.GitObject
	for _, oid := range wantedOrder {
		if _, held := haveObjects[oid]; !held {
			objects = append(objects, wantedObjects[oid])
		}
	}
	var acks []string
	for _, have := range haves {
		if containsStr(wants, have) {
			acks = append(acks, have)
			continue
		}
		for _, want := range wants {
			if s.IsAncestor(repo, have, want) {
				acks = append(acks, have)
				break
			}
		}
	}
	return fetchPlan{included: included, boundary: boundary, objects: objects, acks: acks}
}

// sidebandPack splits pack bytes into sideband-64k packets (band 1), each a
// pkt-line of at most 65520 bytes. An empty pack still sends one band-1 packet.
func sidebandPack(pack []byte) []byte {
	const chunk = 65515
	var parts [][]byte
	for offset := 0; offset < len(pack); offset += chunk {
		end := offset + chunk
		if end > len(pack) {
			end = len(pack)
		}
		payload := make([]byte, 0, end-offset+1)
		payload = append(payload, 1)
		payload = append(payload, pack[offset:end]...)
		parts = append(parts, store.PktLine(payload))
	}
	if len(pack) == 0 {
		parts = append(parts, store.PktLine([]byte{1}))
	}
	return bytes.Join(parts, nil)
}

// shallowInfoLines returns the shallow-info lines for a fetch: new boundary
// commits, plus old ones now complete.
func shallowInfoLines(plan fetchPlan, oldBoundary []string, depth int) []string {
	if depth == 0 {
		return nil
	}
	var lines []string
	for _, oid := range sortedKeys(plan.boundary) {
		lines = append(lines, "shallow "+oid+"\n")
	}
	for _, old := range oldBoundary {
		if !plan.included[old] {
			continue
		}
		if plan.boundary[old] {
			continue
		}
		lines = append(lines, "unshallow "+old+"\n")
	}
	return lines
}

// ---------------------------------------------------------------------------------------------
// upload-pack
// ---------------------------------------------------------------------------------------------

type v2FetchArgs struct {
	wants   []string
	haves   []string
	shallow []string
	depth   int // 0 = null (no depth limit)
}

// parseJSInt parses a leading decimal integer the way JS parseInt(s, 10)
// does: leading whitespace and sign allowed, parsing stops at the first
// non-digit, no digits means failure. Overflows saturate at math.MaxInt.
func parseJSInt(s string) (int, bool) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r' || s[i] == '\v' || s[i] == '\f') {
		i++
	}
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	if i >= len(s) || s[i] < '0' || s[i] > '9' {
		return 0, false
	}
	n := 0
	overflow := false
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		d := int(s[i] - '0')
		if n > (math.MaxInt-d)/10 {
			overflow = true
		} else {
			n = n*10 + d
		}
		i++
	}
	if overflow {
		n = math.MaxInt
	}
	if neg {
		n = -n
	}
	return n, true
}

// stripOneNewline removes a single trailing "\n", like JS .replace(/\n$/, ”).
func stripOneNewline(s string) string {
	return strings.TrimSuffix(s, "\n")
}

func parseV2FetchArgs(packets []store.PktPacket) *v2FetchArgs {
	args := &v2FetchArgs{wants: []string{}, haves: []string{}, shallow: []string{}}
	seenDelim := false
	for _, packet := range packets {
		if packet.Kind == "delim" {
			seenDelim = true
			continue
		}
		if packet.Kind != "data" {
			continue
		}
		line := stripOneNewline(string(packet.Payload))
		if !seenDelim {
			continue
		}
		if strings.HasPrefix(line, "want ") {
			args.wants = append(args.wants, strings.TrimSpace(line[5:]))
		} else if strings.HasPrefix(line, "have ") {
			args.haves = append(args.haves, strings.TrimSpace(line[5:]))
		} else if strings.HasPrefix(line, "shallow ") {
			args.shallow = append(args.shallow, strings.TrimSpace(line[8:]))
		} else if strings.HasPrefix(line, "deepen ") {
			if n, ok := parseJSInt(strings.TrimSpace(line[7:])); ok && n > 0 {
				args.depth = n
			}
		}
	}
	if len(args.wants) == 0 {
		return nil
	}
	return args
}

// fetchCacheKey is the canonical, collision-free encoding of a v2 fetch
// response's inputs. Length-prefixed fields (see cacheKeyBuilder) keep it
// injective: a want, have or shallow value containing ',' can no longer
// collide with two separate values. Depth is always written as an int
// (0 = no limit); field positions are fixed, so 0 never aliases a depth.
func fetchCacheKey(repo *store.Repo, args *v2FetchArgs) string {
	var k cacheKeyBuilder
	k.num(len(repo.Objects))
	k.refs(sortedRefEntries(repo))
	k.strs(args.wants)
	k.strs(args.haves)
	k.strs(args.shallow)
	k.num(args.depth)
	return k.sb.String()
}

func v2FetchResponse(s *store.Store, repo *store.Repo, args *v2FetchArgs) GitResponse {
	for _, want := range args.wants {
		if !store.IsObjectID(want) || !hasObject(repo, want) {
			return textResponse(400, "want "+want+" is not known")
		}
	}
	// The fetch response is a pure function of the repo's objects/refs plus
	// the fetch args, and git objects are immutable and only added (never
	// deleted or mutated). The key embeds the object count and a full refs
	// fingerprint, so any mutation forces a recompute. Bounded to 32 entries
	// per repo. The cached body is never mutated after creation.
	key := fetchCacheKey(repo, args)
	gitCacheMu.Lock()
	cache := cacheFor(fetchResponseCaches, repo)
	if hit, ok := cache.get(key); ok {
		gitCacheMu.Unlock()
		return GitResponse{Status: 200, ContentType: "application/x-git-upload-pack-result", Body: hit}
	}
	gitCacheMu.Unlock()
	plan := planFetch(s, repo, args.wants, args.haves, args.shallow, args.depth)
	var parts [][]byte
	// acknowledgments
	parts = append(parts, store.PktLineString("acknowledgments\n"))
	if len(plan.acks) > 0 {
		for _, ack := range plan.acks {
			parts = append(parts, store.PktLineString("ACK "+ack+"\n"))
		}
		parts = append(parts, store.PktLineString("ready\n"))
	} else {
		parts = append(parts, store.PktLineString("NAK\n"))
	}
	parts = append(parts, store.PktDelim())
	// shallow-info
	shallowLines := shallowInfoLines(plan, args.shallow, args.depth)
	if len(shallowLines) > 0 {
		parts = append(parts, store.PktLineString("shallow-info\n"))
		for _, line := range shallowLines {
			parts = append(parts, store.PktLineString(line))
		}
		parts = append(parts, store.PktDelim())
	}
	// wanted-refs
	parts = append(parts, store.PktLineString("wanted-refs\n"))
	for _, ref := range sortedRefs(repo) {
		if containsStr(args.wants, ref.oid) {
			parts = append(parts, store.PktLineString(ref.oid+" "+ref.name+"\n"))
		}
	}
	parts = append(parts, store.PktDelim())
	// packfile
	parts = append(parts, store.PktLineString("packfile\n"))
	parts = append(parts, sidebandPack(store.WritePack(plan.objects)))
	parts = append(parts, store.PktFlush())
	body := bytes.Join(parts, nil)
	gitCacheMu.Lock()
	cache.put(key, body)
	gitCacheMu.Unlock()
	return GitResponse{Status: 200, ContentType: "application/x-git-upload-pack-result", Body: body}
}

func v2LsRefsResponse(repo *store.Repo, packets []store.PktPacket) GitResponse {
	var peel, symrefs, unborn bool
	prefixes := []string{}
	seenDelim := false
	for _, packet := range packets {
		if packet.Kind == "delim" {
			seenDelim = true
			continue
		}
		if packet.Kind != "data" || !seenDelim {
			continue
		}
		line := stripOneNewline(string(packet.Payload))
		if line == "peel" {
			peel = true
		} else if line == "symrefs" {
			symrefs = true
		} else if line == "unborn" {
			unborn = true
		} else if strings.HasPrefix(line, "ref-prefix ") {
			prefixes = append(prefixes, line[len("ref-prefix "):])
		}
	}
	keep := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return false
	}
	var parts [][]byte
	headTip, hasHead := repo.Refs["refs/heads/main"]
	if len(repo.Refs) == 0 {
		if unborn && keep("HEAD") {
			parts = append(parts, store.PktLineString("unborn HEAD symref-target:refs/heads/main\n"))
		}
	} else {
		if keep("HEAD") && hasHead {
			line := headTip + " HEAD"
			if symrefs {
				line += " symref-target:refs/heads/main"
			}
			parts = append(parts, store.PktLineString(line+"\n"))
		}
		for _, ref := range sortedRefs(repo) {
			if !keep(ref.name) {
				continue
			}
			line := ref.oid + " " + ref.name
			if peel {
				if object, ok := repo.Objects[ref.oid]; ok && object.Type == store.ObjectTag {
					line += " peeled:" + store.ParseTag(object.Data).Object
				}
			}
			parts = append(parts, store.PktLineString(line+"\n"))
		}
	}
	parts = append(parts, store.PktFlush())
	return GitResponse{Status: 200, ContentType: "application/x-git-upload-pack-result", Body: bytes.Join(parts, nil)}
}

func v0FetchResponse(s *store.Store, repo *store.Repo, body []byte) GitResponse {
	packets, _ := store.DecodePktLines(body)
	wants := []string{}
	haves := []string{}
	shallow := []string{}
	depth := 0
	for _, packet := range packets {
		if packet.Kind != "data" {
			continue
		}
		line := stripOneNewline(string(packet.Payload))
		if strings.HasPrefix(line, "want ") {
			rest := strings.Split(line[5:], " ")
			if rest[0] != "" {
				wants = append(wants, rest[0])
			}
		} else if strings.HasPrefix(line, "have ") {
			haves = append(haves, strings.TrimSpace(line[5:]))
		} else if strings.HasPrefix(line, "shallow ") {
			shallow = append(shallow, strings.TrimSpace(line[8:]))
		} else if strings.HasPrefix(line, "deepen ") {
			if n, ok := parseJSInt(strings.TrimSpace(line[7:])); ok && n > 0 {
				depth = n
			}
		}
	}
	if len(wants) == 0 {
		return textResponse(400, "no want lines")
	}
	for _, want := range wants {
		if !store.IsObjectID(want) || !hasObject(repo, want) {
			return textResponse(400, "want "+want+" is not known")
		}
	}
	plan := planFetch(s, repo, wants, haves, shallow, depth)
	var parts [][]byte
	if len(plan.acks) > 0 {
		for _, ack := range plan.acks {
			parts = append(parts, store.PktLineString("ACK "+ack+"\n"))
		}
		parts = append(parts, store.PktLineString("ready\n"))
	} else {
		parts = append(parts, store.PktLineString("NAK\n"))
	}
	for _, line := range shallowInfoLines(plan, shallow, depth) {
		parts = append(parts, store.PktLineString(line))
	}
	parts = append(parts, sidebandPack(store.WritePack(plan.objects)))
	parts = append(parts, store.PktFlush())
	return GitResponse{Status: 200, ContentType: "application/x-git-upload-pack-result", Body: bytes.Join(parts, nil)}
}

// HandleUploadPack answers POST /<owner>/<repo>.git/git-upload-pack: v2
// ls-refs / fetch when the first line names a v2 command, otherwise the v0
// want/have negotiation.
func HandleUploadPack(s *store.Store, owner, repoName string, body []byte) GitResponse {
	repo, errResp := requireRepo(s, owner, repoName)
	if errResp != nil {
		return *errResp
	}
	packets, _ := store.DecodePktLines(body)
	var first *store.PktPacket
	for i := range packets {
		if packets[i].Kind == "data" {
			first = &packets[i]
			break
		}
	}
	if first == nil {
		return textResponse(400, "empty upload-pack request")
	}
	firstLine := string(first.Payload)
	if strings.HasPrefix(firstLine, "command=ls-refs") {
		return v2LsRefsResponse(repo, packets)
	}
	if strings.HasPrefix(firstLine, "command=fetch") {
		args := parseV2FetchArgs(packets)
		if args == nil {
			return textResponse(400, "fetch without wants")
		}
		return v2FetchResponse(s, repo, args)
	}
	if strings.HasPrefix(firstLine, "want ") {
		return v0FetchResponse(s, repo, body)
	}
	return textResponse(400, "malformed upload-pack request")
}
