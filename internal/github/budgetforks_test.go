//go:build unix

package github

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// A minute of concurrent loop/watch samples acquires each credential once.
// Only invocation metadata is counted; credentials never enter the log.
func TestBudgetCredentialsForkBudget(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(bin, "calls")
	t.Setenv("BUDGET_FORK_LOG", log)
	t.Setenv("PATH", bin)
	for name, script := range map[string]string{
		"gh":     "#!/bin/sh\nprintf 'gh\\n' >> \"$BUDGET_FORK_LOG\"\nprintf 'fixture\\n'\n",
		"devctl": "#!/bin/sh\nprintf 'devctl\\n' >> \"$BUDGET_FORK_LOG\"\nif [ \"$2\" = status ]; then printf '%s\\n' '{\"github\":{\"present\":true}}'; else printf 'fixture\\n'; fi\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0700); err != nil { //nolint:gosec // executable fixture
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for range 60 {
		wg.Go(func() {
			for _, c := range BudgetCredentials(t.Context()) {
				if c.Err != nil || c.Skip {
					t.Errorf("identity %s unavailable", c.Identity)
				}
			}
		})
	}
	wg.Wait()
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(strings.Fields(string(raw))); got != 3 {
		t.Fatalf("idle samples forked %d credential helpers, want 3", got)
	}
}
