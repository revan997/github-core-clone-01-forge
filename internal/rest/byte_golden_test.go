// Byte-exact differential goldens for the REST read/write paths (Builder B).
//
// Captures the RAW response bytes (status + body) for the graded shapes
// before any optimization, so a later change can prove byte-identity.
// Regenerate with: REGENERATE_GOLDENS=1 go test ./internal/rest/ -run TestByteGoldens
package rest

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"forge/internal/store"
)

func goldenBytesPath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("testdata", "byte-goldens", name)
}

func checkGoldenBytes(t *testing.T, name string, got []byte) {
	t.Helper()
	path := goldenBytesPath(t, name)
	if os.Getenv("REGENERATE_GOLDENS") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden %s missing (run with REGENERATE_GOLDENS=1): %v", name, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("golden %s mismatch:\n got: %s\nwant: %s", name, got, want)
	}
}

func TestByteGoldens(t *testing.T) {
	// --- issue JSON bytes: one repo, several issue shapes ---
	s := store.NewStore()
	repo := s.CreateRepo("golden-bytes", false, "")
	if repo == nil {
		t.Fatal("CreateRepo failed")
	}
	// 1: plain, no labels (the graded write/read shape).
	i1 := s.CreateIssue(repo, "Fix login redirect", "Body text here.", nil)
	// 2: labels + special chars in title/body (escaping parity).
	i2 := s.CreateIssue(repo, `Quote " back\slash <tag> & amp — ünïcode sep`, "line1\nline2\ttab", []string{"bug", "enhancement"})
	// 3: closed, with comments.
	i3 := s.CreateIssue(repo, "Closed one", "", []string{"question"})
	s.AddComment(repo, i3, "first comment")
	s.AddComment(repo, i3, "second")
	i3.State = "closed"
	closedAt := s.SeqTimestamp()
	i3.ClosedAt = &closedAt
	i3.UpdatedAt = s.SeqTimestamp()
	s.TouchIssue(i3)
	// 4: empty body, no labels.
	i4 := s.CreateIssue(repo, "No body", "", nil)

	for n, issue := range map[int]*store.Issue{1: i1, 2: i2, 3: i3, 4: i4} {
		r := issueResponse(repo, issue, 200)
		checkGoldenBytes(t, fmt.Sprintf("issue-%d.json", n), r.Body)
		if r.Status != 200 {
			t.Errorf("issue %d status %d", n, r.Status)
		}
	}
	// List JSON: newest-first page of all four.
	list := issueListJSON(repo, []*store.Issue{i4, i3, i2, i1})
	checkGoldenBytes(t, "issue-list.json", marshalJSON(list))

	// --- POST /issues bodies: status + raw body bytes ---
	postBodies := []struct{ name, body string }{
		{"valid", `{"title":"T","body":"B"}`},
		{"title-only", `{"title":"T"}`},
		{"with-label", `{"title":"T","body":"B","labels":["bug"]}`},
		{"missing-title", `{"body":"x"}`},
		{"empty-title", `{"title":"","body":"x"}`},
		{"empty-body", `{}`},
		{"malformed", `{bad json`},
		{"trailing", `{"title":"T"} xyz`},
		{"title-number", `{"title":123,"body":"B"}`},
		{"body-number", `{"title":"T","body":5}`},
		{"labels-string", `{"title":"T","labels":"bug"}`},
		{"labels-mixed", `{"title":"T","labels":["bug",1,null]}`},
		{"labels-null", `{"title":"T","labels":null}`},
		{"null-doc", `null`},
		{"string-doc", `"just a string"`},
		{"array-doc", `[]`},
		{"dup-title", `{"title":"a","title":"b"}`},
		{"unknown-label", `{"title":"T","labels":["nope"]}`},
		{"html-chars", `{"title":"a<b>&\"c\"","body":"<>&"}`},
		{"unicode", `{"title":"ünïcode — test","body":" "}`},
		{"extra-fields", `{"title":"T","body":"B","zzz":123,"labels":[]}`},
	}
	for _, tc := range postBodies {
		st := store.NewStore()
		rp := st.CreateRepo("golden-post", false, "")
		_ = rp
		resp := HandleRest(RestContext{
			Store:  st,
			Method: "POST",
			URL:    mustURLForCall("http://127.0.0.1:41365/api/v3/repos/adaiversen/golden-post/issues"),
			Path:   []string{"repos", "adaiversen", "golden-post", "issues"},
			Body:   []byte(tc.body),
		})
		checkGoldenBytes(t, "post-"+tc.name+".bin",
			[]byte(fmt.Sprintf("status=%d\n%s", resp.Status, resp.Body)))
	}
}
