package diskprune

import (
	"os"
	"testing"
	"time"
)

func TestCacheBudgetAccountsForEntriesRemovedDuringScan(t *testing.T) {
	for _, files := range []bool{false, true} {
		mode := "entries"
		if files {
			mode = "files"
		}
		t.Run(mode, func(t *testing.T) {
			for _, tc := range []struct {
				name      string
				maxBytes  int64
				olderThan time.Duration
				removed   int
			}{
				{name: "already fits", maxBytes: 2000},
				{name: "still over budget", maxBytes: 1000, removed: 1},
				{name: "age rule still applies", olderThan: 7 * day, removed: 2},
			} {
				t.Run(tc.name, func(t *testing.T) {
					root := t.TempDir()
					oldest := cacheFile(t, root, "a", 1000, 10*day)
					next := cacheFile(t, root, "b", 1000, 9*day)
					newest := cacheFile(t, root, "c", 1000, 8*day)
					p := Policy{Root: root, Files: files, MaxBytes: tc.maxBytes, OlderThan: tc.olderThan, Recheck: func() ([]string, error) {
						return nil, os.Remove(oldest)
					}}
					r := Cache(p, time.Now(), false, nil, nil, false)
					if r.Held != 3000 || r.Freed != int64(tc.removed)*1000 || r.Removed != tc.removed || len(r.Errors) != 0 {
						t.Fatalf("wrong accounting after concurrent removal: %+v", r)
					}
					if exists(next) != (tc.removed == 0) || exists(newest) != (tc.removed < 2) {
						t.Fatalf("wrong remaining cache: b=%v c=%v", exists(next), exists(newest))
					}
				})
			}
		})
	}
}
