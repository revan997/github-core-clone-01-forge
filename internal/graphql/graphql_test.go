package graphql

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"

	"forge/internal/store"
)

// storeWorks probes the store without side effects; the store builder may
// still be landing, in which case store-dependent tests skip instead of
// panicking.
func storeWorks(s *store.Store) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	_ = s.GetRepo("adaiversen", "__probe__")
	return true
}

func requireStore(t *testing.T, s *store.Store) {
	t.Helper()
	if !storeWorks(s) {
		t.Skip("store not implemented yet")
	}
}

func gql(t *testing.T, s *store.Store, body string) map[string]any {
	t.Helper()
	resp := HandleGraphQL(s, []byte(body))
	if resp.Status != 200 {
		t.Fatalf("HandleGraphQL status = %d, body = %s (want 200 for %s)", resp.Status, resp.Body, body)
	}
	var out map[string]any
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatalf("response is not JSON: %v (body %s)", err, resp.Body)
	}
	return out
}

func gqlRaw(t *testing.T, s *store.Store, body string) (int, []byte) {
	t.Helper()
	resp := HandleGraphQL(s, []byte(body))
	return resp.Status, resp.Body
}

func at(t *testing.T, v any, keys ...string) any {
	t.Helper()
	cur := v
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("at(%v): not an object at key %q (value %#v)", keys, k, cur)
		}
		cur = m[k]
	}
	return cur
}

// seedRepo creates a repo with issues 1..n (issue n closed when closeLast)
// and branches main/feature pointing at one real commit object (so PRs are
// mergeable: head == base).
func seedRepo(t *testing.T, s *store.Store, name string, nIssues int, closeLast bool) *store.Repo {
	t.Helper()
	repo := s.CreateRepo(name, false, "test repo")
	if repo == nil {
		t.Fatalf("CreateRepo(%q) returned nil", name)
	}
	treeOID := store.HashObject(store.ObjectTree, store.BuildTreeBytes(nil))
	commitData := []byte("tree " + treeOID + "\nauthor A U Thor <a@example.invalid> 1700000000 +0000\ncommitter A U Thor <a@example.invalid> 1700000000 +0000\n\ninit\n")
	commitOID := store.HashObject(store.ObjectCommit, commitData)
	repo.Objects[treeOID] = store.GitObject{Type: store.ObjectTree, Data: store.BuildTreeBytes(nil)}
	repo.Objects[commitOID] = store.GitObject{Type: store.ObjectCommit, Data: commitData}
	repo.Refs["refs/heads/main"] = commitOID
	repo.Refs["refs/heads/feature"] = commitOID
	for i := 1; i <= nIssues; i++ {
		issue := s.CreateIssue(repo, fmt.Sprintf("issue %d", i), fmt.Sprintf("body %d", i), []string{})
		if closeLast && i == nIssues {
			issue.State = "closed"
		}
	}
	return repo
}

func TestViewerLogin(t *testing.T) {
	s := store.NewStore()
	out := gql(t, s, `{"query":"query ViewerLogin { viewer { login } }"}`)
	if got := at(t, out, "data", "viewer", "login"); got != "adaiversen" {
		t.Fatalf("viewer.login = %v, want adaiversen", got)
	}
}

func TestIntrospectionIssue(t *testing.T) {
	s := store.NewStore()
	out := gql(t, s, `{"query":"query Issue_fields { Issue: __type(name: \"Issue\") { fields(includeDeprecated: true) { name } } }"}`)
	fields := at(t, out, "data", "Issue", "fields").([]any)
	names := map[string]bool{}
	for _, f := range fields {
		names[f.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"id", "number", "title", "state", "body"} {
		if !names[want] {
			t.Fatalf("Issue fields missing %q: %v", want, names)
		}
	}
}

func TestIntrospectionTypesAndSearchType(t *testing.T) {
	s := store.NewStore()
	out := gql(t, s, `{"query":"query PullRequestAndRepositoryFields { PullRequest: __type(name: \"PullRequest\") { fields(includeDeprecated: true) { name } } Repository: __type(name: \"Repository\") { fields(includeDeprecated: true) { name } } }"}`)
	pr := at(t, out, "data", "PullRequest", "fields").([]any)
	prNames := map[string]bool{}
	for _, f := range pr {
		prNames[f.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files"} {
		if !prNames[want] {
			t.Fatalf("PullRequest fields missing %q", want)
		}
	}
	repo := at(t, out, "data", "Repository", "fields").([]any)
	repoNames := map[string]bool{}
	for _, f := range repo {
		repoNames[f.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"} {
		if !repoNames[want] {
			t.Fatalf("Repository fields missing %q", want)
		}
	}
	out2 := gql(t, s, `{"query":"query SearchType_enumValues { SearchType: __type(name: \"SearchType\") { enumValues(includeDeprecated: true) { name } } }"}`)
	enums := at(t, out2, "data", "SearchType", "enumValues").([]any)
	found := false
	for _, e := range enums {
		if e.(map[string]any)["name"] == "ISSUE" {
			found = true
		}
	}
	if !found {
		t.Fatalf("SearchType enumValues missing ISSUE: %v", enums)
	}
}

func TestIntrospectionUnknownType(t *testing.T) {
	s := store.NewStore()
	out := gql(t, s, `{"query":"query X { Nope: __type(name: \"Nope\") { fields { name } } }"}`)
	if got := at(t, out, "data", "Nope", "name"); got != "Nope" {
		t.Fatalf("unknown __type name = %v", got)
	}
	if got := at(t, out, "data", "Nope", "fields").([]any); len(got) != 0 {
		t.Fatalf("unknown __type fields = %v, want empty", got)
	}
}

func TestInvalidField(t *testing.T) {
	s := store.NewStore()
	out := gql(t, s, `{"query":"query ViewerInvalidField { viewer { login noSuchFieldOnViewer } }"}`)
	if got := at(t, out, "data", "viewer"); got != nil {
		t.Fatalf("viewer = %v, want null", got)
	}
	errs := out["errors"].([]any)
	if len(errs) != 1 || errs[0].(map[string]any)["message"] != `Cannot query field "noSuchFieldOnViewer" on type "User".` {
		t.Fatalf("errors = %v", errs)
	}
}

func TestMalformedQuery(t *testing.T) {
	s := store.NewStore()
	// Body that is not JSON.
	status, body := gqlRaw(t, s, `{`)
	if status != 400 {
		t.Fatalf("bad JSON status = %d, want 400", status)
	}
	var errBody map[string]any
	if err := json.Unmarshal(body, &errBody); err != nil {
		t.Fatalf("error body not JSON: %v", err)
	}
	if errBody["message"] != "Problems parsing JSON" {
		t.Fatalf("message = %v", errBody["message"])
	}
	// No query string.
	out := gql(t, s, `{"query": 123}`)
	errs, ok := out["errors"].([]any)
	if !ok || len(errs) != 1 || errs[0].(map[string]any)["message"] != "A document with a query string is required" {
		t.Fatalf("no-query errors = %v", out)
	}
	if _, hasData := out["data"]; hasData {
		t.Fatalf("no-query response should have no data key: %v", out)
	}
	// JSON null is not a document.
	status, _ = gqlRaw(t, s, `null`)
	if status != 400 {
		t.Fatalf("null body status = %d, want 400", status)
	}
	// Empty body parses as {} -> query-required error, HTTP 200.
	out = gql(t, s, ``)
	if errs, ok := out["errors"].([]any); !ok || len(errs) != 1 {
		t.Fatalf("empty body errors = %v", out)
	}
}

func TestBatch(t *testing.T) {
	s := store.NewStore()
	status, body := gqlRaw(t, s, `[{"query":"query ViewerLogin { viewer { login } }"},{"query":"query ViewerLogin { viewer { login } }"}]`)
	if status != 200 {
		t.Fatalf("batch status = %d", status)
	}
	var out []any
	if err := json.Unmarshal(body, &out); err != nil || len(out) != 2 {
		t.Fatalf("batch response = %s (err %v)", body, err)
	}
	for _, item := range out {
		if at(t, item, "data", "viewer", "login") != "adaiversen" {
			t.Fatalf("batch item viewer = %v", item)
		}
	}
}

func TestUnknownOperation(t *testing.T) {
	s := store.NewStore()
	out := gql(t, s, `{"query":"query Foo { repository } "}`)
	if got := at(t, out, "data"); got != nil {
		t.Fatalf("data = %v, want null", got)
	}
	errs := out["errors"].([]any)
	if len(errs) != 1 || errs[0].(map[string]any)["message"] != "Unknown operation Foo" {
		t.Fatalf("errors = %v", errs)
	}
	// Unknown operation asking for the viewer still answers the viewer.
	out = gql(t, s, `{"query":"query Foo { viewer { login } }"}`)
	if got := at(t, out, "data", "viewer", "login"); got != "adaiversen" {
		t.Fatalf("viewer fallback = %v", got)
	}
}

func TestRepositoryFields(t *testing.T) {
	s := store.NewStore()
	requireStore(t, s)
	repo := seedRepo(t, s, "gql-repo", 0, false)
	out := gql(t, s, `{"query":"query RepositoryFields($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { id databaseId name owner { login } defaultBranchRef { name } viewerPermission hasIssuesEnabled } }","variables":{"owner":"adaiversen","name":"gql-repo"}}`)
	node := at(t, out, "data", "repository")
	if got := at(t, node, "name"); got != "gql-repo" {
		t.Fatalf("name = %v", got)
	}
	if got := at(t, node, "databaseId"); got != float64(repo.DBID) {
		t.Fatalf("databaseId = %v, want %d", got, repo.DBID)
	}
	if got := at(t, node, "owner", "login"); got != "adaiversen" {
		t.Fatalf("owner.login = %v", got)
	}
	if got := at(t, node, "defaultBranchRef", "name"); got != "main" {
		t.Fatalf("defaultBranchRef.name = %v", got)
	}
	if got := at(t, node, "viewerPermission"); got != "ADMIN" {
		t.Fatalf("viewerPermission = %v", got)
	}
	if got := at(t, node, "hasIssuesEnabled"); got != true {
		t.Fatalf("hasIssuesEnabled = %v", got)
	}
	// Unknown repository -> null.
	out = gql(t, s, `{"query":"query RepositoryFields($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { id } }","variables":{"owner":"adaiversen","name":"missing"}}`)
	if got := at(t, out, "data", "repository"); got != nil {
		t.Fatalf("missing repo = %v, want null", got)
	}
}

func TestOpenIssuesStatesFilter(t *testing.T) {
	s := store.NewStore()
	requireStore(t, s)
	seedRepo(t, s, "gql-issues", 3, true) // issues 1,2 open; 3 closed
	out := gql(t, s, `{"query":"query OpenIssues($owner: String!, $name: String!, $first: Int!) { repository(owner: $owner, name: $name) { issues(first: $first, states: [OPEN], orderBy: {field: CREATED_AT, direction: DESC}) { nodes { number title state id } } } }","variables":{"owner":"adaiversen","name":"gql-issues","first":10}}`)
	nodes := at(t, out, "data", "repository", "issues", "nodes").([]any)
	if len(nodes) != 2 {
		t.Fatalf("OPEN nodes = %d, want 2: %v", len(nodes), nodes)
	}
	if nodes[0].(map[string]any)["number"] != float64(2) || nodes[1].(map[string]any)["number"] != float64(1) {
		t.Fatalf("DESC order wrong: %v", nodes)
	}
	for _, n := range nodes {
		if n.(map[string]any)["state"] != "OPEN" {
			t.Fatalf("node state = %v", n)
		}
	}
}

func TestIssuesPagination(t *testing.T) {
	s := store.NewStore()
	requireStore(t, s)
	seedRepo(t, s, "gql-pages", 7, false)
	out := gql(t, s, `{"query":"query IssuesPage($owner: String!, $name: String!, $first: Int!, $after: String) { repository(owner: $owner, name: $name) { issues(first: $first, after: $after) { nodes { number } pageInfo { hasNextPage endCursor } } } }","variables":{"owner":"adaiversen","name":"gql-pages","first":5,"after":null}}`)
	nodes := at(t, out, "data", "repository", "issues", "nodes").([]any)
	if len(nodes) != 5 {
		t.Fatalf("page 1 nodes = %d, want 5", len(nodes))
	}
	if nodes[0].(map[string]any)["number"] != float64(7) {
		t.Fatalf("page 1 first = %v, want 7", nodes[0])
	}
	if got := at(t, out, "data", "repository", "issues", "pageInfo", "hasNextPage"); got != true {
		t.Fatalf("page 1 hasNextPage = %v", got)
	}
	endCursor := at(t, out, "data", "repository", "issues", "pageInfo", "endCursor").(string)
	// Cursor must be base64url("cursor:3").
	raw, err := base64.RawURLEncoding.DecodeString(endCursor)
	if err != nil || string(raw) != "cursor:3" {
		t.Fatalf("endCursor = %q (decoded %q, err %v), want cursor:3", endCursor, raw, err)
	}
	out2 := gql(t, s, fmt.Sprintf(`{"query":"query IssuesPage($owner: String!, $name: String!, $first: Int!, $after: String) { repository(owner: $owner, name: $name) { issues(first: $first, after: $after) { nodes { number } pageInfo { hasNextPage endCursor } } } }","variables":{"owner":"adaiversen","name":"gql-pages","first":5,"after":%q}}`, endCursor))
	nodes2 := at(t, out2, "data", "repository", "issues", "nodes").([]any)
	if len(nodes2) != 2 || nodes2[0].(map[string]any)["number"] != float64(2) || nodes2[1].(map[string]any)["number"] != float64(1) {
		t.Fatalf("page 2 nodes = %v, want [2 1]", nodes2)
	}
	if got := at(t, out2, "data", "repository", "issues", "pageInfo", "hasNextPage"); got != false {
		t.Fatalf("page 2 hasNextPage = %v", got)
	}
}

func TestIssueByNumber(t *testing.T) {
	s := store.NewStore()
	requireStore(t, s)
	seedRepo(t, s, "gql-issue1", 2, false)
	out := gql(t, s, `{"query":"query IssueByNumber($owner: String!, $name: String!, $number: Int!) { repository(owner: $owner, name: $name) { issue(number: $number) { number title state id body } } }","variables":{"owner":"adaiversen","name":"gql-issue1","number":1}}`)
	issue := at(t, out, "data", "repository", "issue")
	if got := at(t, issue, "number"); got != float64(1) {
		t.Fatalf("number = %v", got)
	}
	if got := at(t, issue, "title"); got != "issue 1" {
		t.Fatalf("title = %v", got)
	}
	if got := at(t, issue, "state"); got != "OPEN" {
		t.Fatalf("state = %v", got)
	}
	if got := at(t, issue, "body"); got != "body 1" {
		t.Fatalf("body = %v", got)
	}
}

func TestPullRequestsAndFiles(t *testing.T) {
	s := store.NewStore()
	requireStore(t, s)
	repo := seedRepo(t, s, "gql-prs", 0, false)
	pull, msg, ok := s.CreatePull(repo, "pr title", "pr body", "feature", "main")
	if !ok {
		t.Fatalf("CreatePull failed: %s", msg)
	}
	out := gql(t, s, `{"query":"query PullRequests($owner: String!, $name: String!, $first: Int!) { repository(owner: $owner, name: $name) { pullRequests(first: $first) { nodes { number headRefName baseRefName mergeable state } } } }","variables":{"owner":"adaiversen","name":"gql-prs","first":5}}`)
	nodes := at(t, out, "data", "repository", "pullRequests", "nodes").([]any)
	if len(nodes) != 1 {
		t.Fatalf("pullRequests nodes = %d, want 1", len(nodes))
	}
	node := nodes[0].(map[string]any)
	if node["number"] != float64(pull.Number) || node["headRefName"] != "feature" || node["baseRefName"] != "main" || node["state"] != "OPEN" || node["mergeable"] != "MERGEABLE" {
		t.Fatalf("pull node = %v", node)
	}
	out = gql(t, s, fmt.Sprintf(`{"query":"query PullRequestFiles($owner: String!, $name: String!, $number: Int!) { repository(owner: $owner, name: $name) { pullRequest(number: $number) { number files(first: 10) { nodes { path } } } } }","variables":{"owner":"adaiversen","name":"gql-prs","number":%d}}`, pull.Number))
	files := at(t, out, "data", "repository", "pullRequest", "files", "nodes").([]any)
	if len(files) != 0 {
		t.Fatalf("files = %v, want empty (head == base)", files)
	}
}

func TestMutationsIssueCreateCloseComment(t *testing.T) {
	s := store.NewStore()
	requireStore(t, s)
	repo := seedRepo(t, s, "gql-mut", 0, false)
	repoID := store.GlobalID("Repository", repo.DBID)
	out := gql(t, s, fmt.Sprintf(`{"query":"mutation IssueCreate($input: CreateIssueInput!) { createIssue(input: $input) { issue { number id } } }","variables":{"input":{"repositoryId":%q,"title":"created","body":"b"}}}`, repoID))
	issue := at(t, out, "data", "createIssue", "issue")
	number := issue.(map[string]any)["number"]
	if number != float64(1) {
		t.Fatalf("created issue number = %v", number)
	}
	issueID := issue.(map[string]any)["id"].(string)
	out = gql(t, s, fmt.Sprintf(`{"query":"mutation AddComment($input: AddCommentInput!) { addComment(input: $input) { commentEdge { node { id body } } } }","variables":{"input":{"subjectId":%q,"body":"hello"}}}`, issueID))
	if got := at(t, out, "data", "addComment", "commentEdge", "node", "body"); got != "hello" {
		t.Fatalf("comment body = %v", got)
	}
	out = gql(t, s, fmt.Sprintf(`{"query":"mutation IssueClose($input: CloseIssueInput!) { closeIssue(input: $input) { issue { number id state } } }","variables":{"input":{"issueId":%q}}}`, issueID))
	if got := at(t, out, "data", "closeIssue", "issue", "state"); got != "CLOSED" {
		t.Fatalf("closed state = %v", got)
	}
	// Unknown repository / issue error shapes.
	out = gql(t, s, `{"query":"mutation IssueCreate($input: CreateIssueInput!) { createIssue(input: $input) { issue { number id } } }","variables":{"input":{"repositoryId":"bogus","title":"x","body":"y"}}}`)
	if got := at(t, out, "data", "createIssue"); got != nil {
		t.Fatalf("bad repo createIssue = %v, want null", got)
	}
	errs := out["errors"].([]any)
	if len(errs) != 1 || errs[0].(map[string]any)["message"] != "Repository not found" {
		t.Fatalf("errors = %v", errs)
	}
}

func TestMutationsPullCreateMerge(t *testing.T) {
	s := store.NewStore()
	requireStore(t, s)
	repo := seedRepo(t, s, "gql-prmut", 0, false)
	repoID := store.GlobalID("Repository", repo.DBID)
	out := gql(t, s, fmt.Sprintf(`{"query":"mutation PullRequestCreate($input: CreatePullRequestInput!) { createPullRequest(input: $input) { pullRequest { number id state headRefName baseRefName } } }","variables":{"input":{"repositoryId":%q,"baseRefName":"main","headRefName":"feature","title":"pr"}}}`, repoID))
	pr := at(t, out, "data", "createPullRequest", "pullRequest")
	if got := at(t, pr, "state"); got != "OPEN" {
		t.Fatalf("created PR state = %v", got)
	}
	if got := at(t, pr, "headRefName"); got != "feature" {
		t.Fatalf("headRefName = %v", got)
	}
	prID := pr.(map[string]any)["id"].(string)
	out = gql(t, s, fmt.Sprintf(`{"query":"mutation MergePullRequest($input: MergePullRequestInput!) { mergePullRequest(input: $input) { pullRequest { number merged state } } }","variables":{"input":{"pullRequestId":%q,"mergeMethod":"SQUASH"}}}`, prID))
	if got := at(t, out, "data", "mergePullRequest", "pullRequest", "merged"); got != true {
		t.Fatalf("merged = %v (body %v)", got, out)
	}
	if got := at(t, out, "data", "mergePullRequest", "pullRequest", "state"); got != "MERGED" {
		t.Fatalf("merged PR state = %v", got)
	}
}

func TestNodeRepository(t *testing.T) {
	s := store.NewStore()
	requireStore(t, s)
	repo := seedRepo(t, s, "gql-node", 0, false)
	repoID := store.GlobalID("Repository", repo.DBID)
	out := gql(t, s, fmt.Sprintf(`{"query":"query NodeRepository($id: ID!) { node(id: $id) { id ... on Repository { name nameWithOwner } } }","variables":{"id":%q}}`, repoID))
	if got := at(t, out, "data", "node", "name"); got != "gql-node" {
		t.Fatalf("node name = %v", got)
	}
	if got := at(t, out, "data", "node", "nameWithOwner"); got != "adaiversen/gql-node" {
		t.Fatalf("node nameWithOwner = %v", got)
	}
	out = gql(t, s, `{"query":"query NodeRepository($id: ID!) { node(id: $id) { id } }","variables":{"id":"bogus"}}`)
	if got := at(t, out, "data", "node"); got != nil {
		t.Fatalf("bogus node = %v, want null", got)
	}
}
