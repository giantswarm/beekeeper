//go:build unix

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestBudgetNamesIdentitiesAndNeverReusesAnotherWindow(t *testing.T) {
	a, out := budgetApp(t)
	appRemaining := 4910
	unreadable := false
	reverse := false
	budgetCredentials = func(context.Context) []github.BudgetCredential {
		gh := github.BudgetCredential{Identity: "gh", Value: "gh-fixture"}
		app := github.BudgetCredential{Identity: github.IdentityApp, Value: "app-fixture"}
		if unreadable {
			gh.Err = errors.New("stored login unavailable")
		}
		if reverse {
			return []github.BudgetCredential{app, gh}
		}
		return []github.BudgetCredential{gh, app}
	}
	queries := map[string]int{}
	resetGH, resetApp := time.Now().Add(20*time.Minute).UTC(), time.Now().Add(54*time.Minute).UTC()
	// A legacy unlabelled high reading must not be assigned to either route.
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.BudgetETag = "legacy-etag"
		st.Budget = &state.Budget{Remaining: 4900, At: time.Now(), Reset: resetApp,
			GraphQL: &state.GraphQL{Remaining: 4910, Limit: 5000, Reset: resetApp, At: time.Now()}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, remaining, rest, reset := "gh", 1941, 4800, resetGH
		if r.Header.Get("Authorization") == "Bearer app-fixture" {
			id, remaining, rest, reset = github.IdentityApp, appRemaining, 3000, resetApp
		}
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", fmt.Sprint(rest))
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(reset.Unix()))
		w.Header().Set("ETag", "etag-"+id)
		if tag := r.Header.Get("If-None-Match"); tag != "" && tag != "etag-"+id {
			t.Errorf("identity %s reused another identity's ETag", id)
		}
		switch r.URL.Path {
		case budgetGraphQLPath:
			queries[id]++
			_, _ = fmt.Fprintf(w, `{"data":{"viewer":{"login":"test-%s"},"rateLimit":{"limit":5000,"remaining":%d,"used":%d,"resetAt":%q}}}`, id, remaining, 5000-remaining, reset.Format(time.RFC3339))
		case "/rate_limit":
			_, _ = w.Write([]byte(`{"resources":{"search":{"limit":30,"remaining":30},"code_search":{"limit":10,"remaining":10}}}`))
		default:
			repo := a.cfg.GitHub.ProbeRepo
			if id == github.IdentityApp {
				repo = github.BudgetAppRepo
			}
			if r.URL.Path != "/repos/"+repo {
				t.Errorf("%s REST probe used wrong repository: %s", id, r.URL.Path)
			}
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("BEEKEEPER_GITHUB_API", srv.URL)
	for range 2 {
		b, err := a.probeBudget(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if b.Identity != github.IdentityApp || b.Remaining != 3000 || b.GraphQL.Identity != "gh" || b.GraphQL.Remaining != 1941 || b.GraphQL.Login != "test-gh" {
			t.Fatalf("wrong minima: REST %s/%d, GraphQL %+v", b.Identity, b.Remaining, b.GraphQL)
		}
		if b.Identities[github.IdentityApp].GraphQL.Remaining != 4910 || b.Identities["gh"].GraphQL.Remaining != 1941 {
			t.Fatal("identity caches mixed")
		}
		reverse = true
		t.Chdir(t.TempDir())
	}
	if queries["gh"] != 1 || queries[github.IdentityApp] != 1 {
		t.Fatalf("cache not per identity: %v", queries)
	}
	g := &gateRun{app: a, ctx: context.Background()}
	minimum, err := g.gateBudget()
	if err != nil || minimum.Identity != "gh" || minimum.Remaining != 1941 || !minimum.Reset.Equal(resetGH.Truncate(time.Second)) {
		t.Fatalf("merge gate ignored low GraphQL: %+v, %v", minimum, err)
	}
	a.json = true
	c := a.budgetCmd()
	c.SetArgs([]string{gateFlag})
	err = c.Execute()
	var refused *exitError
	if !errors.As(err, &refused) || refused.code != ExitRefused {
		t.Fatalf("high App balance hid low gh: %v", err)
	}
	var rendered github.Budget
	if err := json.Unmarshal(out.Bytes(), &rendered); err != nil {
		t.Fatal(err)
	}
	if len(rendered.Identities) != 2 || rendered.GraphQL.Identity != "gh" {
		t.Fatal("JSON omitted identities")
	}
	// The minimum may move, but each route keeps its own counter and reset.
	a.cfg.Merge.BudgetFresh.Duration = -time.Second
	appRemaining = 1300
	b, err := a.probeBudget(context.Background())
	if err != nil || b.GraphQL.Identity != github.IdentityApp || b.GraphQL.Remaining != 1300 {
		t.Fatalf("minimum did not move: %+v, %v", b.GraphQL, err)
	}
	// Reproduce the live jump: same source reports a high different window
	// while its depleted window is still unexpired. It must remain gated.
	oldReset := b.GraphQL.Reset
	appRemaining, resetApp = 4900, resetApp.Add(5*time.Minute)
	b, err = a.probeBudget(context.Background())
	if err != nil || b.GraphQL.Remaining != 1300 || !b.GraphQL.Reset.Equal(oldReset) || len(b.Identities[github.IdentityApp].Windows) != 2 {
		t.Fatalf("high new window erased depleted window: %+v, %v", b.GraphQL, err)
	}
	unreadable = true
	b, err = a.probeBudget(context.Background())
	if err == nil || b.GraphQL.Identity != "gh" || b.GraphQL.Err == "" {
		t.Fatalf("unreadable identity did not fail closed: %+v, %v", b.GraphQL, err)
	}
	budgetCredentials = func(context.Context) []github.BudgetCredential {
		return []github.BudgetCredential{{Identity: github.IdentityApp, Value: "app-fixture"}}
	}
	b, err = a.probeBudget(context.Background())
	if err == nil || b.GraphQL.Identity != "gh" || b.GraphQL.Err == "" {
		t.Fatal("a missing provider erased the low identity")
	}
}

// Separate app instances stand in for the loop and embedded watch. A delayed
// response must hold the producer lock; later admission sees the lower reading
// regardless of which response was delayed.
func TestBudgetSerializesConcurrentDelayedResponses(t *testing.T) {
	for _, firstLow := range []bool{false, true} {
		t.Run(fmt.Sprint(firstLow), func(t *testing.T) {
			a, _ := budgetApp(t)
			a.cfg.Merge.BudgetFresh.Duration = -time.Second
			other := *a
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			var queries atomic.Int32
			reset := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-RateLimit-Limit", "5000")
				w.Header().Set("X-RateLimit-Remaining", "4800")
				w.Header().Set("X-RateLimit-Reset", fmt.Sprint(reset.Unix()))
				if r.URL.Path != budgetGraphQLPath {
					_, _ = w.Write([]byte(`{}`))
					return
				}
				n := queries.Add(1)
				remaining := 4900
				if (n == 1 && firstLow) || (n == 2 && !firstLow) {
					remaining = 2000
				}
				if n == 1 {
					close(entered)
					<-release
				}
				_, _ = fmt.Fprintf(w, `{"data":{"viewer":{"login":"fixture"},"rateLimit":{"limit":5000,"remaining":%d,"resetAt":%q}}}`, remaining, reset.Format(time.RFC3339))
			}))
			t.Cleanup(srv.Close)
			// Unblock handlers before closing the server, even on a failed assertion.
			t.Cleanup(unblock)
			t.Setenv("BEEKEEPER_GITHUB_API", srv.URL)
			type result struct {
				budget github.Budget
				err    error
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			first, second := make(chan result, 1), make(chan result, 1)
			go func() { b, err := a.probeBudget(ctx); first <- result{b, err} }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			go func() { b, err := other.probeBudget(ctx); second <- result{b, err} }()
			select {
			case r := <-second:
				t.Fatalf("concurrent producer bypassed delayed probe: %+v", r)
			case <-time.After(50 * time.Millisecond):
			}
			if queries.Load() != 1 {
				t.Fatal("second GraphQL probe overlapped delayed response")
			}
			unblock()
			if r := <-first; r.err != nil {
				t.Fatal(r.err)
			}
			if r := <-second; r.err != nil || r.budget.GraphQL.Remaining != 2000 {
				t.Fatalf("admission lost the low reading: %+v, %v", r.budget.GraphQL, r.err)
			}
			// Another high sample cannot reopen admission in the same window.
			b, err := a.probeBudget(ctx)
			st, readErr := a.store.Read()
			if err != nil || readErr != nil || b.GraphQL.Remaining != 2000 || st.Budget.GraphQL.Remaining != 2000 || st.Budget.Identities["gh"].GraphQL.Remaining != 2000 {
				t.Fatalf("returned/persisted minima diverged: %+v, %+v, %v, %v", b.GraphQL, st.Budget, err, readErr)
			}
		})
	}
}

// The provider's global verdict includes unrelated services. Only its GitHub
// metadata decides whether the App can participate in the budget.
func TestBudgetGateUsesGitHubMetadataDespiteProviderExit(t *testing.T) {
	for _, tc := range []struct {
		name, metadata   string
		wantApp, warning bool
	}{
		{"unrelated auth required", `{"github":{"present":true},"circleci":{"present":false}}`, true, false},
		{"github absent", `{"github":{"present":false}}`, false, false},
		{"metadata unreadable", `not-json`, false, true},
		{"github block absent", `{}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refuse := false
			refusingGitHub(t, &refuse)
			a, out := budgetApp(t)
			budgetCredentials = github.BudgetCredentials
			bin := t.TempDir()
			script := "#!/bin/sh\nif [ \"$1 $2\" = 'auth status' ]; then\nprintf '%s\\n' '" + tc.metadata + "'\nexit 8\nfi\nprintf 'tok\\n'\n"
			if err := os.WriteFile(filepath.Join(bin, "devctl"), []byte(script), 0o700); err != nil { //nolint:gosec // executable fixture
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			// A prior failed App probe must not keep the corrected fallback blocked.
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Budget = &state.Budget{Identities: map[string]state.Budget{github.IdentityApp: {Identity: github.IdentityApp, GraphQL: &state.GraphQL{Err: "old metadata error"}}}}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			c := a.budgetCmd()
			c.SetArgs([]string{gateFlag})
			c.SetOut(out)
			c.SetErr(out)
			if err := c.Execute(); err != nil {
				t.Fatal(err)
			}
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			_, app := st.Budget.Identities[github.IdentityApp]
			if app != tc.wantApp {
				t.Fatalf("App present = %v, want %v", app, tc.wantApp)
			}
			if strings.Contains(out.String(), "WARNING:") != tc.warning {
				t.Fatalf("warning missing or unexpected: %s", out.String())
			}
		})
	}
}

func TestBudgetMetadataFallbackRetainsKnownLowWindow(t *testing.T) {
	refuse := false
	refusingGitHub(t, &refuse)
	a, out := budgetApp(t)
	budgetCredentials = func(context.Context) []github.BudgetCredential {
		return []github.BudgetCredential{{Identity: github.IdentityGH, Value: "tok"},
			{Identity: github.IdentityApp, Skip: true, Warning: "App metadata unavailable"}}
	}
	reset := time.Now().Add(time.Hour)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Budget = &state.Budget{Identities: map[string]state.Budget{github.IdentityApp: {
			Identity: github.IdentityApp, Remaining: 4000, Limit: 5000, Reset: reset,
			GraphQL: &state.GraphQL{Identity: github.IdentityApp, Remaining: 100, Limit: 5000, Reset: reset, At: time.Now()},
		}}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	c := a.budgetCmd()
	c.SetArgs([]string{gateFlag})
	c.SetOut(out)
	c.SetErr(out)
	err := c.Execute()
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 3 {
		t.Fatalf("known low App budget did not hold gate: %v", err)
	}
	if !strings.Contains(out.String(), "WARNING:") {
		t.Fatal("missing fallback warning")
	}
}

// A busy state writer used to stall budget for the state's two-minute lock
// wait even when its caller supplied a short deadline.
func TestBudgetDoesNotWaitForStateWriter(t *testing.T) {
	a, _ := budgetApp(t)
	refuse := false
	refusingGitHub(t, &refuse)
	lock := flock.New(filepath.Join(a.store.Dir(), "state.lock"))
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Unlock() }()
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := a.probeBudget(ctx)
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("budget did not fail promptly on a busy state writer: %v (%s)", err, time.Since(start))
	}
}

// The loop forces GraphQL samples before and after each step. That must
// not multiply REST probes, nor advance the REST cache's sampling time.
func TestForcedGraphQLSamplesShareRESTCache(t *testing.T) {
	a, _ := budgetApp(t)
	a.cfg.Merge.BudgetFresh.Duration = -time.Second
	var rest, graph atomic.Int32
	reset := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4800")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(reset.Unix()))
		if r.URL.Path == budgetGraphQLPath {
			graph.Add(1)
			_, _ = fmt.Fprintf(w, `{"data":{"rateLimit":{"limit":5000,"remaining":4800,"resetAt":%q}}}`, reset.Format(time.RFC3339))
		} else {
			rest.Add(1)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("BEEKEEPER_GITHUB_API", srv.URL)
	var sampled time.Time
	for i := range 6 {
		if _, err := a.probeBudget(t.Context()); err != nil {
			t.Fatal(err)
		}
		st, err := a.store.Read()
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			sampled = st.Budget.Identities["gh"].At
		} else if !st.Budget.Identities["gh"].At.Equal(sampled) {
			t.Fatal("GraphQL sample extended the REST cache lifetime")
		}
	}
	if rest.Load() != 1 || graph.Load() != 6 {
		t.Fatalf("REST %d GraphQL %d, want 1 and 6", rest.Load(), graph.Load())
	}
	// Expiry forces a real REST read despite continuing GraphQL traffic.
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		b := st.Budget.Identities["gh"]
		b.At = time.Now().Add(-time.Minute)
		st.Budget.Identities["gh"] = b
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.probeBudget(t.Context()); err != nil {
		t.Fatal(err)
	}
	if rest.Load() != 2 {
		t.Fatal("expired REST sample was reused")
	}
}

// Pre-install canaries must survive ordinary state contention, while the
// short-deadline regression above must still fail promptly.
func TestBudgetPublishesAfterTransientStateContention(t *testing.T) {
	a, _ := budgetApp(t)
	refuse := false
	refusingGitHub(t, &refuse)
	lock := flock.New(filepath.Join(a.store.Dir(), "state.lock"))
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	released := make(chan error, 1)
	time.AfterFunc(350*time.Millisecond, func() { released <- lock.Unlock() })
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	b, err := a.probeBudget(ctx)
	unlockErr := <-released
	if err != nil || unlockErr != nil {
		t.Fatalf("transient contention rejected healthy budget: %v; unlock: %v", err, unlockErr)
	}
	st, err := a.store.Read()
	if err != nil || st.Budget == nil || st.Budget.Remaining != b.Remaining {
		t.Fatalf("budget not published: %+v, %v", st, err)
	}
}
