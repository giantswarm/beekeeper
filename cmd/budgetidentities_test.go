//go:build unix

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
