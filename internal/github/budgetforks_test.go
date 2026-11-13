//go:build unix

package github

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const forkFixtureDevctl = "devctl"

// budgetForkFixture puts a gh and, with devctl, a devctl on an otherwise
// empty PATH, each logging its start to the returned file. Only invocation
// metadata is logged; credentials never enter it.
func budgetForkFixture(t *testing.T, devctl bool) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "calls")
	t.Setenv("BUDGET_FORK_LOG", log)
	t.Setenv("PATH", bin)
	scripts := map[string]string{
		"gh": "#!/bin/sh\nprintf 'gh\\n' >> \"$BUDGET_FORK_LOG\"\nprintf 'fixture\\n'\n",
	}
	if devctl {
		scripts[forkFixtureDevctl] = "#!/bin/sh\nprintf 'devctl\\n' >> \"$BUDGET_FORK_LOG\"\nif [ \"$2\" = status ]; then printf '%s\\n' '{\"github\":{\"present\":true}}'; else printf 'fixture\\n'; fi\n"
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0700); err != nil { //nolint:gosec // executable fixture
			t.Fatal(err)
		}
	}
	return log
}

func budgetForkStarts(t *testing.T, log string) int {
	t.Helper()
	raw, err := os.ReadFile(log) //nolint:gosec // the test's own temporary file
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(strings.Fields(string(raw)))
}

// A minute of concurrent loop/watch samples acquires each credential once.
func TestBudgetCredentialsForkBudget(t *testing.T) {
	log := budgetForkFixture(t, true)
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
	if got := budgetForkStarts(t, log); got != 3 {
		t.Fatalf("idle samples forked %d credential helpers, want 3", got)
	}
}

// A caller whose deadline ended says nothing about the providers: its failed
// read must not be served to the next caller for a minute, which would fail
// that caller's budget closed.
func TestBudgetCredentialsKeepNoCancelledRead(t *testing.T) {
	log := budgetForkFixture(t, true)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	BudgetCredentials(cancelled)
	credentials := BudgetCredentials(t.Context())
	if len(credentials) != 2 {
		t.Fatalf("got %d identities, want two", len(credentials))
	}
	for _, c := range credentials {
		if c.Err != nil || c.Skip || c.Value != "fixture" {
			t.Fatalf("identity %s served the cancelled read: err=%v skip=%t", c.Identity, c.Err, c.Skip)
		}
	}
	if got := budgetForkStarts(t, log); got != 3 {
		t.Fatalf("forked %d credential helpers after the cancelled read, want 3", got)
	}
}

// Without devctl there is no App identity: the caller then names a previously
// configured App unavailable instead of silently keeping its old window.
func TestBudgetCredentialsLeaveOutAnAbsentApp(t *testing.T) {
	log := budgetForkFixture(t, false)
	credentials := BudgetCredentials(t.Context())
	if len(credentials) != 1 || credentials[0].Identity != IdentityGH || credentials[0].Err != nil {
		t.Fatalf("credentials without devctl = %+v, want the gh identity alone", credentials)
	}
	if got := budgetForkStarts(t, log); got != 1 {
		t.Fatalf("forked %d credential helpers, want 1", got)
	}
}
