// Package graphql is a faithful Go port of the Node forge's graphql.ts: the
// GraphQL v4 surface (POST /api/graphql), a small hand-rolled executor for
// exactly the documents the suite sends. It dispatches on the operation name
// (every suite document is named) and reads arguments from `variables`,
// falling back to the literals in the query text.
//
// It is NOT a general GraphQL engine: only the fixed shapes in graphql.ts
// are served (repository/issues/pullRequests connections, __type
// introspection, node(id), and the create/close/merge/addComment mutations).
// Auth is handled by the app layer; this handler assumes authorization.
//
// Viewer identity comes from the scenario package (scenario.Viewer), the
// same source graphql.ts imports VIEWER from.
package graphql

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"forge/internal/rest"
	"forge/internal/scenario"
	"forge/internal/store"
)

var (
	issueFields          = []string{"id", "databaseId", "number", "title", "state", "body", "createdAt", "updatedAt", "author", "labels", "comments", "url"}
	pullRequestFields    = []string{"id", "databaseId", "number", "title", "state", "body", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "mergeable", "merged", "files", "createdAt", "updatedAt"}
	repositoryFields     = []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "createdAt"}
	searchTypeEnumValues = []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION"}
)

var (
	operationRe  = regexp.MustCompile(`^\s*(?:query|mutation)\s+([A-Za-z0-9_]+)`)
	typeRe       = regexp.MustCompile(`(\w+)\s*:\s*__type\s*\(\s*name\s*:\s*"([^"]+)"\s*\)`)
	statesLitRe  = regexp.MustCompile(`states\s*:\s*\[([^\]]*)\]`)
	directionRe  = regexp.MustCompile(`direction\s*:\s*ASC`)
	viewerWordRe = regexp.MustCompile(`\bviewer\b`)
)

func isRecord(v any) bool {
	_, ok := v.(map[string]any)
	return ok
}

func asString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// asNumber mirrors TS asNumber: a finite JSON number, else absent.
func asNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		return n, true
	case json.Number:
		f, err := n.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// asInt is asNumber restricted to integer-valued numbers (TS map lookups with a
// non-integer key miss, so a fractional number must not match issue #1).
func asInt(v any) (int, bool) {
	f, ok := asNumber(v)
	if !ok || math.Trunc(f) != f {
		return 0, false
	}
	return int(f), true
}

// jsNumber mirrors JS Number(s) for the small strings decodeCursor sees:
// "" -> 0, surrounding whitespace trimmed, hex and exponents honoured.
func jsNumber(s string) (float64, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, true
	}
	if f, err := strconv.ParseFloat(t, 64); err == nil {
		return f, true
	}
	if strings.HasPrefix(t, "0x") || strings.HasPrefix(t, "0X") {
		if i, err := strconv.ParseInt(t[2:], 16, 64); err == nil {
			return float64(i), true
		}
	}
	return 0, false
}

// decodeCursor decodes a `cursor:<number>` page cursor (base64url); ok=false
// when it is not one. Mirrors TS decodeCursor (Number() + integer check).
func decodeCursor(cursor string) (int, bool) {
	if cursor == "" {
		return 0, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(cursor)
		if err != nil {
			return 0, false
		}
	}
	text := string(raw)
	if !strings.HasPrefix(text, "cursor:") {
		return 0, false
	}
	n, ok := jsNumber(text[len("cursor:"):])
	if !ok || math.Trunc(n) != n {
		return 0, false
	}
	return int(n), true
}

func encodeCursor(number int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(number)))
}

func issueNode(repo *store.Repo, issue *store.Issue) map[string]any {
	state := "CLOSED"
	if issue.State == "open" {
		state = "OPEN"
	}
	return map[string]any{
		"id":         issue.GlobalID,
		"databaseId": issue.Number,
		"number":     issue.Number,
		"title":      issue.Title,
		"state":      state,
		"body":       issue.Body,
		"createdAt":  issue.CreatedAt,
		"updatedAt":  issue.UpdatedAt,
	}
}

func pullNode(s *store.Store, repo *store.Repo, pull *store.PullRequest) map[string]any {
	state := "CLOSED"
	if pull.Merged {
		state = "MERGED"
	} else if pull.State == "open" {
		state = "OPEN"
	}
	mergeable := "UNKNOWN"
	if s.PullMergeable(repo, pull) {
		mergeable = "MERGEABLE"
	} else if pull.State == "open" {
		mergeable = "CONFLICTING"
	}
	headOid, ok := repo.Refs["refs/heads/"+pull.HeadRef]
	if !ok {
		headOid = pull.HeadSHA
	}
	baseOid, ok := repo.Refs["refs/heads/"+pull.BaseRef]
	if !ok {
		baseOid = pull.BaseSHA
	}
	files := s.PullFiles(repo, pull)
	nodes := make([]any, 0, len(files))
	for _, f := range files {
		nodes = append(nodes, map[string]any{"path": f.Filename})
	}
	return map[string]any{
		"id":          pull.GlobalID,
		"databaseId":  pull.Number,
		"number":      pull.Number,
		"title":       pull.Title,
		"body":        pull.Body,
		"state":       state,
		"merged":      pull.Merged,
		"mergeable":   mergeable,
		"headRefName": pull.HeadRef,
		"baseRefName": pull.BaseRef,
		"headRefOid":  headOid,
		"baseRefOid":  baseOid,
		"createdAt":   pull.CreatedAt,
		"updatedAt":   pull.UpdatedAt,
		"files":       map[string]any{"nodes": nodes},
	}
}

func repositoryNode(s *store.Store, repo *store.Repo) map[string]any {
	headTip, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]
	var defaultBranchRef any
	if ok {
		defaultBranchRef = map[string]any{"name": repo.DefaultBranch, "target": map[string]any{"oid": headTip}}
	}
	return map[string]any{
		"id":               store.GlobalID("Repository", repo.DBID),
		"databaseId":       repo.DBID,
		"name":             repo.Name,
		"nameWithOwner":    repo.Owner + "/" + repo.Name,
		"owner":            map[string]any{"login": repo.Owner, "id": 1000},
		"defaultBranchRef": defaultBranchRef,
		"viewerPermission": "ADMIN",
		"hasIssuesEnabled": true,
		"createdAt":        repo.CreatedSeq,
	}
}

// issuesConnection is the issues connection: CREATED_AT DESC by default
// (number desc), paged by cursor. Mirrors TS issuesConnection exactly,
// including totalCount counting the filtered (not the paged) list.
func issuesConnection(repo *store.Repo, first *float64, states []string, after string, direction string) map[string]any {
	issues := make([]*store.Issue, 0, len(repo.Issues))
	for _, issue := range repo.Issues {
		issues = append(issues, issue)
	}
	if len(states) > 0 {
		wanted := make(map[string]bool, len(states))
		for _, st := range states {
			wanted[strings.ToLower(st)] = true
		}
		filtered := issues[:0]
		for _, issue := range issues {
			if wanted[issue.State] {
				filtered = append(filtered, issue)
			}
		}
		issues = filtered
	}
	if direction == "ASC" {
		sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })
	} else {
		sort.Slice(issues, func(i, j int) bool { return issues[i].Number > issues[j].Number })
	}
	afterNumber, hasAfter := decodeCursor(after)
	if hasAfter {
		filtered := issues[:0]
		for _, issue := range issues {
			if direction == "ASC" {
				if issue.Number > afterNumber {
					filtered = append(filtered, issue)
				}
			} else if issue.Number < afterNumber {
				filtered = append(filtered, issue)
			}
		}
		issues = filtered
	}
	limit := len(issues)
	if first != nil {
		limit = int(*first) // TS slice(0, x) truncates toward zero, same as int()
		if limit < 0 {
			limit = 0
		}
	}
	if limit > len(issues) {
		limit = len(issues)
	}
	page := issues[:limit]
	hasNextPage := len(issues) > len(page)
	nodes := make([]any, 0, len(page))
	for _, issue := range page {
		nodes = append(nodes, issueNode(repo, issue))
	}
	var startCursor, endCursor any
	if len(page) > 0 {
		startCursor = encodeCursor(page[0].Number)
		endCursor = encodeCursor(page[len(page)-1].Number)
	}
	return map[string]any{
		"nodes":      nodes,
		"totalCount": len(issues),
		"pageInfo": map[string]any{
			"hasNextPage":     hasNextPage,
			"hasPreviousPage": hasAfter,
			"startCursor":     startCursor,
			"endCursor":       endCursor,
		},
	}
}

func errResult(message string) map[string]any {
	return map[string]any{"errors": []any{map[string]any{"message": message}}}
}

// executor holds the per-document state: the query text and its variables.
type executor struct {
	store     *store.Store
	query     string
	variables map[string]any
}

// arg reads one argument: variables first, then a string literal, then a
// number literal in the query text. Mirrors the TS `arg` closure.
func (e *executor) arg(name string) any {
	if v, ok := e.variables[name]; ok {
		return v
	}
	strRe := regexp.MustCompile(regexp.QuoteMeta(name) + `\s*:\s*"([^"]*)"`)
	if m := strRe.FindStringSubmatch(e.query); m != nil {
		return m[1]
	}
	numRe := regexp.MustCompile(regexp.QuoteMeta(name) + `\s*:\s*(\d+)`)
	if m := numRe.FindStringSubmatch(e.query); m != nil {
		if n, err := strconv.ParseFloat(m[1], 64); err == nil {
			return n
		}
	}
	return nil
}

func (e *executor) ownerName() (owner, name string) {
	owner = scenario.Viewer.Login
	if v, ok := asString(e.arg("owner")); ok {
		owner = v
	}
	if v, ok := asString(e.arg("name")); ok {
		name = v
	}
	return owner, name
}

func (e *executor) repositoryField(repo *store.Repo, field string) any {
	if repo == nil {
		return nil
	}
	switch field {
	case "issue":
		number, ok := asInt(e.arg("number"))
		if !ok {
			return nil
		}
		issue, ok := repo.Issues[number]
		if !ok {
			return nil
		}
		return issueNode(repo, issue)
	case "issues":
		var first *float64
		if f, ok := asNumber(e.arg("first")); ok {
			first = &f
		}
		var states []string
		if raw, ok := e.variables["states"].([]any); ok {
			for _, item := range raw {
				if s, ok := item.(string); ok {
					states = append(states, s)
				}
			}
		} else if m := statesLitRe.FindStringSubmatch(e.query); m != nil {
			for _, item := range strings.Split(m[1], ",") {
				if item = strings.TrimSpace(item); item != "" {
					states = append(states, item)
				}
			}
		}
		after, _ := asString(e.arg("after"))
		direction := "DESC"
		if directionRe.MatchString(e.query) {
			direction = "ASC"
		}
		return issuesConnection(repo, first, states, after, direction)
	case "pullRequests":
		var first *int
		if n, ok := asInt(e.arg("first")); ok {
			first = &n
		}
		pulls := make([]*store.PullRequest, 0, len(repo.Pulls))
		for _, pull := range repo.Pulls {
			pulls = append(pulls, pull)
		}
		sort.Slice(pulls, func(i, j int) bool { return pulls[i].Number < pulls[j].Number })
		if first != nil {
			// TS pulls.slice(0, first): a negative end counts back from len.
			end := *first
			if end < 0 {
				end = len(pulls) + end
			}
			if end < 0 {
				end = 0
			}
			if end > len(pulls) {
				end = len(pulls)
			}
			pulls = pulls[:end]
		}
		nodes := make([]any, 0, len(pulls))
		for _, pull := range pulls {
			nodes = append(nodes, pullNode(e.store, repo, pull))
		}
		return map[string]any{"nodes": nodes, "totalCount": len(repo.Pulls)}
	case "pullRequest":
		number, ok := asInt(e.arg("number"))
		if !ok {
			return nil
		}
		pull, ok := repo.Pulls[number]
		if !ok {
			return nil
		}
		return pullNode(e.store, repo, pull)
	}
	return nil
}

func findRepoByGlobalID(s *store.Store, id string) *store.Repo {
	kind, parts, ok := store.ParseGlobalID(id)
	if !ok || kind != "Repository" || len(parts) < 1 {
		return nil
	}
	dbID, err := strconv.Atoi(parts[0])
	if err != nil {
		return nil
	}
	for _, repo := range s.ListViewerRepos() {
		if repo.DBID == dbID {
			return repo
		}
	}
	return nil
}

func findIssueByGlobalID(s *store.Store, id string) (*store.Repo, *store.Issue) {
	kind, parts, ok := store.ParseGlobalID(id)
	if !ok || kind != "Issue" || len(parts) < 2 {
		return nil, nil
	}
	dbID, err1 := strconv.Atoi(parts[0])
	num, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return nil, nil
	}
	for _, repo := range s.ListViewerRepos() {
		if repo.DBID != dbID {
			continue
		}
		if issue, ok := repo.Issues[num]; ok {
			return repo, issue
		}
	}
	return nil, nil
}

func findPullByGlobalID(s *store.Store, id string) (*store.Repo, *store.PullRequest) {
	kind, parts, ok := store.ParseGlobalID(id)
	if !ok || kind != "PullRequest" || len(parts) < 2 {
		return nil, nil
	}
	dbID, err1 := strconv.Atoi(parts[0])
	num, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return nil, nil
	}
	for _, repo := range s.ListViewerRepos() {
		if repo.DBID != dbID {
			continue
		}
		if pull, ok := repo.Pulls[num]; ok {
			return repo, pull
		}
	}
	return nil, nil
}

// executeDocument executes one document and returns its JSON body
// ({data} or {data, errors}). Mirrors TS executeDocument.
func (e *executor) executeDocument(document any) map[string]any {
	doc, ok := document.(map[string]any)
	if !ok {
		return errResult("A document with a query string is required")
	}
	query, ok := doc["query"].(string)
	if !ok {
		return errResult("A document with a query string is required")
	}
	e.query = query
	if vars, ok := doc["variables"].(map[string]any); ok {
		e.variables = vars
	} else {
		e.variables = map[string]any{}
	}

	operation := ""
	if m := operationRe.FindStringSubmatch(query); m != nil {
		operation = m[1]
	}

	// Introspection: __type(name: "...") — a document may carry several
	// aliased __type fields at once (PullRequest + Repository).
	if strings.Contains(query, "__type") {
		data := map[string]any{}
		for _, m := range typeRe.FindAllStringSubmatch(query, -1) {
			alias, typeName := m[1], m[2]
			var fields []string
			switch typeName {
			case "Issue":
				fields = issueFields
			case "PullRequest":
				fields = pullRequestFields
			case "Repository":
				fields = repositoryFields
			}
			var enumValues []string
			if typeName == "SearchType" {
				enumValues = searchTypeEnumValues
			}
			fieldList := make([]any, 0, len(fields))
			for _, name := range fields {
				fieldList = append(fieldList, map[string]any{"name": name})
			}
			enumList := make([]any, 0, len(enumValues))
			for _, name := range enumValues {
				enumList = append(enumList, map[string]any{"name": name})
			}
			data[alias] = map[string]any{"name": typeName, "fields": fieldList, "enumValues": enumList}
		}
		if len(data) > 0 {
			return map[string]any{"data": data}
		}
		return errResult("No __type field found")
	}

	if strings.Contains(query, "noSuchFieldOnViewer") {
		return map[string]any{
			"data":   map[string]any{"viewer": nil},
			"errors": []any{map[string]any{"message": `Cannot query field "noSuchFieldOnViewer" on type "User".`}},
		}
	}

	owner, name := e.ownerName()
	var repo *store.Repo
	if name != "" {
		repo = e.store.GetRepo(owner, name)
	}

	withRepo := func(field string) map[string]any {
		if repo == nil {
			return map[string]any{"data": map[string]any{"repository": nil}}
		}
		node := repositoryNode(e.store, repo)
		node[field] = e.repositoryField(repo, field)
		return map[string]any{"data": map[string]any{"repository": node}}
	}

	input := func() map[string]any {
		if in, ok := e.variables["input"].(map[string]any); ok {
			return in
		}
		return map[string]any{}
	}

	switch operation {
	case "ViewerLogin":
		return map[string]any{"data": map[string]any{"viewer": map[string]any{
			"login": scenario.Viewer.Login,
			"id":    store.GlobalID("User", scenario.Viewer.ID),
			"name":  scenario.Viewer.Name,
			"email": scenario.Viewer.Email,
		}}}
	case "RepositoryFields":
		if repo == nil {
			return map[string]any{"data": map[string]any{"repository": nil}}
		}
		return map[string]any{"data": map[string]any{"repository": repositoryNode(e.store, repo)}}
	case "OpenIssues", "IssuesPage":
		return withRepo("issues")
	case "IssueByNumber":
		return withRepo("issue")
	case "PullRequests":
		return withRepo("pullRequests")
	case "PullRequestFiles", "PullRequestId":
		return withRepo("pullRequest")
	case "IssueCreate":
		{
			in := input()
			repositoryID, _ := asString(in["repositoryId"])
			target := findRepoByGlobalID(e.store, repositoryID)
			if target == nil {
				return map[string]any{
					"data":   map[string]any{"createIssue": nil},
					"errors": []any{map[string]any{"message": "Repository not found"}},
				}
			}
			title, _ := asString(in["title"])
			body, _ := asString(in["body"])
			issue := e.store.CreateIssue(target, title, body, []string{})
			return map[string]any{"data": map[string]any{"createIssue": map[string]any{"issue": issueNode(target, issue)}}}
		}
	case "IssueClose":
		{
			in := input()
			issueID, _ := asString(in["issueId"])
			foundRepo, found := findIssueByGlobalID(e.store, issueID)
			if found == nil {
				return map[string]any{
					"data":   map[string]any{"closeIssue": nil},
					"errors": []any{map[string]any{"message": "Issue not found"}},
				}
			}
			found.State = "closed"
			closedAt := e.store.SeqTimestamp()
			found.ClosedAt = &closedAt
			found.UpdatedAt = e.store.SeqTimestamp()
			e.store.TouchIssue(found)
			return map[string]any{"data": map[string]any{"closeIssue": map[string]any{"issue": issueNode(foundRepo, found)}}}
		}
	case "PullRequestCreate":
		{
			in := input()
			repositoryID, _ := asString(in["repositoryId"])
			target := findRepoByGlobalID(e.store, repositoryID)
			if target == nil {
				return map[string]any{
					"data":   map[string]any{"createPullRequest": nil},
					"errors": []any{map[string]any{"message": "Repository not found"}},
				}
			}
			title, _ := asString(in["title"])
			body, _ := asString(in["body"])
			head, _ := asString(in["headRefName"])
			base, _ := asString(in["baseRefName"])
			pull, message, ok := e.store.CreatePull(target, title, body, head, base)
			if !ok {
				return map[string]any{
					"data":   map[string]any{"createPullRequest": nil},
					"errors": []any{map[string]any{"message": message}},
				}
			}
			return map[string]any{"data": map[string]any{"createPullRequest": map[string]any{"pullRequest": pullNode(e.store, target, pull)}}}
		}
	case "MergePullRequest":
		{
			in := input()
			pullRequestID, _ := asString(in["pullRequestId"])
			foundRepo, found := findPullByGlobalID(e.store, pullRequestID)
			if found == nil {
				return map[string]any{
					"data":   map[string]any{"mergePullRequest": nil},
					"errors": []any{map[string]any{"message": "Pull request not found"}},
				}
			}
			if _, message, ok := e.store.MergePull(foundRepo, found); !ok {
				return map[string]any{
					"data":   map[string]any{"mergePullRequest": nil},
					"errors": []any{map[string]any{"message": message}},
				}
			}
			return map[string]any{"data": map[string]any{"mergePullRequest": map[string]any{"pullRequest": pullNode(e.store, foundRepo, found)}}}
		}
	case "AddComment":
		{
			in := input()
			subjectID, _ := asString(in["subjectId"])
			foundRepo, found := findIssueByGlobalID(e.store, subjectID)
			if found == nil {
				return map[string]any{
					"data":   map[string]any{"addComment": nil},
					"errors": []any{map[string]any{"message": "Subject not found"}},
				}
			}
			body, _ := asString(in["body"])
			comment := e.store.AddComment(foundRepo, found, body)
			return map[string]any{"data": map[string]any{"addComment": map[string]any{"commentEdge": map[string]any{"node": map[string]any{
				"id":   comment.GlobalID,
				"body": comment.Body,
			}}}}}
		}
	case "NodeRepository":
		{
			id, _ := asString(e.arg("id"))
			kind, parts, ok := store.ParseGlobalID(id)
			if !ok || kind != "Repository" || len(parts) < 1 {
				return map[string]any{"data": map[string]any{"node": nil}}
			}
			dbID, err := strconv.Atoi(parts[0])
			if err != nil {
				return map[string]any{"data": map[string]any{"node": nil}}
			}
			for _, candidate := range e.store.ListViewerRepos() {
				if candidate.DBID == dbID {
					return map[string]any{"data": map[string]any{"node": repositoryNode(e.store, candidate)}}
				}
			}
			return map[string]any{"data": map[string]any{"node": nil}}
		}
	default:
		// A named operation the executor does not know, or an anonymous
		// document: answer with the viewer when the document asks for it,
		// else a field error (never a crash).
		if viewerWordRe.MatchString(query) {
			return map[string]any{"data": map[string]any{"viewer": map[string]any{"login": scenario.Viewer.Login}}}
		}
		op := operation
		if op == "" {
			op = "(anonymous)"
		}
		return map[string]any{
			"data":   nil,
			"errors": []any{map[string]any{"message": "Unknown operation " + op}},
		}
	}
}

// HandleGraphQL handles POST /api/graphql. Mirrors TS handleGraphQL: a JSON
// array is a batch answered in order; a JSON object is one document; anything
// else is a 400.
func HandleGraphQL(s *store.Store, body []byte) rest.RestResponse {
	var value any
	if len(body) == 0 {
		// TS parseJsonBody: an empty body parses as {}.
		value = map[string]any{}
	} else {
		var resp *rest.RestResponse
		value, resp = rest.ParseJSONBody(body)
		if resp != nil {
			return *resp
		}
	}
	e := &executor{store: s}
	switch v := value.(type) {
	case []any:
		results := make([]any, 0, len(v))
		for _, document := range v {
			results = append(results, e.executeDocument(document))
		}
		return rest.JSONBody(200, results)
	case map[string]any:
		return rest.JSONBody(200, e.executeDocument(v))
	default:
		return rest.JSONError(400, "A GraphQL document is required")
	}
}
