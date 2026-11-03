package claude

import (
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// sdkFixture writes a project folder: the worker's own transcript, two
// sdk-py reviews (one on a model without a price), one before the window
// and a transcript of another entrypoint.
func sdkFixture(t *testing.T, since time.Time) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string][]string{
		"worker.jsonl": {
			`{"type":"user","entrypoint":"cli","timestamp":"2026-10-08T09:00:00Z","message":{"content":"task"}}`,
			`{"type":"assistant","timestamp":"2026-10-08T09:00:01Z","message":{"id":"w1","model":"m","usage":{"input_tokens":1000000}}}`,
		},
		"r1.jsonl": {
			`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-10-08T10:00:00Z"}`,
			`{"type":"user","entrypoint":"sdk-py","timestamp":"2026-10-08T10:00:00Z","message":{"content":"review"}}`,
			`{"type":"assistant","timestamp":"2026-10-08T10:00:02Z","message":{"id":"r1a","model":"m","usage":{"input_tokens":2000000}}}`,
			`{"type":"assistant","timestamp":"2026-10-08T10:00:05Z","message":{"id":"r1b","model":"m","usage":{"output_tokens":100000}}}`,
		},
		"r2.jsonl": {
			`{"type":"user","entrypoint":"sdk-py","timestamp":"2026-10-08T09:30:00Z","message":{"content":"review"}}`,
			`{"type":"assistant","timestamp":"2026-10-08T09:30:02Z","message":{"id":"r2a","model":"x-unpriced","usage":{"input_tokens":5}}}`,
		},
		"old.jsonl": {
			`{"type":"user","entrypoint":"sdk-py","timestamp":"2026-10-08T07:00:00Z","message":{"content":"review"}}`,
			`{"type":"assistant","timestamp":"2026-10-08T07:00:02Z","message":{"id":"o1","model":"m","usage":{"input_tokens":5}}}`,
		},
		"notes.txt": {`{"type":"user","entrypoint":"sdk-py"}`},
	}
	for name, lines := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := since.Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "old.jsonl"), old, old); err != nil {
		t.Fatal(err)
	}
	return dir
}

const unpricedModel = "x-unpriced"

func TestSDKChildrenReadsTheFoldersReviews(t *testing.T) {
	since := time.Date(2026, 10, 8, 8, 0, 0, 0, time.UTC)
	got := SDKChildren(sdkFixture(t, since), since, spendPrices)
	if len(got) != 2 {
		t.Fatalf("children %+v, want r2 and r1", got)
	}
	r2, r1 := got[0], got[1]
	if r2.Session != "r2" || !r2.Unpriced || !slices.Equal(r2.Models, []string{unpricedModel}) || r2.Requests != 1 {
		t.Errorf("first %+v, want the unpriced r2", r2)
	}
	// 2M in $2, 100k out $1.
	if r1.Session != "r1" || r1.Unpriced || r1.Requests != 2 || math.Abs(r1.USD-3) > 1e-9 || !slices.Equal(r1.Models, []string{"m"}) {
		t.Errorf("second %+v, want r1 at $3 on m", r1)
	}
	if want := time.Date(2026, 10, 8, 10, 0, 2, 0, time.UTC); !r1.Started.Equal(want) {
		t.Errorf("r1 started %s, want its first request %s", r1.Started, want)
	}
}

func TestSDKChildrenWithoutTheFolder(t *testing.T) {
	if got := SDKChildren(filepath.Join(t.TempDir(), "none"), time.Time{}, spendPrices); got != nil {
		t.Errorf("children %+v of no folder", got)
	}
}
