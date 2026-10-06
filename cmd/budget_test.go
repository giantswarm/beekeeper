//go:build unix

package cmd

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// graphqlRefusedText is GitHub's refusal of a GraphQL call while its
// counter still shows headroom.
const graphqlRefusedText = "API rate limit already exceeded for user ID 1."

// refusingGitHub stands in for gh's login and for GitHub: REST answers with
// headroom, GraphQL refuses while refuse is set. It counts the GraphQL
// queries.
func refusingGitHub(t *testing.T, refuse *bool) (queries *int) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\necho tok\n"), 0o700); err != nil { //nolint:gosec // a fake gh
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	queries = new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-RateLimit-Limit", "5000")
		h.Set("X-RateLimit-Remaining", "4800")
		h.Set("X-RateLimit-Reset", "1791258912")
		if r.URL.Path != "/graphql" {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		*queries++
		if *refuse {
			_, _ = w.Write([]byte(`{"data":null,"errors":[{"type":"RATE_LIMITED","message":"` + graphqlRefusedText + `"}]}`))
			return
		}
		resetAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		_, _ = w.Write([]byte(`{"data":{"rateLimit":{"limit":5000,"remaining":4960,"used":40,"resetAt":"` + resetAt + `"}}}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("BEEKEEPER_GITHUB_API", srv.URL)
	return queries
}

func budgetApp(t *testing.T) (*app, *bytes.Buffer) {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cfg := &config.Config{GitHub: config.GitHub{Floor: 2500, ProbeRepo: scratchRepo},
		Merge: config.Merge{BudgetFresh: config.Duration{Duration: time.Minute}}}
	return &app{cfg: cfg, store: store, now: time.Now(), out: &out}, &out
}

// With REST available and GraphQL refused, budget says so and --gate
// refuses; the refusal is kept in the state for the next reader.
func TestBudgetGateRefusesWhileGraphQLIsRefused(t *testing.T) {
	refuse := true
	refusingGitHub(t, &refuse)
	a, out := budgetApp(t)
	c := a.budgetCmd()
	c.SetArgs([]string{"--gate"})
	c.SetOut(out)
	c.SetErr(out)
	err := c.Execute()
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != ExitRefused {
		t.Fatalf("budget --gate = %v, want exit %d", err, ExitRefused)
	}
	for _, want := range []string{"4800 of 5000 left", "GRAPHQL REFUSED (secondary limit, 4800 of 5000 left) until an answer says otherwise: " + graphqlRefusedText} {
		if !strings.Contains(out.String()+ee.msg, want) {
			t.Errorf("budget printed %q and refused %q, want %q", out.String(), ee.msg, want)
		}
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if g := st.Budget.GraphQL; g == nil || g.Refused != graphqlRefusedText || !g.Secondary {
		t.Errorf("stored GraphQL reading = %+v, want the refusal", g)
	}
}

// A fresh reading is reused: a run of gated gh calls costs one GraphQL
// point per merge.budgetFresh, not one each.
func TestBudgetReusesAFreshGraphQLReading(t *testing.T) {
	refuse := false
	queries := refusingGitHub(t, &refuse)
	a, _ := budgetApp(t)
	for range 3 {
		b, err := a.probeBudget(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if b.GraphQL == nil || b.GraphQL.Remaining != 4960 || b.GraphQL.Refused != "" {
			t.Fatalf("GraphQL = %+v, want 4960 left", b.GraphQL)
		}
	}
	if *queries != 1 {
		t.Errorf("%d GraphQL queries for three probes in a minute, want 1", *queries)
	}
}

// A reused reading reports what it read: the points used are what the
// limit lost, for GraphQL and core alike.
func TestBudgetCachedReadingReportsUsed(t *testing.T) {
	refuse := false
	refusingGitHub(t, &refuse)
	a, _ := budgetApp(t)
	for i := range 2 {
		b, err := a.probeBudget(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if b.Used != b.Limit-b.Remaining {
			t.Errorf("probe %d: core used %d, want %d", i, b.Used, b.Limit-b.Remaining)
		}
		if g := b.GraphQL; g == nil || g.Used != g.Limit-g.Remaining || g.Used != 40 {
			t.Errorf("probe %d: GraphQL = %+v, want 40 used", i, g)
		}
	}
}

// A fresh reading whose reset has passed counted a window that is over: the
// next probe reads the limit again and never reports the past reset.
func TestBudgetRereadsAReadingPastItsReset(t *testing.T) {
	refuse := false
	queries := refusingGitHub(t, &refuse)
	a, _ := budgetApp(t)
	now := time.Now().UTC()
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Budget = &state.Budget{Limit: 5000, Remaining: 4800, At: now,
			GraphQL: &state.GraphQL{Limit: 5000, Remaining: 4062, Reset: now.Add(-time.Minute), At: now}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	b, err := a.probeBudget(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if *queries != 1 {
		t.Errorf("%d GraphQL queries, want 1: the reading past its reset was reused", *queries)
	}
	if g := b.GraphQL; g == nil || !g.Reset.After(now) || g.Remaining != 4960 {
		t.Errorf("GraphQL = %+v, want the new reading with a reset after %s", g, now)
	}
}

// The watch says a GraphQL refusal once, with the callers, and its end once.
func TestWatchSaysAGraphQLRefusalOnce(t *testing.T) {
	refuse := true
	refusingGitHub(t, &refuse)
	a, out := budgetApp(t)
	a.cfg.Merge.BudgetFresh.Duration = 0
	a.cfg.Watch.Repeat.Duration = time.Hour
	w := &watcher{app: a, last: map[string]time.Time{}}
	for range 2 {
		w.budget(context.Background(), time.Now())
	}
	if n := strings.Count(out.String(), "GRAPHQL REFUSED"); n != 1 {
		t.Errorf("the watch said the refusal %d times, want once:\n%s", n, out)
	}
	if !strings.Contains(out.String(), "; callers: ") {
		t.Errorf("the refusal names no callers:\n%s", out)
	}
	refuse = false
	w.budget(context.Background(), time.Now())
	if !strings.Contains(out.String(), "ENDED GRAPHQL REFUSED") {
		t.Errorf("the refusal's end was not said:\n%s", out)
	}
}
