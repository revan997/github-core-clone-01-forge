package store

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The hand-rolled SeqTimestamp must be byte-identical to the time.Format
// version it replaced, for every sequence value the store can reach.
func TestSeqTimestampMatchesTimeFormat(t *testing.T) {
	// Exhaustive for the first million seconds (~11.5 days of sequence),
	// then sampled far out (leap years, month/year boundaries).
	for seq := 0; seq <= 1_000_000; seq++ {
		got := seqTimestampString(seq)
		want := time.UnixMilli(TimeBaseMs + int64(seq)*1000).UTC().Format("2006-01-02T15:04:05.000Z")
		if got != want {
			t.Fatalf("seq %d: got %q, want %q", seq, got, want)
		}
	}
	for seq := 1_000_000; seq <= 200_000_000; seq += 997 {
		got := seqTimestampString(seq)
		want := time.UnixMilli(TimeBaseMs + int64(seq)*1000).UTC().Format("2006-01-02T15:04:05.000Z")
		if got != want {
			t.Fatalf("seq %d: got %q, want %q", seq, got, want)
		}
	}
	// The store method itself advances by exactly one second per call.
	s := NewStore()
	prev := ""
	for i := 0; i < 1000; i++ {
		got := s.SeqTimestamp()
		want := time.UnixMilli(TimeBaseMs + int64(i+1)*1000).UTC().Format("2006-01-02T15:04:05.000Z")
		if got != want {
			t.Fatalf("call %d: got %q, want %q", i, got, want)
		}
		if got == prev {
			t.Fatalf("call %d: timestamp did not advance", i)
		}
		prev = got
	}
}

func TestGlobalIDMatchesFmtVersion(t *testing.T) {
	ref := func(kind string, parts ...any) string {
		strs := make([]string, len(parts))
		for i, p := range parts {
			strs[i] = fmt.Sprint(p)
		}
		return base64.RawURLEncoding.EncodeToString([]byte(kind + ":" + strings.Join(strs, ":")))
	}
	cases := []struct {
		kind  string
		parts []any
	}{
		{"Issue", []any{1, 1}},
		{"Issue", []any{1, 123456}},
		{"IssueComment", []any{2, 3, 999999}},
		{"Label", []any{"bug"}},
		{"Label", []any{"good first issue"}},
		{"User", []any{1000}},
		{"Repository", []any{7}},
		{"Blob", []any{1, "abcdef0123456789abcdef0123456789abcdef01"}},
		{"Ref", []any{1, "refs/heads/feature-x"}},
		{"PullRequest", []any{int64(3), 4}},
	}
	for _, c := range cases {
		if got, want := GlobalID(c.kind, c.parts...), ref(c.kind, c.parts...); got != want {
			t.Errorf("GlobalID(%q, %v) = %q, want %q", c.kind, c.parts, got, want)
		}
	}
}
