package omp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
)

func TestSpendTranscripts(t *testing.T) {
	dir := t.TempDir()
	from := time.Date(2026, 10, 8, 8, 0, 0, 0, time.UTC)
	to := from.Add(4 * time.Hour)
	files := map[string][]string{
		"-work/2026-10-08T07-00-00-000Z_s1.jsonl": {
			`{"type":"session","id":"s1","timestamp":"2026-10-08T07:00:00Z","cwd":"/work"}`,
			// Before the window.
			`{"type":"message","id":"e0","timestamp":"2026-10-08T07:30:00Z","message":{"role":"assistant","provider":"p","model":"m","responseId":"r0","usage":{"input":9}}}`,
			`{"type":"message","id":"e1","timestamp":"2026-10-08T09:00:00Z","message":{"role":"user","content":"\"usage\""}}`,
			`{"type":"message","id":"e2","timestamp":"2026-10-08T09:00:01Z","message":{"role":"assistant","provider":"p","model":"m","responseId":"r1","usage":{"input":100,"output":10,"cacheRead":1000,"cacheWrite":50,"cost":{"total":1}}}}`,
			// No usage: no request.
			`{"type":"message","id":"e3","timestamp":"2026-10-08T09:00:02Z","message":{"role":"assistant","provider":"p","model":"m","responseId":"r2"}}`,
		},
		// A subagent's file, in its session's folder.
		"-work/2026-10-08T07-00-00-000Z_s1/Sub.jsonl": {
			`{"type":"message","id":"f1","timestamp":"2026-10-08T10:00:00Z","message":{"role":"assistant","provider":"p","model":"m","responseId":"r3","usage":{"input":7}}}`,
		},
	}
	for name, lines := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := map[bool][]claude.SpendRequest{}
	for _, tr := range SpendTranscripts(dir, from, to) {
		got[tr.Subagent] = append(got[tr.Subagent], tr.Requests...)
	}
	session, sub := got[false], got[true]
	if len(session) != 1 || len(sub) != 1 {
		t.Fatalf("session requests %+v, subagent requests %+v; want one each", session, sub)
	}
	want := claude.SpendRequest{
		ID:     "r1@2026-10-08T09:00:01Z",
		At:     time.Date(2026, 10, 8, 9, 0, 1, 0, time.UTC),
		Model:  "p/m",
		Tokens: claude.Tokens{Input: 100, Output: 10, CacheRead: 1000, CacheWrite5m: 50},
	}
	if r := session[0]; r.ID != want.ID || !r.At.Equal(want.At) || r.Model != want.Model || r.Tokens != want.Tokens {
		t.Errorf("request %+v, want %+v", r, want)
	}
	if len(SpendTranscripts(filepath.Join(dir, "none"), from, to)) != 0 {
		t.Error("requests without the sessions directory")
	}
}
