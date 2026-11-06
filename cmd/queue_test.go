package cmd

import (
	"context"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/giantswarm/beekeeper/internal/github"
)

const queueIssue = "o/r#7"

func queueCall(a *app, args ...string) error {
	c := a.queueCmd()
	c.SetArgs(args)
	c.SetOut(a.out)
	c.SetErr(a.out)
	c.SilenceUsage = true
	c.SilenceErrors = true
	return c.Execute()
}

// fakeIssueBody answers gh's reads and edits of one issue body.
func fakeIssueBody(t *testing.T, body string) *string {
	t.Helper()
	old := github.RunGH
	t.Cleanup(func() { github.RunGH = old })
	var mu sync.Mutex
	github.RunGH = func(_ context.Context, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		if args[1] == "view" {
			return []byte(body + "\n"), nil
		}
		raw, err := os.ReadFile(args[slices.Index(args, "--body-file")+1]) //nolint:gosec // the test's file
		body = string(raw)
		return nil, err
	}
	return &body
}

// Two adds at once both land, with distinct numbers, and the call leaves no
// body file behind.
func TestConcurrentQueueAddsBothLand(t *testing.T) {
	a, _ := noteApp(t)
	body := fakeIssueBody(t, "1. first\n")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, row := range []string{"one", "two"} {
		wg.Go(func() {
			r, _ := parseRef(queueIssue)
			_, errs[i] = a.addQueueRow(context.Background(), r, row)
		})
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatal(errs)
	}
	for _, want := range []string{"1. first", "2. ", "3. ", "one", "two"} {
		if !strings.Contains(*body, want) {
			t.Fatalf("body %q lacks %q", *body, want)
		}
	}
}

func TestQueueAddStaysInTheDesksOrganisations(t *testing.T) {
	a, _ := noteApp(t)
	a.cfg.GitHub.AllowedOrgs = []string{"mine"}
	if err := queueCall(a, "add", queueIssue, "row"); Code(err) != ExitRefused {
		t.Fatalf("outside the organisations: %v", err)
	}
	if err := queueCall(a, "add", "no-issue", "row"); Code(err) != ExitUsage {
		t.Fatalf("no issue: %v", err)
	}
}
