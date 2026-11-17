package rest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"forge/internal/store"
)

// ---------------------------------------------------------------------------
// Store-stub guard: the store package is still stubs (methods panic with
// "not implemented") until the store builder lands. Tests that drive
// HandleRest run through runWithStore, which turns that panic into a skip
// instead of a failure; pure-helper tests run unconditionally.
// ---------------------------------------------------------------------------

// runWithStore runs body against a fresh store. If the store stubs panic,
// the test is skipped (pending store implementation) rather than failed.
func runWithStore(t *testing.T, body func(s *store.Store)) {
	t.Helper()
	panicked := func() (p any) {
		defer func() { p = recover() }()
		body(store.NewStore())
		return nil
	}()
	if panicked != nil {
		if fmt.Sprint(panicked) == "not implemented" {
			t.Skip("store implementation pending (store stub panicked)")
		}
		panic(panicked)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("bad test URL %q: %v", raw, err)
	}
	return u
}

func call(s *store.Store, method, rawurl string, body string, path ...string) RestResponse {
	return HandleRest(RestContext{
		Store:  s,
		Method: method,
		URL:    mustURLForCall(rawurl),
		Path:   path,
		Body:   []byte(body),
	})
}

func mustURLForCall(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}

func decodeBody(t *testing.T, resp RestResponse) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(resp.Body, &v); err != nil {
		t.Fatalf("response body is not JSON (%d): %s: %v", resp.Status, resp.Body, err)
	}
	return v
}

func field(t *testing.T, v any, key string) any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected JSON object, got %T (%v)", v, v)
	}
	val, ok := m[key]
	if !ok {
		t.Fatalf("missing key %q in %v", key, m)
	}
	return val
}

// canonicalJSON parses JSON and re-marshals with sorted keys so two
// documents can be compared byte-for-byte regardless of key order.
func canonicalJSON(t *testing.T, raw []byte) []byte {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("not JSON: %s: %v", raw, err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("cannot re-marshal: %v", err)
	}
	return out
}

func expectJSONEqual(t *testing.T, what string, got []byte, want []byte) {
	t.Helper()
	g, w := canonicalJSON(t, got), canonicalJSON(t, want)
	if !bytes.Equal(g, w) {
		t.Errorf("%s: JSON mismatch\n got: %s\nwant: %s", what, g, w)
	}
}

// ---------------------------------------------------------------------------
// Pure helpers (run even while the store is still stubs).
// ---------------------------------------------------------------------------

func TestParseJSONBody(t *testing.T) {
	v, resp := ParseJSONBody(nil)
	if resp != nil {
		t.Fatalf("empty body: expected ok, got %v", resp)
	}
	if !reflect.DeepEqual(v, map[string]any{}) {
		t.Errorf("empty body: got %#v, want {}", v)
	}
	v, resp = ParseJSONBody([]byte(`{"a":1,"b":[true,null]}`))
	if resp != nil {
		t.Fatalf("valid body: expected ok, got %v", resp)
	}
	if field(t, v, "a") != float64(1) {
		t.Errorf("valid body: a = %v", field(t, v, "a"))
	}
	_, resp = ParseJSONBody([]byte(`{bad json`))
	if resp == nil {
		t.Fatal("malformed body: expected a 400 response")
	}
	if resp.Status != 400 {
		t.Errorf("malformed body: status %d, want 400", resp.Status)
	}
	if string(canonicalJSON(t, resp.Body)) != `{"message":"Problems parsing JSON"}` {
		t.Errorf("malformed body: body %s", resp.Body)
	}
	_, resp = ParseJSONBody([]byte(`{"a":1} trailing`))
	if resp == nil || resp.Status != 400 {
		t.Errorf("trailing garbage: expected 400, got %+v", resp)
	}
	v, resp = ParseJSONBody([]byte(`null`))
	if resp != nil || v != nil {
		t.Errorf("JSON null: got (%v, %v), want (nil, nil)", v, resp)
	}
}

func TestJSONBodyAndError(t *testing.T) {
	r := JSONBody(201, map[string]any{"b": 1, "a": "x<y>&z"}, map[string]string{"Link": `<>; rel="next"`})
	if r.Status != 201 {
		t.Errorf("status %d", r.Status)
	}
	// No HTML escaping: JSON.stringify does not escape < > &.
	if !strings.Contains(string(r.Body), "x<y>&z") {
		t.Errorf("body escapes HTML like Go default: %s", r.Body)
	}
	if r.Headers["Link"] != `<>; rel="next"` {
		t.Errorf("headers %v", r.Headers)
	}
	e := JSONError(404, "Not Found")
	if e.Status != 404 || string(e.Body) != `{"message":"Not Found"}` {
		t.Errorf("JSONError: %+v body %s", e, e.Body)
	}
	ve := validationError("Validation Failed", []map[string]any{
		{"resource": "Issue", "code": "custom", "field": "labels", "message": "label nope does not exist"},
	})
	if ve.Status != 422 {
		t.Errorf("validationError status %d", ve.Status)
	}
	expectJSONEqual(t, "validationError", ve.Body,
		[]byte(`{"message":"Validation Failed","errors":[{"resource":"Issue","code":"custom","field":"labels","message":"label nope does not exist"}]}`))
}

func TestQueryInt(t *testing.T) {
	cases := []struct {
		raw  string
		def  int
		want int
	}{
		{"", 1, 1},      // absent -> Number("")=0 -> || fallback
		{"abc", 1, 1},   // NaN -> fallback
		{"2.5", 1, 1},   // non-integer -> fallback
		{"0", 1, 1},     // 0 -> || fallback
		{"-3", 1, -3},   // negative passes through (paginate clamps)
		{"10", 30, 10},  // plain
		{" 7 ", 1, 7},   // JS Number trims whitespace
		{"1e2", 1, 100}, // JS Number parses exponents
		{"", 30, 30},    // per_page default
		{"0", 30, 30},   // per_page 0 -> default 30
	}
	for _, c := range cases {
		if got := queryInt(c.raw, c.def); got != c.want {
			t.Errorf("queryInt(%q, %d) = %d, want %d", c.raw, c.def, got, c.want)
		}
	}
}

func TestJsNumber(t *testing.T) {
	if f, ok := jsNumber(""); !ok || f != 0 {
		t.Errorf(`jsNumber("") = %v,%v want 0,true`, f, ok)
	}
	if _, ok := jsNumber("abc"); ok {
		t.Errorf(`jsNumber("abc") ok, want !ok`)
	}
	if f, ok := jsNumber("Infinity"); !ok || f != math.Inf(1) { // +Inf
		t.Errorf(`jsNumber("Infinity") = %v,%v`, f, ok)
	}
}

func TestParseIssueNumber(t *testing.T) {
	cases := []struct {
		s    string
		want int
		ok   bool
	}{
		{"1", 1, true}, {"01", 1, true}, {"abc", 0, false},
		{"1.5", 0, false}, {"", 0, true}, {" 3 ", 3, true},
		{"1e2", 100, true}, {"-1", -1, true}, {"Infinity", 0, false},
	}
	for _, c := range cases {
		n, ok := parseIssueNumber(c.s)
		if n != c.want || ok != c.ok {
			t.Errorf("parseIssueNumber(%q) = (%d,%v), want (%d,%v)", c.s, n, ok, c.want, c.ok)
		}
	}
}

func TestParseIdent(t *testing.T) {
	// Reproduces the TS quirk: only the FIRST '>' is stripped, so the
	// trailing epoch stays in the email (verified against Node golden 27).
	name, email := parseIdent("ada iversen <adaiversen@example.invalid> 1700000001 +0000")
	if name != "ada iversen" || email != "adaiversen@example.invalid 1700000001 +0000" {
		t.Errorf("parseIdent full: got (%q,%q)", name, email)
	}
	name, email = parseIdent("plain name")
	if name != "plain name" || email != "" {
		t.Errorf("parseIdent plain: got (%q,%q)", name, email)
	}
	name, email = parseIdent("a <b@c>")
	if name != "a" || email != "b@c" {
		t.Errorf("parseIdent short: got (%q,%q)", name, email)
	}
}

func TestDecodeBase64Lenient(t *testing.T) {
	if got := string(decodeBase64Lenient("aGVsbG8gd29ybGQ=")); got != "hello world" {
		t.Errorf("decode: %q", got)
	}
	// Node ignores non-alphabet chars.
	if got := string(decodeBase64Lenient("aGVs\nbG8=")); got != "hello" {
		t.Errorf("decode with newline: %q", got)
	}
}

func TestPaginate(t *testing.T) {
	u := mustURL(t, "http://127.0.0.1:41365/api/v3/repos/o/r/issues?per_page=2&page=1")
	items := []int{1, 2, 3, 4, 5}
	pageItems, headers := paginate(u, items, 1, 2)
	if !reflect.DeepEqual(pageItems, []int{1, 2}) {
		t.Errorf("page 1 items %v", pageItems)
	}
	want := `<http://127.0.0.1:41365/api/v3/repos/o/r/issues?per_page=2&page=2>; rel="next", ` +
		`<http://127.0.0.1:41365/api/v3/repos/o/r/issues?per_page=2&page=3>; rel="last"`
	if headers["Link"] != want {
		t.Errorf("page 1 Link:\n got: %s\nwant: %s", headers["Link"], want)
	}

	u2 := mustURL(t, "http://127.0.0.1:41365/api/v3/repos/o/r/issues?per_page=2&page=2")
	pageItems, headers = paginate(u2, items, 2, 2)
	if !reflect.DeepEqual(pageItems, []int{3, 4}) {
		t.Errorf("page 2 items %v", pageItems)
	}
	want = `<http://127.0.0.1:41365/api/v3/repos/o/r/issues?per_page=2&page=3>; rel="next", ` +
		`<http://127.0.0.1:41365/api/v3/repos/o/r/issues?per_page=2&page=3>; rel="last", ` +
		`<http://127.0.0.1:41365/api/v3/repos/o/r/issues?per_page=2&page=1>; rel="first", ` +
		`<http://127.0.0.1:41365/api/v3/repos/o/r/issues?per_page=2&page=1>; rel="prev"`
	if headers["Link"] != want {
		t.Errorf("page 2 Link:\n got: %s\nwant: %s", headers["Link"], want)
	}

	// Last page, then an out-of-range page (empty items, still last/first/prev).
	u3 := mustURL(t, "http://127.0.0.1:41365/api/v3/repos/o/r/issues?per_page=2&page=3")
	pageItems, headers = paginate(u3, items, 3, 2)
	if !reflect.DeepEqual(pageItems, []int{5}) {
		t.Errorf("page 3 items %v", pageItems)
	}
	if !strings.Contains(headers["Link"], `rel="last"`) || strings.Contains(headers["Link"], `rel="next"`) {
		t.Errorf("page 3 Link: %s", headers["Link"])
	}
	u9 := mustURL(t, "http://127.0.0.1:41365/api/v3/repos/o/r/issues?per_page=2&page=9")
	pageItems, headers = paginate(u9, items, 9, 2)
	if len(pageItems) != 0 {
		t.Errorf("page 9 items %v, want empty", pageItems)
	}
	if !strings.Contains(headers["Link"], `page=8>; rel="prev"`) {
		t.Errorf("page 9 Link: %s", headers["Link"])
	}

	// Single page: no Link header at all.
	us := mustURL(t, "http://127.0.0.1:41365/api/v3/user/repos?per_page=10")
	_, headers = paginate(us, []int{1}, 1, 10)
	if len(headers) != 0 {
		t.Errorf("single page headers %v, want none", headers)
	}

	// Other query params are preserved in place; page/per_page appended.
	many := make([]int, 150)
	uq := mustURL(t, "http://127.0.0.1:41365/api/v3/repos/o/r/issues?state=open")
	_, headers = paginate(uq, many, 1, 30)
	if !strings.HasPrefix(headers["Link"], `<http://127.0.0.1:41365/api/v3/repos/o/r/issues?state=open&page=2&per_page=30>; rel="next"`) {
		t.Errorf("params preserved: %s", headers["Link"])
	}

	// per_page clamps to 100, page clamps to >= 1.
	_, headers = paginate(us, many, 0, 500)
	if !strings.Contains(headers["Link"], "per_page=100") {
		t.Errorf("per_page clamp: %s", headers["Link"])
	}
}

// ---------------------------------------------------------------------------
// HandleRest flows (need the store; skipped until it lands).
// ---------------------------------------------------------------------------

func TestRestFlows(t *testing.T) {
	runWithStore(t, func(s *store.Store) {
		base := "http://127.0.0.1:41365/api/v3"

		// Create repo: 201 + shape.
		r := call(s, "POST", base+"/user/repos", `{"name":"flow-repo","private":true,"description":"d"}`, "user", "repos")
		if r.Status != 201 {
			t.Fatalf("create repo: status %d body %s", r.Status, r.Body)
		}
		body := decodeBody(t, r)
		if field(t, body, "name") != "flow-repo" || field(t, body, "private") != true {
			t.Errorf("create repo shape: %s", r.Body)
		}
		if field(t, body, "default_branch") != "main" {
			t.Errorf("default_branch: %v", field(t, body, "default_branch"))
		}
		if field(t, body, "id") != float64(1) {
			t.Errorf("repo id: %v", field(t, body, "id"))
		}

		// Duplicate repo: 422.
		r = call(s, "POST", base+"/user/repos", `{"name":"flow-repo"}`, "user", "repos")
		if r.Status != 422 {
			t.Errorf("duplicate repo: status %d, want 422 (%s)", r.Status, r.Body)
		}

		// Get repo + stranger 404 + missing repo 404.
		r = call(s, "GET", base+"/repos/adaiversen/flow-repo", "", "repos", "adaiversen", "flow-repo")
		if r.Status != 200 {
			t.Errorf("get repo: status %d", r.Status)
		}
		r = call(s, "GET", base+"/repos/olgabauer/flow-repo", "", "repos", "olgabauer", "flow-repo")
		if r.Status != 404 {
			t.Errorf("stranger repo: status %d, want 404", r.Status)
		}
		expectJSONEqual(t, "stranger body", r.Body, []byte(`{"message":"Not Found"}`))
		r = call(s, "GET", base+"/repos/adaiversen/nope", "", "repos", "adaiversen", "nope")
		if r.Status != 404 {
			t.Errorf("missing repo: status %d, want 404", r.Status)
		}

		// Invalid repo name: 422.
		r = call(s, "POST", base+"/user/repos", `{"name":"bad name!"}`, "user", "repos")
		if r.Status != 422 {
			t.Errorf("invalid name: status %d, want 422", r.Status)
		}

		// Create/get/list/patch issue, incl. label validation 422.
		repoPath := []string{"repos", "adaiversen", "flow-repo", "issues"}
		r = call(s, "POST", base+"/repos/adaiversen/flow-repo/issues", `{"title":"T1","body":"B1","labels":["nope"]}`, repoPath...)
		if r.Status != 422 {
			t.Errorf("unknown label: status %d, want 422 (%s)", r.Status, r.Body)
		}
		r = call(s, "POST", base+"/repos/adaiversen/flow-repo/issues", `{"title":"T1","body":"B1","labels":["bug"]}`, repoPath...)
		if r.Status != 201 {
			t.Fatalf("create issue: status %d body %s", r.Status, r.Body)
		}
		issue := decodeBody(t, r)
		if field(t, issue, "number") != float64(1) || field(t, issue, "state") != "open" {
			t.Errorf("issue shape: %s", r.Body)
		}
		labels := field(t, issue, "labels").([]any)
		if len(labels) != 1 || field(t, labels[0], "name") != "bug" {
			t.Errorf("issue labels: %v", labels)
		}
		if field(t, issue, "closed_at") != nil {
			t.Errorf("closed_at: %v, want null", field(t, issue, "closed_at"))
		}
		// Missing title: 422.
		r = call(s, "POST", base+"/repos/adaiversen/flow-repo/issues", `{"body":"x"}`, repoPath...)
		if r.Status != 422 {
			t.Errorf("missing title: status %d, want 422", r.Status)
		}
		// Issues 2 and 3 for pagination.
		call(s, "POST", base+"/repos/adaiversen/flow-repo/issues", `{"title":"T2","body":"B2"}`, repoPath...)
		call(s, "POST", base+"/repos/adaiversen/flow-repo/issues", `{"title":"T3","body":"B3"}`, repoPath...)

		// Pagination Link header on a 3-issue list, per_page=2.
		r = call(s, "GET", base+"/repos/adaiversen/flow-repo/issues?per_page=2&page=1", "", repoPath...)
		if r.Status != 200 {
			t.Fatalf("list issues: %d", r.Status)
		}
		list := decodeBody(t, r).([]any)
		if len(list) != 2 {
			t.Errorf("page 1: %d issues, want 2", len(list))
		}
		if field(t, list[0], "number") != float64(3) {
			t.Errorf("default sort is newest first: first number %v", field(t, list[0], "number"))
		}
		if !strings.Contains(r.Headers["Link"], `rel="next"`) || !strings.Contains(r.Headers["Link"], `rel="last"`) {
			t.Errorf("page 1 Link: %q", r.Headers["Link"])
		}
		// Ascending sort.
		r = call(s, "GET", base+"/repos/adaiversen/flow-repo/issues?sort=created&direction=asc&per_page=100", "", repoPath...)
		list = decodeBody(t, r).([]any)
		if field(t, list[0], "number") != float64(1) || field(t, list[2], "number") != float64(3) {
			t.Errorf("asc sort: %v", list)
		}

		// Get one + patch close (closed_at set, state closed).
		one := []string{"repos", "adaiversen", "flow-repo", "issues", "1"}
		r = call(s, "GET", base+"/repos/adaiversen/flow-repo/issues/1", "", one...)
		if r.Status != 200 {
			t.Errorf("get issue: %d", r.Status)
		}
		r = call(s, "PATCH", base+"/repos/adaiversen/flow-repo/issues/1", `{"state":"closed"}`, one...)
		if r.Status != 200 {
			t.Fatalf("patch issue: %d %s", r.Status, r.Body)
		}
		patched := decodeBody(t, r)
		if field(t, patched, "state") != "closed" || field(t, patched, "closed_at") == nil {
			t.Errorf("patched issue: %s", r.Body)
		}
		// state=open list now excludes it.
		r = call(s, "GET", base+"/repos/adaiversen/flow-repo/issues?state=open&per_page=100", "", repoPath...)
		for _, item := range decodeBody(t, r).([]any) {
			if field(t, item, "number") == float64(1) {
				t.Errorf("state=open lists closed issue 1")
			}
		}
		// Bad issue number: 404.
		r = call(s, "GET", base+"/repos/adaiversen/flow-repo/issues/abc", "", "repos", "adaiversen", "flow-repo", "issues", "abc")
		if r.Status != 404 {
			t.Errorf("non-numeric issue: %d, want 404", r.Status)
		}

		// Comments: add + list.
		comments := []string{"repos", "adaiversen", "flow-repo", "issues", "2", "comments"}
		r = call(s, "POST", base+"/repos/adaiversen/flow-repo/issues/2/comments", `{"body":"hello"}`, comments...)
		if r.Status != 201 {
			t.Fatalf("add comment: %d %s", r.Status, r.Body)
		}
		if field(t, decodeBody(t, r), "body") != "hello" {
			t.Errorf("comment body: %s", r.Body)
		}
		r = call(s, "POST", base+"/repos/adaiversen/flow-repo/issues/2/comments", `{"body":""}`, comments...)
		if r.Status != 422 {
			t.Errorf("empty comment: %d, want 422", r.Status)
		}
		r = call(s, "GET", base+"/repos/adaiversen/flow-repo/issues/2/comments", "", comments...)
		clist := decodeBody(t, r).([]any)
		if len(clist) != 1 {
			t.Errorf("comments list: %d, want 1", len(clist))
		}

		// Contents: PUT (201) + GET + PUT update (200) + stale sha (409).
		contents := []string{"repos", "adaiversen", "flow-repo", "contents", "README.md"}
		r = call(s, "PUT", base+"/repos/adaiversen/flow-repo/contents/README.md",
			`{"message":"add readme","content":"aGVsbG8gd29ybGQ="}`, contents...)
		if r.Status != 201 {
			t.Fatalf("contents PUT: %d %s", r.Status, r.Body)
		}
		putBody := decodeBody(t, r)
		blobSha := field(t, field(t, putBody, "content"), "sha").(string)
		if field(t, field(t, putBody, "content"), "encoding") != "base64" {
			t.Errorf("contents PUT encoding: %s", r.Body)
		}
		r = call(s, "GET", base+"/repos/adaiversen/flow-repo/contents/README.md", "", contents...)
		if r.Status != 200 || field(t, decodeBody(t, r), "content") != "aGVsbG8gd29ybGQ=" {
			t.Errorf("contents GET: %d %s", r.Status, r.Body)
		}
		r = call(s, "PUT", base+"/repos/adaiversen/flow-repo/contents/README.md",
			`{"message":"upd","content":"aGVsbG8=","sha":"`+blobSha+`"}`, contents...)
		if r.Status != 200 {
			t.Errorf("contents PUT update: %d, want 200 (%s)", r.Status, r.Body)
		}
		r = call(s, "PUT", base+"/repos/adaiversen/flow-repo/contents/README.md",
			`{"message":"upd","content":"aGVsbG8=","sha":"`+blobSha+`"}`, contents...)
		if r.Status != 409 {
			t.Errorf("contents PUT stale sha: %d, want 409", r.Status)
		}

		// Pulls: create (number 4: issues 1-3 share the counter), get,
		// list, files, merge. Head == base here, so files is empty and
		// the merge is a no-op fast-forward the store accepts.
		pulls := []string{"repos", "adaiversen", "flow-repo", "pulls"}
		r = call(s, "POST", base+"/repos/adaiversen/flow-repo/pulls",
			`{"title":"PR","body":"pb","head":"main","base":"main"}`, pulls...)
		if r.Status != 201 {
			t.Fatalf("create pull: %d %s", r.Status, r.Body)
		}
		pull := decodeBody(t, r)
		if field(t, pull, "number") != float64(4) {
			t.Errorf("pull number: %v, want 4 (shared counter)", field(t, pull, "number"))
		}
		if field(t, pull, "mergeable_state") != "clean" {
			t.Errorf("mergeable_state: %v, want clean", field(t, pull, "mergeable_state"))
		}
		head := field(t, pull, "head").(map[string]any)
		if head["ref"] != "main" || head["sha"] == "" || head["sha"] == nil {
			t.Errorf("pull head: %v", head)
		}
		onePull := []string{"repos", "adaiversen", "flow-repo", "pulls", "4"}
		r = call(s, "GET", base+"/repos/adaiversen/flow-repo/pulls/4", "", onePull...)
		if r.Status != 200 {
			t.Errorf("get pull: %d", r.Status)
		}
		r = call(s, "GET", base+"/repos/adaiversen/flow-repo/pulls/99", "", "repos", "adaiversen", "flow-repo", "pulls", "99")
		if r.Status != 404 {
			t.Errorf("unknown pull: %d, want 404", r.Status)
		}
		r = call(s, "GET", base+"/repos/adaiversen/flow-repo/pulls?state=all", "", pulls...)
		if len(decodeBody(t, r).([]any)) != 1 {
			t.Errorf("pulls state=all: %s", r.Body)
		}
		r = call(s, "GET", base+"/repos/adaiversen/flow-repo/pulls/4/files", "", "repos", "adaiversen", "flow-repo", "pulls", "4", "files")
		if r.Status != 200 {
			t.Errorf("pull files: %d", r.Status)
		}
		r = call(s, "PUT", base+"/repos/adaiversen/flow-repo/pulls/4/merge", `{"merge_method":"squash"}`,
			"repos", "adaiversen", "flow-repo", "pulls", "4", "merge")
		if r.Status != 200 {
			t.Errorf("merge pull: %d %s", r.Status, r.Body)
		} else {
			merged := decodeBody(t, r)
			if field(t, merged, "merged") != true {
				t.Errorf("merge body: %s", r.Body)
			}
		}
		r = call(s, "GET", base+"/repos/adaiversen/flow-repo/pulls/4", "", onePull...)
		after := decodeBody(t, r)
		if field(t, after, "state") != "closed" || field(t, after, "merged") != true ||
			field(t, after, "mergeable_state") != "merged" {
			t.Errorf("merged pull: %s", r.Body)
		}

		// Root / meta / user / unknown route / malformed JSON.
		r = call(s, "GET", base+"/", "", []string{}...)
		if r.Status != 200 || r.Headers["x-oauth-scopes"] != "repo, read:org" {
			t.Errorf("root: %d headers %v", r.Status, r.Headers)
		}
		r = call(s, "GET", base+"/meta", "", "meta")
		if r.Status != 200 || field(t, decodeBody(t, r), "installed_version") != "1.0.0" {
			t.Errorf("meta: %d %s", r.Status, r.Body)
		}
		r = call(s, "GET", base+"/user", "", "user")
		if r.Status != 200 || field(t, decodeBody(t, r), "login") != "adaiversen" {
			t.Errorf("user: %d %s", r.Status, r.Body)
		}
		r = call(s, "GET", base+"/nonexistent", "", "nonexistent")
		if r.Status != 404 {
			t.Errorf("unknown route: %d, want 404", r.Status)
		}
		r = call(s, "POST", base+"/user/repos", `{bad json`, "user", "repos")
		if r.Status != 400 {
			t.Errorf("malformed JSON: %d, want 400", r.Status)
		}

		// PATCH + DELETE repo.
		repoOnly := []string{"repos", "adaiversen", "flow-repo"}
		r = call(s, "PATCH", base+"/repos/adaiversen/flow-repo", `{"description":"new desc"}`, repoOnly...)
		if r.Status != 200 || field(t, decodeBody(t, r), "description") != "new desc" {
			t.Errorf("patch repo: %d %s", r.Status, r.Body)
		}
		r = call(s, "DELETE", base+"/repos/adaiversen/flow-repo", "", repoOnly...)
		if r.Status != 204 {
			t.Errorf("delete repo: %d, want 204", r.Status)
		}
		r = call(s, "GET", base+"/repos/adaiversen/flow-repo", "", repoOnly...)
		if r.Status != 404 {
			t.Errorf("get after delete: %d, want 404", r.Status)
		}
	})
}

// ---------------------------------------------------------------------------
// Node-vs-Go golden replay.
//
// The fixtures in testdata/node-golden were captured from the real Node
// entry (LOCAL_DEV=1, fresh server) by replaying the exact request sequence
// below. This test replays the same sequence against HandleRest on a fresh
// Go store and compares every response body byte-for-byte after JSON
// key-sorting, plus status codes and Link headers. It needs the store.
// ---------------------------------------------------------------------------

type goldenStep struct {
	method string
	url    string
	body   string
	path   []string
	// golden file with the expected body (and, for .txt fixtures, the
	// expected status on the last line); "" means no body comparison.
	golden string
	// golden header file to compare the Link header against ("" = skip).
	linkGolden string
}

func TestNodeGoldenReplay(t *testing.T) {
	runWithStore(t, func(s *store.Store) {
		// The commit sha produced by step 18 (contents PUT); filled in
		// from the golden fixture itself so later steps use the real sha.
		putGolden := readGolden(t, "18-contents-put.json")
		var putBody struct {
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		if err := json.Unmarshal(putGolden, &putBody); err != nil {
			t.Fatalf("golden 18: %v", err)
		}
		commitSha := putBody.Commit.SHA

		base := "http://127.0.0.1:41365/api/v3"
		repo := base + "/repos/adaiversen/golden-repo"
		steps := []goldenStep{
			{"GET", base + "/", "", nil, "01-root.json", ""},
			{"GET", base + "/meta", "", []string{"meta"}, "02-meta.json", ""},
			{"POST", base + "/user/repos", `{"name":"golden-repo","private":true,"description":"golden desc"}`, []string{"user", "repos"}, "03-create-repo.json", ""},
			{"POST", base + "/user/repos", `{"name":"golden-repo"}`, []string{"user", "repos"}, "04-dup-repo.txt", ""},
			{"GET", repo, "", []string{"repos", "adaiversen", "golden-repo"}, "05-get-repo.json", ""},
			{"GET", base + "/repos/olgabauer/golden-repo", "", []string{"repos", "olgabauer", "golden-repo"}, "06-stranger.txt", ""},
			{"POST", repo + "/labels", `{"name":"bug","color":"d73a4a"}`, []string{"repos", "adaiversen", "golden-repo", "labels"}, "07-label.json", ""},
			{"POST", repo + "/issues", `{"title":"First issue","body":"hello body","labels":["bug"]}`, []string{"repos", "adaiversen", "golden-repo", "issues"}, "08-issue1.json", ""},
			{"POST", repo + "/issues", `{"title":"Second issue","body":"b2"}`, []string{"repos", "adaiversen", "golden-repo", "issues"}, "09-issue2.json", ""},
			{"POST", repo + "/issues", `{"title":"Third issue","body":"b3"}`, []string{"repos", "adaiversen", "golden-repo", "issues"}, "10-issue3.json", ""},
			{"POST", repo + "/issues", `{"title":"Bad","body":"x","labels":["nope"]}`, []string{"repos", "adaiversen", "golden-repo", "issues"}, "11-badlabel.txt", ""},
			{"GET", repo + "/issues?per_page=2&page=1", "", []string{"repos", "adaiversen", "golden-repo", "issues"}, "12-list-p1.json", "12-list-headers.txt"},
			{"GET", repo + "/issues?per_page=2&page=2", "", []string{"repos", "adaiversen", "golden-repo", "issues"}, "13-list-p2.json", "13-list-headers.txt"},
			{"GET", repo + "/issues/1", "", []string{"repos", "adaiversen", "golden-repo", "issues", "1"}, "14-get-issue1.json", ""},
			{"PATCH", repo + "/issues/1", `{"state":"closed"}`, []string{"repos", "adaiversen", "golden-repo", "issues", "1"}, "15-patch-issue1.json", ""},
			{"POST", repo + "/issues/2/comments", `{"body":"a comment"}`, []string{"repos", "adaiversen", "golden-repo", "issues", "2", "comments"}, "16-comment.json", ""},
			{"GET", repo + "/issues/2/comments", "", []string{"repos", "adaiversen", "golden-repo", "issues", "2", "comments"}, "17-comments-list.json", ""},
			{"PUT", repo + "/contents/README.md", `{"message":"add readme","content":"aGVsbG8gd29ybGQ="}`, []string{"repos", "adaiversen", "golden-repo", "contents", "README.md"}, "18-contents-put.json", ""},
			{"GET", repo + "/contents/README.md", "", []string{"repos", "adaiversen", "golden-repo", "contents", "README.md"}, "19-contents-get.json", ""},
			{"GET", base + "/user", "", []string{"user"}, "20-user.json", ""},
			{"POST", base + "/user/repos", `{bad json`, []string{"user", "repos"}, "21-malformed.txt", ""},
			{"GET", base + "/nonexistent", "", []string{"nonexistent"}, "22-unknown.txt", ""},
			{"GET", repo + "/labels", "", []string{"repos", "adaiversen", "golden-repo", "labels"}, "23-labels-list.json", ""},
			{"POST", repo + "/git/refs", `{"ref":"refs/heads/feature","sha":"` + commitSha + `"}`, []string{"repos", "adaiversen", "golden-repo", "git", "refs"}, "24-ref.json", ""},
			{"GET", repo + "/branches", "", []string{"repos", "adaiversen", "golden-repo", "branches"}, "25-branches.json", ""},
			{"GET", repo + "/commits", "", []string{"repos", "adaiversen", "golden-repo", "commits"}, "26-commits.json", ""},
			{"GET", repo + "/commits/" + commitSha, "", []string{"repos", "adaiversen", "golden-repo", "commits", commitSha}, "27-commit.json", ""},
			{"POST", repo + "/pulls", `{"title":"My PR","body":"pr body","head":"feature","base":"main"}`, []string{"repos", "adaiversen", "golden-repo", "pulls"}, "28-pull.json", ""},
			// GET /pulls/1 is a 404: the pull is number 4 (issues 1-3 share
			// the repo's number counter).
			{"GET", repo + "/pulls/1", "", []string{"repos", "adaiversen", "golden-repo", "pulls", "1"}, "29-pull-get.json", ""},
			{"GET", repo + "/pulls/4/files", "", []string{"repos", "adaiversen", "golden-repo", "pulls", "4", "files"}, "30-pull-files.json", ""},
			{"GET", repo + "/pulls/4/commits", "", []string{"repos", "adaiversen", "golden-repo", "pulls", "4", "commits"}, "30b-pull-commits.json", ""},
			{"PUT", repo + "/pulls/4/merge", `{"merge_method":"squash"}`, []string{"repos", "adaiversen", "golden-repo", "pulls", "4", "merge"}, "31-pull-merge.json", ""},
			{"GET", repo + "/pulls/4", "", []string{"repos", "adaiversen", "golden-repo", "pulls", "4"}, "32-pull-merged.json", ""},
			{"GET", repo + "/contents/", "", []string{"repos", "adaiversen", "golden-repo", "contents"}, "33-contents-root.json", ""},
			{"GET", base + "/user/repos?per_page=10", "", []string{"user", "repos"}, "34-userrepos.json", "34-userrepos-headers.txt"},
		}
		for i, step := range steps {
			resp := call(s, step.method, step.url, step.body, step.path...)
			wantBody, wantStatus := readGoldenStatus(t, step.golden)
			if resp.Status != wantStatus {
				t.Errorf("step %d (%s %s): status %d, want %d (body %s)",
					i+1, step.method, step.url, resp.Status, wantStatus, resp.Body)
				continue
			}
			expectJSONEqual(t, fmt.Sprintf("step %d (%s %s)", i+1, step.method, step.url), resp.Body, wantBody)
			if step.linkGolden != "" {
				wantLink := linkHeaderFromFile(t, step.linkGolden)
				if resp.Headers["Link"] != wantLink {
					t.Errorf("step %d (%s %s): Link header\n got: %q\nwant: %q",
						i+1, step.method, step.url, resp.Headers["Link"], wantLink)
				}
			}
		}
	})
}

func goldenDir(t *testing.T) string {
	t.Helper()
	return filepath.Join("testdata", "node-golden")
}

func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(goldenDir(t), name))
	if err != nil {
		t.Fatalf("golden %s: %v", name, err)
	}
	return raw
}

// readGoldenStatus reads a .json fixture (status implied by name) or a .txt
// fixture (body + "\n" + status, as captured with curl -w).
func readGoldenStatus(t *testing.T, name string) ([]byte, int) {
	t.Helper()
	raw := readGolden(t, name)
	if strings.HasSuffix(name, ".txt") {
		idx := strings.LastIndexByte(string(raw), '\n')
		if idx < 0 {
			t.Fatalf("golden %s: no status line", name)
		}
		var status int
		if _, err := fmt.Sscanf(string(raw[idx+1:]), "%d", &status); err != nil {
			t.Fatalf("golden %s: bad status: %v", name, err)
		}
		return raw[:idx], status
	}
	// Status implied by the fixture: 201 for creates, else 200. The
	// per-step table knows the real expectation; encode it in the name map.
	status := 200
	switch name {
	case "03-create-repo.json", "07-label.json", "08-issue1.json",
		"09-issue2.json", "10-issue3.json", "16-comment.json",
		"18-contents-put.json", "24-ref.json", "28-pull.json":
		status = 201
	case "29-pull-get.json": // GET /pulls/1: the pull is number 4
		status = 404
	}
	return raw, status
}

// linkHeaderFromFile extracts the `link:` header value from a curl -D dump.
func linkHeaderFromFile(t *testing.T, name string) string {
	t.Helper()
	raw := string(readGolden(t, name))
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(strings.ToLower(line), "link:") {
			return strings.TrimSpace(line[len("link:"):])
		}
	}
	return ""
}
