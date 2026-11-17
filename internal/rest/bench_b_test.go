// Microbenchmarks for the REST read/write hot paths (Builder B).
// These drive HandleRest in-process with the exact shapes the graded
// workloads use: read = sequential GET of one warm issue (no query);
// write = sequential POST {"title","body"} creates (no labels).
package rest

import (
	"encoding/json"
	"fmt"
	"net/url"
	"testing"

	"forge/internal/store"
)

const benchTitle = "Fix login redirect loop on mobile Safari"
const benchBody = "When the session cookie expires mid-flow the app redirects to /login, which bounces back to the dashboard and loops forever."

func benchStoreWithIssue(b *testing.B) (*store.Store, *store.Repo) {
	b.Helper()
	s := store.NewStore()
	repo := s.CreateRepo("bench-read", false, "")
	if repo == nil {
		b.Fatal("CreateRepo failed")
	}
	s.CreateIssue(repo, benchTitle, benchBody, nil)
	return s, repo
}

func benchURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}

// BenchmarkReadSingleIssue: the graded read workload, in-process.
func BenchmarkReadSingleIssue(b *testing.B) {
	s, _ := benchStoreWithIssue(b)
	u := benchURL("http://127.0.0.1:41365/api/v3/repos/adaiversen/bench-read/issues/1")
	path := []string{"repos", "adaiversen", "bench-read", "issues", "1"}
	// Warm the cached JSON exactly like the workload's warm-ups.
	resp := HandleRest(RestContext{Store: s, Method: "GET", URL: u, Path: path})
	if resp.Status != 200 {
		b.Fatalf("warm-up GET: %d %s", resp.Status, resp.Body)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp := HandleRest(RestContext{Store: s, Method: "GET", URL: u, Path: path})
		if resp.Status != 200 {
			b.Fatalf("GET: %d", resp.Status)
		}
	}
}

// BenchmarkWriteCreateIssue: the graded write workload, in-process.
// The repo is recreated every 200 creates (as the graded workload does)
// so the issue map stays bounded.
func BenchmarkWriteCreateIssue(b *testing.B) {
	s := store.NewStore()
	repo := s.CreateRepo("bench-write", false, "")
	if repo == nil {
		b.Fatal("CreateRepo failed")
	}
	u := benchURL("http://127.0.0.1:41365/api/v3/repos/adaiversen/bench-write/issues")
	path := []string{"repos", "adaiversen", "bench-write", "issues"}
	payloads := make([][]byte, 200)
	for i := range payloads {
		payloads[i] = []byte(fmt.Sprintf(`{"title":%q,"body":%q}`, benchTitle+fmt.Sprintf(" [bench %d]", i+1), benchBody))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%200 == 0 {
			s.DeleteRepo("adaiversen", "bench-write")
			repo = s.CreateRepo("bench-write", false, "")
			_ = repo
		}
		resp := HandleRest(RestContext{Store: s, Method: "POST", URL: u, Path: path, Body: payloads[i%200]})
		if resp.Status != 201 {
			b.Fatalf("POST: %d %s", resp.Status, resp.Body)
		}
	}
}

// BenchmarkTypedIssueCreateDecode: typed struct decode of the graded write
// payload (the fast path in handleIssues POST).
func BenchmarkTypedIssueCreateDecode(b *testing.B) {
	body := []byte(fmt.Sprintf(`{"title":%q,"body":%q}`, benchTitle, benchBody))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var fast issueCreateBody
		if err := json.Unmarshal(body, &fast); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkParseJSONBody: generic map decode of the graded write payload.
func BenchmarkParseJSONBody(b *testing.B) {
	body := []byte(fmt.Sprintf(`{"title":%q,"body":%q}`, benchTitle, benchBody))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		v, resp := ParseJSONBody(body)
		if resp != nil || v == nil {
			b.Fatal("parse failed")
		}
	}
}

// BenchmarkIssueShapeMarshal: build + marshal a fresh issue shape (the
// per-POST serialization cost; the cached path is BenchmarkReadSingleIssue).
func BenchmarkIssueShapeMarshal(b *testing.B) {
	_, repo := benchStoreWithIssue(b)
	issue := repo.Issues[1]
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = marshalJSON(issueJSONValue(repo, issue))
	}
}

// BenchmarkSeqTimestamp: the deterministic clock used per create.
func BenchmarkSeqTimestamp(b *testing.B) {
	s := store.NewStore()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.SeqTimestamp()
	}
}

// BenchmarkGlobalID: the base64 node id used per create.
func BenchmarkGlobalID(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = store.GlobalID("Issue", 1, i)
	}
}

// benchSink defeats dead-code elimination so the copy is really made.
var benchSink int

//go:noinline
func copyStringBytes(s string) []byte { return []byte(s) }

// BenchmarkIssueResponseCopy: the []byte(string) copy in issueResponse.
// (A plain []byte(s) in the loop is elided by the compiler when only read,
// so the copy is forced through a noinline helper, as in production where
// the bytes escape into the response.)
func BenchmarkIssueResponseCopy(b *testing.B) {
	_, repo := benchStoreWithIssue(b)
	issue := repo.Issues[1]
	jsonText := issueJSON(repo, issue)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bts := copyStringBytes(jsonText)
		benchSink += int(bts[0]) + int(bts[len(bts)-1])
	}
}
