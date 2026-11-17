package mcp

import (
	"bytes"
	"encoding/json"
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

func call(t *testing.T, s *store.Store, body string) (int, map[string]any) {
	t.Helper()
	resp := HandleMcp(s, []byte(body))
	var out map[string]any
	if len(resp.Body) > 0 {
		if err := json.Unmarshal(resp.Body, &out); err != nil {
			t.Fatalf("response is not JSON: %v (body %s)", err, resp.Body)
		}
	}
	return resp.Status, out
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

func rpcError(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	err, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected a JSON-RPC error, got %v", out)
	}
	return err
}

func toolCall(t *testing.T, s *store.Store, id any, name string, args map[string]any) (int, map[string]any) {
	t.Helper()
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": args})
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": json.RawMessage(params)})
	return call(t, s, string(req))
}

// seedBranches points main/feature at one real commit object (head == base,
// so PRs are mergeable).
func seedBranches(t *testing.T, repo *store.Repo) {
	t.Helper()
	treeOID := store.HashObject(store.ObjectTree, store.BuildTreeBytes(nil))
	commitData := []byte("tree " + treeOID + "\nauthor A U Thor <a@example.invalid> 1700000000 +0000\ncommitter A U Thor <a@example.invalid> 1700000000 +0000\n\ninit\n")
	commitOID := store.HashObject(store.ObjectCommit, commitData)
	repo.Objects[treeOID] = store.GitObject{Type: store.ObjectTree, Data: store.BuildTreeBytes(nil)}
	repo.Objects[commitOID] = store.GitObject{Type: store.ObjectCommit, Data: commitData}
	repo.Refs["refs/heads/main"] = commitOID
	repo.Refs["refs/heads/feature"] = commitOID
}

func TestParseError(t *testing.T) {
	s := store.NewStore()
	status, out := call(t, s, `{"jsonrpc": "2.0", "id": 424242, "method": "tools/list"`)
	if status != 400 {
		t.Fatalf("status = %d, want 400", status)
	}
	if got := rpcError(t, out)["code"]; got != float64(-32700) {
		t.Fatalf("code = %v, want -32700", got)
	}
	if out["id"] != nil {
		t.Fatalf("id = %v, want null", out["id"])
	}
}

func TestInvalidRequest(t *testing.T) {
	s := store.NewStore()
	status, out := call(t, s, `{}`)
	if status != 400 {
		t.Fatalf("status = %d, want 400", status)
	}
	if got := rpcError(t, out)["code"]; got != float64(-32600) {
		t.Fatalf("code = %v, want -32600", got)
	}
	status, out = call(t, s, `[]`)
	if status != 400 || rpcError(t, out)["code"] != float64(-32600) {
		t.Fatalf("array request: status %d, out %v", status, out)
	}
}

func TestDiscover(t *testing.T) {
	s := store.NewStore()
	status, out := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"server/discover"}`)
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if got := at(t, out, "result", "resultType"); got != "discover" {
		t.Fatalf("resultType = %v", got)
	}
	versions := at(t, out, "result", "supportedVersions").([]any)
	found := false
	for _, v := range versions {
		if v == "2026-07-28" {
			found = true
		}
	}
	if !found {
		t.Fatalf("supportedVersions = %v", versions)
	}
	if got := at(t, out, "result", "serverInfo", "name"); got != "makefun-forge" {
		t.Fatalf("serverInfo.name = %v", got)
	}
	if got := at(t, out, "result", "serverInfo", "version"); got != "1.0.0" {
		t.Fatalf("serverInfo.version = %v", got)
	}
	if _, ok := at(t, out, "result", "capabilities", "tools").(map[string]any); !ok {
		t.Fatalf("capabilities.tools missing")
	}
}

func TestInitialize(t *testing.T) {
	s := store.NewStore()
	_, out := call(t, s, `{"jsonrpc":"2.0","id":"abc","method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if got := at(t, out, "result", "protocolVersion"); got != "2025-06-18" {
		t.Fatalf("negotiated = %v, want 2025-06-18", got)
	}
	if got := out["id"]; got != "abc" {
		t.Fatalf("id echoed = %v, want abc", got)
	}
	_, out = call(t, s, `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	if got := at(t, out, "result", "protocolVersion"); got != "2025-11-25" {
		t.Fatalf("unknown version negotiated = %v, want 2025-11-25", got)
	}
}

func TestPingAndNotifications(t *testing.T) {
	s := store.NewStore()
	_, out := call(t, s, `{"jsonrpc":"2.0","id":9,"method":"ping"}`)
	if _, ok := out["result"].(map[string]any); !ok {
		t.Fatalf("ping result = %v", out)
	}
	resp := HandleMcp(s, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if resp.Status != 202 || len(resp.Body) != 0 {
		t.Fatalf("notification: status %d, body %q, want 202 empty", resp.Status, resp.Body)
	}
	_, out = call(t, s, `{"jsonrpc":"2.0","id":10,"method":"notifications/initialized"}`)
	if _, ok := out["result"].(map[string]any); !ok {
		t.Fatalf("notifications/initialized with id: %v", out)
	}
}

func TestToolsList(t *testing.T) {
	s := store.NewStore()
	status, out := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	tools := at(t, out, "result", "tools").([]any)
	if len(tools) != 6 {
		t.Fatalf("tools = %d, want 6", len(tools))
	}
	want := []string{"create_repo", "get_repo", "create_issue", "list_issues", "create_pull_request", "merge_pull_request"}
	names := map[string]bool{}
	for _, tool := range tools {
		m := tool.(map[string]any)
		name := m["name"].(string)
		names[name] = true
		if m["description"] == "" {
			t.Fatalf("tool %s has no description", name)
		}
		if at(t, m, "inputSchema", "type") != "object" {
			t.Fatalf("tool %s inputSchema.type != object", name)
		}
	}
	for _, w := range want {
		if !names[w] {
			t.Fatalf("missing tool %s", w)
		}
	}
	// Idempotent: byte-identical over two calls.
	first := HandleMcp(s, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	second := HandleMcp(s, []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	// Bodies differ only by the echoed id; the tools arrays must be identical.
	var f, s2 map[string]any
	_ = json.Unmarshal(first.Body, &f)
	_ = json.Unmarshal(second.Body, &s2)
	fb, _ := json.Marshal(at(t, f, "result", "tools"))
	sb, _ := json.Marshal(at(t, s2, "result", "tools"))
	if !bytes.Equal(fb, sb) {
		t.Fatalf("tools/list not idempotent:\n%s\n%s", fb, sb)
	}
}

func TestUnknownMethod(t *testing.T) {
	s := store.NewStore()
	status, out := call(t, s, `{"jsonrpc":"2.0","id":7,"method":"resources/list"}`)
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
	err := rpcError(t, out)
	if err["code"] != float64(-32601) {
		t.Fatalf("code = %v, want -32601", err["code"])
	}
	if err["message"] != "Method not found: resources/list" {
		t.Fatalf("message = %v", err["message"])
	}
}

func TestUnknownTool(t *testing.T) {
	s := store.NewStore()
	status, out := toolCall(t, s, 3, "mcp_no_such_tool", map[string]any{})
	if status != 200 {
		t.Fatalf("status = %d, want 200 (never 500)", status)
	}
	err := rpcError(t, out)
	if err["code"] != float64(-32602) {
		t.Fatalf("code = %v, want -32602", err["code"])
	}
	if err["message"] != "unknown tool mcp_no_such_tool" {
		t.Fatalf("message = %v", err["message"])
	}
}

func TestToolsCallNeedsName(t *testing.T) {
	s := store.NewStore()
	status, out := call(t, s, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"arguments":{}}}`)
	if status != 200 || rpcError(t, out)["code"] != float64(-32602) {
		t.Fatalf("status %d, out %v", status, out)
	}
}

func TestMissingArguments(t *testing.T) {
	s := store.NewStore()
	_, out := toolCall(t, s, 5, "create_issue", map[string]any{})
	result := at(t, out, "result")
	if got := at(t, result, "isError"); got != true {
		t.Fatalf("isError = %v, want true", got)
	}
	text := at(t, result, "content").([]any)[0].(map[string]any)["text"].(string)
	var content map[string]any
	if err := json.Unmarshal([]byte(text), &content); err != nil {
		t.Fatalf("tool text not JSON: %v", err)
	}
	if content["error"] != "missing required argument: owner" {
		t.Fatalf("error = %v", content["error"])
	}
}

func TestCreateRepoGetRepo(t *testing.T) {
	s := store.NewStore()
	requireStore(t, s)
	_, out := toolCall(t, s, 1, "create_repo", map[string]any{"name": "mcp-repo", "description": "d", "private": true})
	result := at(t, out, "result")
	if got := at(t, result, "isError"); got != false {
		t.Fatalf("create_repo isError = %v: %v", got, result)
	}
	sc := at(t, result, "structuredContent")
	if got := at(t, sc, "full_name"); got != "adaiversen/mcp-repo" {
		t.Fatalf("full_name = %v", got)
	}
	if got := at(t, sc, "private"); got != true {
		t.Fatalf("private = %v", got)
	}
	// Duplicate -> isError.
	_, out = toolCall(t, s, 2, "create_repo", map[string]any{"name": "mcp-repo"})
	if got := at(t, out, "result", "isError"); got != true {
		t.Fatalf("duplicate create_repo isError = %v", got)
	}
	// get_repo round-trips.
	_, out = toolCall(t, s, 3, "get_repo", map[string]any{"owner": "adaiversen", "repo": "mcp-repo"})
	if got := at(t, out, "result", "structuredContent", "name"); got != "mcp-repo" {
		t.Fatalf("get_repo name = %v", got)
	}
	_, out = toolCall(t, s, 4, "get_repo", map[string]any{"owner": "adaiversen", "repo": "nope"})
	if got := at(t, out, "result", "isError"); got != true {
		t.Fatalf("missing get_repo isError = %v", got)
	}
}

func TestCreateIssueListIssues(t *testing.T) {
	s := store.NewStore()
	requireStore(t, s)
	repo := s.CreateRepo("mcp-issues", false, "")
	if repo == nil {
		t.Fatal("CreateRepo returned nil")
	}
	_, out := toolCall(t, s, 1, "create_issue", map[string]any{"owner": "adaiversen", "repo": "mcp-issues", "title": "hello issue", "body": "b"})
	sc := at(t, out, "result", "structuredContent")
	if got := at(t, sc, "number"); got != float64(1) {
		t.Fatalf("issue number = %v", got)
	}
	if got := at(t, sc, "state"); got != "open" {
		t.Fatalf("issue state = %v", got)
	}
	if got := at(t, sc, "url"); got != "/api/v3/repos/adaiversen/mcp-issues/issues/1" {
		t.Fatalf("issue url = %v", got)
	}
	_, out = toolCall(t, s, 2, "list_issues", map[string]any{"owner": "adaiversen", "repo": "mcp-issues", "state": "open"})
	issues := at(t, out, "result", "structuredContent", "issues").([]any)
	if len(issues) != 1 || issues[0].(map[string]any)["title"] != "hello issue" {
		t.Fatalf("list_issues = %v", issues)
	}
	_, out = toolCall(t, s, 3, "list_issues", map[string]any{"owner": "adaiversen", "repo": "mcp-issues", "state": "closed"})
	if got := at(t, out, "result", "structuredContent", "issues").([]any); len(got) != 0 {
		t.Fatalf("closed list = %v, want empty", got)
	}
	_, out = toolCall(t, s, 4, "list_issues", map[string]any{"owner": "adaiversen", "repo": "mcp-issues", "state": "all"})
	if got := at(t, out, "result", "structuredContent", "issues").([]any); len(got) != 1 {
		t.Fatalf("all list = %v, want 1", got)
	}
}

func TestCreatePullRequestMerge(t *testing.T) {
	s := store.NewStore()
	requireStore(t, s)
	repo := s.CreateRepo("mcp-prs", false, "")
	if repo == nil {
		t.Fatal("CreateRepo returned nil")
	}
	seedBranches(t, repo)
	_, out := toolCall(t, s, 1, "create_pull_request", map[string]any{
		"owner": "adaiversen", "repo": "mcp-prs", "title": "pr", "head": "feature", "base": "main",
	})
	sc := at(t, out, "result", "structuredContent")
	if got := at(t, sc, "number"); got != float64(1) {
		t.Fatalf("PR number = %v (%v)", got, out)
	}
	if got := at(t, sc, "head"); got != "feature" {
		t.Fatalf("head = %v", got)
	}
	if got := at(t, sc, "headRefName"); got != "feature" {
		t.Fatalf("headRefName = %v", got)
	}
	// "owner:branch" head is split on ':'.
	_, out = toolCall(t, s, 2, "create_pull_request", map[string]any{
		"owner": "adaiversen", "repo": "mcp-prs", "title": "pr2", "head": "adaiversen:feature", "base": "main",
	})
	if got := at(t, out, "result", "structuredContent", "head"); got != "feature" {
		t.Fatalf("colon head = %v", got)
	}
	_, out = toolCall(t, s, 3, "merge_pull_request", map[string]any{"owner": "adaiversen", "repo": "mcp-prs", "pull_number": 1})
	sc = at(t, out, "result", "structuredContent")
	if got := at(t, sc, "merged"); got != true {
		t.Fatalf("merged = %v (%v)", got, out)
	}
	if got := at(t, sc, "state"); got != "closed" {
		t.Fatalf("state = %v", got)
	}
	if got := at(t, sc, "sha"); got == "" || got == nil {
		t.Fatalf("sha = %v", got)
	}
	// Merging a missing PR -> isError.
	_, out = toolCall(t, s, 4, "merge_pull_request", map[string]any{"owner": "adaiversen", "repo": "mcp-prs", "pull_number": 99})
	if got := at(t, out, "result", "isError"); got != true {
		t.Fatalf("missing PR merge isError = %v", got)
	}
}
