package github_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/forge"
	"github.com/giantswarm/beekeeper/internal/forge/github"
	fakegithub "github.com/giantswarm/beekeeper/internal/testing/fakes/github"
)

var start = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

// Fixtures of the seed and the steps: people, repositories, issues and pull requests, board options
// and checks.
const (
	alice      = "alice"
	bob        = "bob"
	fixtureOrg = "giantswarm"
	branchMain = "main"

	repoKlaus     = "giantswarm/klaus"
	refKlaus55    = "giantswarm/klaus#55"
	refHappa7     = "giantswarm/happa#7"
	refRoadmap90  = "giantswarm/roadmap#90"
	refRoadmap100 = "giantswarm/roadmap#100"
	refRoadmap101 = "giantswarm/roadmap#101"
	refRoadmap102 = "giantswarm/roadmap#102"
	refRoadmap103 = "giantswarm/roadmap#103"
	refRoadmap105 = "giantswarm/roadmap#105"

	optBumblebee  = "Bumblebee🐝"
	optUpNext     = "Up Next ➡️"
	optInProgress = "In Progress ⛏️"
	optBacklog    = "Backlog 📦"
	optInbox      = "Inbox 📥"
	optStory      = "Story 📑"
	optBug        = "Bug 🐞"

	checkGoBuild = "ci/circleci: go-build"
	checkGoTest  = "ci/circleci: go-test"
)

// GitHub's vocabulary as the fake speaks it: issue and pull request states, check run statuses and
// conclusions, commit status states, a repository's visibility and the fault kinds.
const (
	stateOpen         = "open"
	stateClosed       = "closed"
	checkCompleted    = "completed"
	conclusionSuccess = "success"
	statusSuccess     = "success"
	visibilityPublic  = "public"
	keyVisibility     = "visibility"
	faultHTTP         = "http_error"
	faultRateLimit    = "rate_limit"
)

// Arguments of the fake's step tools, which are also the keys of its seed.
const (
	argRef        = "ref"
	argTitle      = "title"
	argBody       = "body"
	argAuthor     = "author"
	argAssignees  = "assignees"
	argBoard      = "board"
	argDeliver    = "deliver"
	argState      = "state"
	argIssue      = "issue"
	argStatus     = "status"
	argKind       = "kind"
	argTeam       = "team"
	argPriority   = "priority"
	argParent     = "parent"
	argChild      = "child"
	argOp         = "op"
	argHead       = "head"
	argCloses     = "closes"
	argBy         = "by"
	argMergeSHA   = "merge_sha"
	argRepo       = "repo"
	argSHA        = "sha"
	argName       = "name"
	argConclusion = "conclusion"
	argContext    = "context"
	argTimes      = "times"
	argMatch      = "match"
	argRetryAfter = "retry_after"
	argAPI        = "api"
	argRemaining  = "remaining"
	argResetIn    = "reset_in"
	argSinceStep  = "since_step"
	argMethod     = "method"
	argPath       = "path"
	argOperation  = "operation"
)

// env is a client wired to a fake GitHub, as the harness wires them.
type env struct {
	t     *testing.T
	clock *clock.Fake
	fake  *fakegithub.Server
	gh    *github.Client
	mu    sync.Mutex
	reqs  []github.Request
	hooks int
}

func baseSeed() map[string]any {
	return map[string]any{
		"app":    map[string]any{"id": 100, "installation": 200, "webhookSecret": "fixture-webhook-secret"},
		argBoard: map[string]any{"project": 273, "teams": []any{"Atlas 🗺️", optBumblebee}},
		"org": map[string]any{"members": []any{
			map[string]any{"login": alice, "teams": []any{"team-bumblebee"}},
		}},
		"repos": []any{
			map[string]any{argName: "giantswarm/roadmap", keyVisibility: visibilityPublic},
			map[string]any{
				argName: repoKlaus, keyVisibility: visibilityPublic, "defaultBranch": branchMain, "owner": "team-bumblebee",
				"requiredChecks": []any{checkGoTest, checkGoBuild}, "requiredApprovals": 1,
			},
			map[string]any{argName: "giantswarm/happa", keyVisibility: "private", "installed": false},
			map[string]any{argName: "alice/klaus-experiments", keyVisibility: visibilityPublic, "installed": false},
		},
		"issues": []any{
			map[string]any{
				argRef: refRoadmap101, argTitle: "Retry transient errors", argBody: "### Acceptance criteria\n- [ ] retries\n",
				argAssignees: []any{alice},
				argBoard:     map[string]any{argStatus: optInProgress, argKind: optStory, argTeam: optBumblebee, argPriority: 2},
			},
			map[string]any{
				argRef: refRoadmap102, argTitle: "Up next", argParent: refRoadmap90,
				"blockedBy": []any{refRoadmap100},
				argBoard:    map[string]any{argStatus: optUpNext, argKind: optStory, argTeam: optBumblebee},
			},
			map[string]any{argRef: refRoadmap90, argTitle: "Epic", argBoard: map[string]any{argStatus: optUpNext, argKind: "Epic 🎯", argTeam: optBumblebee}},
			map[string]any{argRef: refRoadmap100, argTitle: "Blocker"},
			map[string]any{argRef: refRoadmap103, argTitle: "Backlog", argBoard: map[string]any{argStatus: optBacklog, argKind: optStory, argTeam: optBumblebee}},
			map[string]any{argRef: "giantswarm/roadmap#104", argTitle: "Atlas", argBoard: map[string]any{argStatus: optUpNext, argKind: optStory, argTeam: "Atlas 🗺️"}},
			map[string]any{argRef: refRoadmap105, argTitle: "Closed", argState: stateClosed, argBoard: map[string]any{argStatus: "Validation ☑️", argKind: optStory, argTeam: optBumblebee}},
			map[string]any{argRef: "giantswarm/roadmap#130", argTitle: "A bug", argBoard: map[string]any{argStatus: optInbox, argKind: optBug, argTeam: optBumblebee}},
			map[string]any{argRef: "giantswarm/roadmap#131", argTitle: "Inbox story", argBoard: map[string]any{argStatus: optInbox, argKind: optStory, argTeam: optBumblebee}},
		},
		"pulls": []any{
			map[string]any{
				argRef: refKlaus55, argAuthor: alice, argHead: "a1", argCloses: []any{refRoadmap101},
				"checks": []any{
					map[string]any{argName: checkGoBuild, argStatus: checkCompleted, argConclusion: conclusionSuccess},
					map[string]any{argName: checkGoTest, argStatus: "in_progress"},
				},
				"statuses": []any{map[string]any{argContext: "ci/circleci: lint", argState: "pending"}},
				"reviews":  []any{map[string]any{argBy: bob, argState: "approved"}},
			},
			map[string]any{argRef: refHappa7, argAuthor: alice, argHead: "h1", argCloses: []any{refRoadmap102}},
		},
		"rateLimit": map[string]any{
			forge.APIREST:    map[string]any{"limit": 5000, argRemaining: 5000},
			forge.APIGraphQL: map[string]any{"limit": 5000, argRemaining: 5000},
		},
	}
}

func newEnv(t *testing.T, seed map[string]any) *env {
	t.Helper()
	e := &env{t: t, clock: clock.NewFake(start)}
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		e.mu.Lock()
		e.hooks++
		e.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(recv.Close)
	fake, err := fakegithub.New(fakegithub.Options{Clock: e.clock, WebhookURL: recv.URL, WebhookSecret: "fixture-webhook-secret"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	if seed == nil {
		seed = baseSeed()
	}
	if err := fake.Seed(seed); err != nil {
		t.Fatal(err)
	}
	gh, err := github.New(github.Config{
		BaseURL:        fake.URL(),
		AppID:          100,
		InstallationID: 200,
		PrivateKeyPEM:  fake.AppKeyPEM(),
		Clock:          e.clock,
		UserAgent:      "beekeeper/test (engine-0)",
		Reserve:        0.2,
		Observer: func(r github.Request) {
			e.mu.Lock()
			e.reqs = append(e.reqs, r)
			e.mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.fake, e.gh = fake, gh
	return e
}

func (e *env) do(tool string, args map[string]any) any {
	e.t.Helper()
	res, err := e.fake.Do(context.Background(), tool, args)
	if err != nil {
		e.t.Fatalf("%s: %v", tool, err)
	}
	return res
}

func (e *env) calls(args map[string]any) map[string]any {
	e.t.Helper()
	return e.do("github_calls", args).(map[string]any)
}

func (e *env) requests() []github.Request {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]github.Request(nil), e.reqs...)
}

func wantKind(t *testing.T, err error, k forge.ErrorKind) *forge.Error {
	t.Helper()
	fe, ok := forge.AsError(err)
	if !ok || fe.Kind != k {
		t.Fatalf("want a %s error, got %v", k, err)
	}
	return fe
}

func ctx() context.Context { return context.Background() }

func TestTokenIsCachedUntilShortlyBeforeExpiry(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	mints := func() int {
		return e.calls(map[string]any{argAPI: "app", argOperation: forge.OpCreateInstallToken})["count"].(int)
	}
	for range 3 {
		if _, _, err := e.gh.PullRequest(ctx(), refKlaus55, ""); err != nil {
			t.Fatal(err)
		}
	}
	if got := mints(); got != 1 {
		t.Fatalf("mints after three reads: %d, want 1", got)
	}
	e.clock.Advance(54 * time.Minute) // the token expires at 10:00; 09:54 is still more than 5m before
	if _, _, err := e.gh.PullRequest(ctx(), refKlaus55, ""); err != nil {
		t.Fatal(err)
	}
	if got := mints(); got != 1 {
		t.Fatalf("mints at 09:54: %d, want 1", got)
	}
	e.clock.Advance(2 * time.Minute)
	if _, _, err := e.gh.PullRequest(ctx(), refKlaus55, ""); err != nil {
		t.Fatal(err)
	}
	if got := mints(); got != 2 {
		t.Fatalf("mints at 09:56: %d, want 2", got)
	}
	// Token minting is a POST, but not a mutating call.
	c := e.calls(map[string]any{argMethod: "POST"})
	if c["mutating"].(int) != 0 || c["app"].(int) != 2 {
		t.Fatalf("POSTs: %v", c)
	}
	// The mint is reported with its operation id under api app.
	var minted int
	for _, r := range e.requests() {
		if r.Operation == forge.OpCreateInstallToken {
			minted++
			if r.API != forge.APIApp || r.Code != http.StatusCreated || r.Method != http.MethodPost {
				t.Fatalf("mint reported as %+v", r)
			}
		}
	}
	if minted != 2 {
		t.Fatalf("mints reported: %d", minted)
	}
}

func TestConditionalReadAnswered304IsFree(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	pr, meta, err := e.gh.PullRequest(ctx(), refKlaus55, "")
	if err != nil {
		t.Fatal(err)
	}
	if pr.HeadSHA != "a1" || pr.State != stateOpen || pr.Author != alice || pr.Base != branchMain || meta.ETag == "" || meta.NotModified {
		t.Fatalf("first read: %+v %+v", pr, meta)
	}
	before := e.gh.Budget().REST
	_, meta2, err := e.gh.PullRequest(ctx(), refKlaus55, meta.ETag)
	if err != nil {
		t.Fatal(err)
	}
	if !meta2.NotModified || meta2.ETag != meta.ETag {
		t.Fatalf("second read: %+v", meta2)
	}
	after := e.gh.Budget().REST
	if after.Remaining != before.Remaining || !after.Known || after.Limit != 5000 {
		t.Fatalf("a 304 cost budget: before %+v after %+v", before, after)
	}
	c := e.calls(map[string]any{argPath: "/repos/giantswarm/klaus/pulls/55"})
	if c["count"] != 2 || c["conditional"] != 1 || c["notModified"] != 1 || c["modified"] != 1 {
		t.Fatalf("calls: %v", c)
	}
	reqs := e.requests()
	last := reqs[len(reqs)-1]
	if last.Code != http.StatusNotModified || !last.Conditional || !last.NotModified || last.Operation != forge.OpPullsGet ||
		last.API != forge.APIREST || last.Class != forge.ClassObservation || last.Limit == nil || last.Limit.Remaining != after.Remaining {
		t.Fatalf("304 reported as %+v", last)
	}

	// A change makes the next conditional read a 200 that costs one call.
	e.do("github_pr_push", map[string]any{argRef: refKlaus55, argHead: "a2"})
	pr, meta3, err := e.gh.PullRequest(ctx(), refKlaus55, meta.ETag)
	if err != nil {
		t.Fatal(err)
	}
	if meta3.NotModified || pr.HeadSHA != "a2" || meta3.ETag == meta.ETag {
		t.Fatalf("changed read: %+v %+v", pr, meta3)
	}
	if got := e.gh.Budget().REST.Remaining; got != after.Remaining-1 {
		t.Fatalf("remaining after a 200: %d, want %d", got, after.Remaining-1)
	}
}

func TestReserveDefersObservationUntilReset(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.do("github_fault", map[string]any{argKind: faultRateLimit, argAPI: forge.APIREST, argRemaining: 900, argResetIn: "30m"})
	// The first read learns the remaining budget from the headers.
	if _, _, err := e.gh.PullRequest(ctx(), refKlaus55, ""); err != nil {
		t.Fatal(err)
	}
	b := e.gh.Budget()
	if b.REST.Remaining != 899 || !b.REST.Paused || !b.REST.PausedUntil.Equal(start.Add(30*time.Minute)) || b.GraphQL.Paused {
		t.Fatalf("budget after learning: %+v", b)
	}
	n := len(e.fake.Calls())
	_, _, err := e.gh.CheckRuns(ctx(), repoKlaus, "a1", "")
	fe := wantKind(t, err, forge.Deferred)
	if !fe.ResetAt.Equal(start.Add(30*time.Minute)) || fe.Code != 0 || fe.Operation != forge.OpChecksListForRef {
		t.Fatalf("deferral: %+v", fe)
	}
	if len(e.fake.Calls()) != n {
		t.Fatal("a deferred read was sent")
	}
	if got := e.gh.Budget().DeferredREST; got != 1 {
		t.Fatalf("deferred reads: %d", got)
	}
	// The metrics class isn't held by the observation reserve, and GraphQL has its own bucket.
	if _, _, err := e.gh.CheckRuns(forge.WithClass(ctx(), forge.ClassMetrics), repoKlaus, "a1", ""); err != nil {
		t.Fatalf("metrics read: %v", err)
	}
	if _, err := e.gh.BoardSchema(ctx(), fixtureOrg, 273); err != nil {
		t.Fatalf("GraphQL read: %v", err)
	}
	// After the reset the window is full again and observation resumes.
	e.clock.Advance(30 * time.Minute)
	b = e.gh.Budget()
	if b.REST.Paused || b.REST.Remaining != 5000 {
		t.Fatalf("budget at the reset: %+v", b.REST)
	}
	if _, _, err := e.gh.CheckRuns(ctx(), repoKlaus, "a1", ""); err != nil {
		t.Fatal(err)
	}
	if got := e.gh.Budget().REST.Remaining; got < 4990 {
		t.Fatalf("remaining after the reset: %d", got)
	}
}

func TestPrimaryRateLimitStopsTheAPIUntilReset(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	if _, err := e.gh.BoardSchema(ctx(), fixtureOrg, 273); err != nil { // the token is minted
		t.Fatal(err)
	}
	e.do("github_fault", map[string]any{argKind: faultRateLimit, argAPI: forge.APIGraphQL, argRemaining: 0, argResetIn: "30m"})
	e.fake.MarkStep("limited")

	_, _, err := e.gh.BoardItem(ctx(), "PVTI_giantswarm_273_giantswarm/roadmap#101")
	fe := wantKind(t, err, forge.RateLimited)
	reset := start.Add(30 * time.Minute)
	if !fe.ResetAt.Equal(reset) || fe.Operation != forge.OpBoardItemsByNode {
		t.Fatalf("rate limited: %+v", fe)
	}
	c := e.calls(map[string]any{argSinceStep: "limited", argAPI: forge.APIGraphQL})
	if c["count"] != 1 || c["rateLimited"] != 1 {
		t.Fatalf("calls: %v", c)
	}
	// No request is made to a stopped API, of any class.
	for _, cx := range []context.Context{ctx(), forge.WithClass(ctx(), forge.ClassMetrics)} {
		_, err = e.gh.BoardTeamItems(cx, forge.TeamItemsQuery{Org: fixtureOrg, Project: 273, TeamOption: optBumblebee, Statuses: []string{optUpNext}})
		_ = wantKind(t, err, forge.RateLimited) // the kind is all this checks
	}
	if c := e.calls(map[string]any{argSinceStep: "limited", argAPI: forge.APIGraphQL}); c["count"] != 1 {
		t.Fatalf("calls while stopped: %v", c)
	}
	b := e.gh.Budget()
	if !b.GraphQL.Paused || b.GraphQL.Remaining != 0 || !b.GraphQL.ResetAt.Equal(reset) || b.REST.Paused {
		t.Fatalf("budget: %+v", b)
	}
	// REST is a separate bucket.
	if _, _, err := e.gh.PullRequest(ctx(), refKlaus55, ""); err != nil {
		t.Fatal(err)
	}
	e.clock.Set(reset)
	if _, found, err := e.gh.BoardItem(ctx(), "PVTI_giantswarm_273_giantswarm/roadmap#101"); err != nil || !found {
		t.Fatalf("after the reset: found %v, %v", found, err)
	}

	// A REST primary limit is a 403 with remaining 0.
	e.do("github_fault", map[string]any{argKind: faultRateLimit, argAPI: forge.APIREST, argRemaining: 0, argResetIn: "10m"})
	_, _, err = e.gh.PullRequest(ctx(), refKlaus55, "")
	fe = wantKind(t, err, forge.RateLimited)
	if fe.Code != http.StatusForbidden || !fe.ResetAt.Equal(reset.Add(10*time.Minute)) {
		t.Fatalf("REST rate limit: %+v", fe)
	}
}

func TestSecondaryRateLimitPausesForRetryAfter(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	if _, _, err := e.gh.PullRequest(ctx(), refKlaus55, ""); err != nil {
		t.Fatal(err)
	}
	e.do("github_fault", map[string]any{argKind: "secondary_rate_limit", argRetryAfter: "60s", argTimes: 1, argMatch: "/repos/giantswarm/klaus/commits/a1/check-runs"})
	e.fake.MarkStep("secondary")
	_, _, err := e.gh.CheckRuns(ctx(), repoKlaus, "a1", "")
	fe := wantKind(t, err, forge.SecondaryRateLimited)
	if fe.RetryAfter != time.Minute || fe.Code != http.StatusForbidden || !fe.ResetAt.Equal(start.Add(time.Minute)) {
		t.Fatalf("secondary: %+v", fe)
	}
	// Every call waits: REST with the token and the delivery log with the App's JWT.
	e.clock.Advance(59 * time.Second)
	_, _, err = e.gh.PullRequest(ctx(), refKlaus55, "")
	if fe := wantKind(t, err, forge.SecondaryRateLimited); fe.RetryAfter != time.Second {
		t.Fatalf("retry after: %v", fe.RetryAfter)
	}
	_, _, err = e.gh.Deliveries(ctx(), "")
	_ = wantKind(t, err, forge.SecondaryRateLimited) // the kind is all this checks
	if c := e.calls(map[string]any{argSinceStep: "secondary"}); c["count"] != 1 || c["rateLimited"] != 1 {
		t.Fatalf("calls: %v", c)
	}
	e.clock.Advance(time.Second)
	runs, _, err := e.gh.CheckRuns(ctx(), repoKlaus, "a1", "")
	if err != nil || len(runs) != 2 {
		t.Fatalf("retry: %v %v", runs, err)
	}
}

func TestFailuresAreClassified(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		fault  map[string]any
		ref    string
		kind   forge.ErrorKind
		code   int
		noCall bool
	}{
		{name: "404", fault: map[string]any{argKind: faultHTTP, argStatus: 404}, kind: forge.Refused, code: 404},
		{name: "410", fault: map[string]any{argKind: faultHTTP, argStatus: 410}, kind: forge.Refused, code: 410},
		{name: "422", fault: map[string]any{argKind: faultHTTP, argStatus: 422}, kind: forge.Refused, code: 422},
		{name: "403 without a rate limit", fault: map[string]any{argKind: faultHTTP, argStatus: 403}, kind: forge.Refused, code: 403},
		{name: "500", fault: map[string]any{argKind: faultHTTP, argStatus: 500}, kind: forge.Transient, code: 500},
		{name: "502", fault: map[string]any{argKind: faultHTTP, argStatus: 502}, kind: forge.Transient, code: 502},
		{name: "private repo outside the installation", ref: refHappa7, kind: forge.Refused, code: 404},
		{name: "malformed ref", ref: "nonsense", kind: forge.Refused, code: 0, noCall: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, nil)
			ref := refKlaus55
			if tc.ref != "" {
				ref = tc.ref
			}
			if tc.fault != nil {
				tc.fault["match"] = "/repos/giantswarm/klaus/pulls"
				e.do("github_fault", tc.fault)
			}
			_, _, err := e.gh.PullRequest(ctx(), ref, "")
			fe := wantKind(t, err, tc.kind)
			if fe.Code != tc.code || fe.Operation != forge.OpPullsGet {
				t.Fatalf("error: %+v", fe)
			}
			if tc.noCall && len(e.fake.Calls()) != 0 {
				t.Fatal("a request was made")
			}
		})
	}

	t.Run("public repo outside the installation is read", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, nil)
		e.do("github_pr_open", map[string]any{argRef: "alice/klaus-experiments#3", argAuthor: alice, argHead: "c1", argCloses: []any{refRoadmap101}})
		pr, _, err := e.gh.PullRequest(ctx(), "alice/klaus-experiments#3", "")
		if err != nil || pr.HeadSHA != "c1" {
			t.Fatalf("%+v %v", pr, err)
		}
	})

	t.Run("transport error", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, nil)
		e.fake.Close()
		_, _, err := e.gh.PullRequest(ctx(), refKlaus55, "")
		if fe := wantKind(t, err, forge.Transient); fe.Code != 0 {
			t.Fatalf("%+v", fe)
		}
	})

	t.Run("refused GraphQL", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, nil)
		_, err := e.gh.BoardSchema(ctx(), fixtureOrg, 999)
		_ = wantKind(t, err, forge.Refused) // the kind is all this checks
	})

	t.Run("5xx on GraphQL", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, nil)
		e.do("github_fault", map[string]any{argKind: faultHTTP, argStatus: 502, argMatch: "BoardProbe", argTimes: 1})
		_, err := e.gh.BoardProbe(ctx(), forge.ProbeQuery{Org: fixtureOrg, Project: 273, TeamOption: optBumblebee})
		if fe := wantKind(t, err, forge.Transient); fe.Code != 502 || fe.API != forge.APIGraphQL {
			t.Fatalf("%+v", fe)
		}
		if _, err := e.gh.BoardProbe(ctx(), forge.ProbeQuery{Org: fixtureOrg, Project: 273, TeamOption: optBumblebee}); err != nil {
			t.Fatalf("after the fault: %v", err)
		}
	})
}

func TestBoardTeamItemsIsOneGraphQLCall(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	q := forge.TeamItemsQuery{
		Org: fixtureOrg, Project: 273, TeamOption: optBumblebee,
		Statuses:    []string{optUpNext, optInProgress, "Blocked / Waiting ⛔️", "Validation ☑️"},
		InboxStatus: optInbox, BugKind: optBug,
	}
	items, err := e.gh.BoardTeamItems(ctx(), q)
	if err != nil {
		t.Fatal(err)
	}
	c := e.calls(map[string]any{argAPI: forge.APIGraphQL})
	if c["count"] != 1 || c["graphqlPoints"] != 2 || items.Points != 2 {
		t.Fatalf("calls %v, points %d", c, items.Points)
	}
	var active []string
	for _, it := range items.Active {
		active = append(active, it.Issue.Ref)
	}
	want := []string{refRoadmap101, refRoadmap102, refRoadmap90}
	if len(active) != len(want) {
		t.Fatalf("active: %v, want %v", active, want)
	}
	for i := range want {
		if active[i] != want[i] {
			t.Fatalf("active: %v, want %v", active, want)
		}
	}
	if len(items.InboxBugs) != 1 || items.InboxBugs[0].Issue.Ref != "giantswarm/roadmap#130" || items.InboxBugs[0].Kind != optBug {
		t.Fatalf("inbox bugs: %+v", items.InboxBugs)
	}
	it := items.Active[0]
	if it.NodeID != "PVTI_giantswarm_273_giantswarm/roadmap#101" || !it.IsIssue || it.Status != optInProgress || it.Team != optBumblebee ||
		it.Kind != optStory || it.Priority == nil || *it.Priority != 2 || !it.StatusAt.Equal(start) {
		t.Fatalf("item: %+v", it)
	}
	is := it.Issue
	if is.NodeID != "I_giantswarm/roadmap#101" || is.State != stateOpen || len(is.Assignees) != 1 || is.Assignees[0] != alice ||
		len(is.ClosedBy) != 1 || is.ClosedBy[0].Ref != refKlaus55 || is.ClosedBy[0].State != stateOpen {
		t.Fatalf("issue: %+v", is)
	}
	up := items.Active[1].Issue
	if up.Parent != refRoadmap90 || len(up.BlockedBy) != 1 || up.BlockedBy[0] != (forge.LinkedIssue{Ref: refRoadmap100, State: stateOpen}) {
		t.Fatalf("links: %+v", up)
	}
	// A closing PR in a private repo outside the installation is listed; reading it is refused.
	if len(up.ClosedBy) != 1 || up.ClosedBy[0].Ref != refHappa7 {
		t.Fatalf("closed by: %+v", up.ClosedBy)
	}
	epic := items.Active[2].Issue
	if len(epic.SubIssues) != 1 || epic.SubIssues[0].Ref != refRoadmap102 {
		t.Fatalf("sub-issues: %+v", epic.SubIssues)
	}
	reqs := e.requests()
	last := reqs[len(reqs)-1]
	if last.Operation != forge.OpBoardTeamItems || last.Points != 2 || last.API != forge.APIGraphQL || last.Method != http.MethodPost || last.Code != 200 {
		t.Fatalf("reported: %+v", last)
	}
}

func TestBoardTeamItemsPagesLargeTeams(t *testing.T) {
	t.Parallel()
	seed := baseSeed()
	var issues []any
	for i := range 40 {
		issues = append(issues, map[string]any{
			argRef: "giantswarm/roadmap#" + itoa(300+i), argTitle: "item",
			argBoard: map[string]any{argStatus: optUpNext, argKind: optStory, argTeam: optBumblebee},
		})
	}
	seed["issues"] = issues
	seed["pulls"] = []any{}
	e := newEnv(t, seed)
	items, err := e.gh.BoardTeamItems(ctx(), forge.TeamItemsQuery{
		Org: fixtureOrg, Project: 273, TeamOption: optBumblebee, Statuses: []string{optUpNext}, InboxStatus: optInbox, BugKind: optBug,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items.Active) != 40 {
		t.Fatalf("active: %d", len(items.Active))
	}
	c := e.calls(map[string]any{argOperation: forge.OpBoardTeamItems})
	// The second page asks only for the active items: 1 + 5×30 requests, 2 points.
	if c["count"] != 2 || c["graphqlPoints"] != 4 || items.Points != 4 {
		t.Fatalf("calls: %v, points %d", c, items.Points)
	}
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{digits[i%10]}, b...)
	}
	return string(b)
}

func TestBoardReads(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)

	schema, err := e.gh.BoardSchema(ctx(), fixtureOrg, 273)
	if err != nil {
		t.Fatal(err)
	}
	if schema.ProjectID != "PVT_giantswarm_273" || len(schema.Options["Team"]) != 2 || schema.Options["Team"][1] != optBumblebee ||
		len(schema.Options["Status"]) != 8 {
		t.Fatalf("schema: %+v", schema)
	}

	items, err := e.gh.BoardItemsByNode(ctx(), []string{"PVTI_giantswarm_273_giantswarm/roadmap#103", "PVTI_giantswarm_273_giantswarm/roadmap#999"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items["PVTI_giantswarm_273_giantswarm/roadmap#103"].Status != optBacklog {
		t.Fatalf("items: %+v", items)
	}

	e.do("github_board_archive", map[string]any{argIssue: refRoadmap101})
	it, found, err := e.gh.BoardItem(ctx(), "PVTI_giantswarm_273_giantswarm/roadmap#101")
	if err != nil || !found || !it.Archived {
		t.Fatalf("archived: %+v %v %v", it, found, err)
	}
	e.do("github_board_remove", map[string]any{argIssue: refRoadmap101})
	if _, found, err := e.gh.BoardItem(ctx(), "PVTI_giantswarm_273_giantswarm/roadmap#101"); err != nil || found {
		t.Fatalf("deleted: found %v, %v", found, err)
	}

	e.clock.Advance(time.Minute)
	e.do("github_board_set", map[string]any{argIssue: refRoadmap102, argStatus: optInProgress})
	probe, err := e.gh.BoardProbe(ctx(), forge.ProbeQuery{Org: fixtureOrg, Project: 273, TeamOption: optBumblebee})
	if err != nil {
		t.Fatal(err)
	}
	if len(probe.Items) == 0 || probe.Items[0].IssueRef != refRoadmap102 || !probe.Items[0].UpdatedAt.Equal(start.Add(time.Minute)) {
		t.Fatalf("probe: %+v", probe)
	}
	for _, op := range []string{forge.OpBoardSchema, forge.OpBoardItemsByNode, forge.OpBoardProbe} {
		if c := e.calls(map[string]any{argOperation: op}); c["graphqlPoints"] != c["count"] {
			t.Fatalf("%s costs more than 1 point: %v", op, c)
		}
	}
	if c := e.calls(map[string]any{argOperation: forge.OpBoardItemsByNode}); c["count"] != 3 {
		t.Fatalf("BoardItem must send BoardItemsByNode: %v", c)
	}
}

func TestRESTReads(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	is, _, err := e.gh.Issue(ctx(), refRoadmap101, "")
	if err != nil || is.Title != "Retry transient errors" || is.NodeID != "I_giantswarm/roadmap#101" || is.Assignees[0] != alice {
		t.Fatalf("issue: %+v %v", is, err)
	}
	repo, _, err := e.gh.Repo(ctx(), repoKlaus, "")
	if err != nil || repo.DefaultBranch != branchMain || repo.Private {
		t.Fatalf("repo: %+v %v", repo, err)
	}
	reviews, _, err := e.gh.Reviews(ctx(), refKlaus55, "")
	if err != nil || len(reviews) != 1 || reviews[0].By != bob || reviews[0].State != "APPROVED" {
		t.Fatalf("reviews: %+v %v", reviews, err)
	}
	runs, _, err := e.gh.CheckRuns(ctx(), repoKlaus, "a1", "")
	if err != nil || len(runs) != 2 {
		t.Fatalf("check runs: %+v %v", runs, err)
	}
	e.do("github_status", map[string]any{argRepo: repoKlaus, argSHA: "a1", argContext: "ci/circleci: lint", argState: statusSuccess})
	sts, _, err := e.gh.Statuses(ctx(), repoKlaus, "a1", "")
	if err != nil || len(sts) != 1 || sts[0].State != statusSuccess {
		t.Fatalf("statuses (latest per context): %+v %v", sts, err)
	}
	rules, _, err := e.gh.BranchRules(ctx(), repoKlaus, branchMain, "")
	if err != nil || rules.RequiredApprovals != 1 || len(rules.RequiredChecks) != 2 {
		t.Fatalf("rules: %+v %v", rules, err)
	}
	repos, meta, err := e.gh.InstallationRepos(ctx(), "")
	if err != nil || len(repos) != 2 || repos[0] != repoKlaus {
		t.Fatalf("installation repos: %v %v", repos, err)
	}
	if _, meta2, err := e.gh.InstallationRepos(ctx(), meta.ETag); err != nil || !meta2.NotModified {
		t.Fatalf("conditional installation repos: %+v %v", meta2, err)
	}
	e.do("github_pr_merge", map[string]any{argRef: refKlaus55, argMergeSHA: "m1", argBy: bob})
	pr, _, err := e.gh.PullRequest(ctx(), refKlaus55, "")
	if err != nil || !pr.Merged || pr.MergeSHA != "m1" || pr.State != stateClosed || !pr.MergedAt.Equal(start) {
		t.Fatalf("merged PR: %+v %v", pr, err)
	}
	ops := map[string]bool{}
	for _, r := range e.requests() {
		ops[r.Operation] = true
		if r.API == forge.APIREST && r.Method != http.MethodGet {
			t.Fatalf("a REST %s", r.Method)
		}
	}
	for _, op := range []string{forge.OpIssuesGet, forge.OpReposGet, forge.OpPullsListReviews, forge.OpChecksListForRef,
		forge.OpStatusesListForRef, forge.OpReposGetBranchRules, forge.OpListInstallationRepos, forge.OpPullsGet} {
		if !ops[op] {
			t.Fatalf("operation %s not reported", op)
		}
	}
	if c := e.calls(nil); c["mutating"] != 0 {
		t.Fatalf("mutating calls: %v", c)
	}
}

func TestDeliveryLog(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.do("github_check_run", map[string]any{argRepo: repoKlaus, argSHA: "a1", argName: checkGoTest, argStatus: checkCompleted, argConclusion: conclusionSuccess})
	e.do("github_check_run", map[string]any{argRepo: repoKlaus, argSHA: "a1", argName: checkGoBuild, argStatus: checkCompleted, argConclusion: "failure", argDeliver: "lost"})
	ds, cursor, err := e.gh.Deliveries(ctx(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 2 || ds[0].Failed() || !ds[1].Failed() || ds[0].Event != "check_run" || ds[0].Action != checkCompleted || cursor != ds[1].ID {
		t.Fatalf("deliveries: %+v cursor %s", ds, cursor)
	}
	detail, err := e.gh.Delivery(ctx(), ds[1].ID)
	if err != nil || detail.GUID != ds[1].GUID || len(detail.Payload) == 0 {
		t.Fatalf("detail: %+v %v", detail, err)
	}
	ds, cursor2, err := e.gh.Deliveries(ctx(), cursor)
	if err != nil || len(ds) != 0 || cursor2 != cursor {
		t.Fatalf("nothing new: %+v %s %v", ds, cursor2, err)
	}
	e.do("github_status", map[string]any{argRepo: repoKlaus, argSHA: "a1", argContext: "x", argState: statusSuccess})
	ds, _, err = e.gh.Deliveries(ctx(), cursor)
	if err != nil || len(ds) != 1 || ds[0].Event != "status" {
		t.Fatalf("new: %+v %v", ds, err)
	}
	for _, r := range e.requests() {
		if (r.Operation == forge.OpListWebhookDeliveries || r.Operation == forge.OpGetWebhookDelivery) && r.API != forge.APIApp {
			t.Fatalf("delivery log read as %+v", r)
		}
	}
	// The delivery log is read with the App's JWT: no installation token is minted for it.
	if c := e.calls(map[string]any{argOperation: forge.OpCreateInstallToken}); c["count"] != 0 {
		t.Fatalf("mints: %v", c)
	}
}

func TestDeliveryLogPagesBackToTheCursor(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	ds, cursor, err := e.gh.Deliveries(ctx(), "")
	if err != nil || len(ds) != 0 || cursor != "0" {
		t.Fatalf("empty log: %v %q %v", ds, cursor, err)
	}
	for i := range 150 {
		e.do("github_status", map[string]any{argRepo: repoKlaus, argSHA: "a1", argContext: "c" + itoa(i), argState: statusSuccess})
	}
	e.fake.MarkStep("page")
	ds, _, err = e.gh.Deliveries(ctx(), cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 150 {
		t.Fatalf("deliveries: %d", len(ds))
	}
	if c := e.calls(map[string]any{argSinceStep: "page", argOperation: forge.OpListWebhookDeliveries}); c["count"] != 2 {
		t.Fatalf("pages: %v", c)
	}
}

func TestNewValidatesItsConfig(t *testing.T) {
	t.Parallel()
	c := clock.NewFake(start)
	const baseURL = "http://x"
	for name, cfg := range map[string]github.Config{
		"no base URL": {AppID: 1, InstallationID: 1, Clock: c},
		"no app":      {BaseURL: baseURL, Clock: c},
		"no clock":    {BaseURL: baseURL, AppID: 1, InstallationID: 1},
		"bad key":     {BaseURL: baseURL, AppID: 1, InstallationID: 1, Clock: c, PrivateKeyPEM: []byte("nope")},
		"bad reserve": {BaseURL: baseURL, AppID: 1, InstallationID: 1, Clock: c, Reserve: 1.5},
	} {
		if _, err := github.New(cfg); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestTokenAsksForReadPermissionsOnly(t *testing.T) {
	t.Parallel()
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/200/access_tokens" {
			body, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"token":"ghs_x","expires_at":"2026-11-16T10:00:00Z"}`)
			return
		}
		_, _ = io.WriteString(w, `{"full_name":"giantswarm/klaus"}`)
	}))
	t.Cleanup(srv.Close)
	gh := cannedClient(t, srv.URL)
	if _, _, err := gh.Repo(ctx(), repoKlaus, ""); err != nil {
		t.Fatal(err)
	}
	var req struct {
		Permissions map[string]string `json:"permissions"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("mint body %s: %v", body, err)
	}
	const read = "read"
	want := map[string]string{"issues": read, "pull_requests": read, "checks": read, "statuses": read, "contents": read,
		"metadata": read, "organization_projects": read}
	if len(req.Permissions) != len(want) {
		t.Fatalf("permissions: %v", req.Permissions)
	}
	for p, level := range want {
		if req.Permissions[p] != level {
			t.Fatalf("permissions: %v", req.Permissions)
		}
	}
}

// cannedClient is a client of a hand-written GitHub at url.
func cannedClient(t *testing.T, url string) *github.Client {
	t.Helper()
	keys, err := fakegithub.New(fakegithub.Options{Clock: clock.NewFake(start)})
	if err != nil {
		t.Fatal(err)
	}
	keys.Close()
	gh, err := github.New(github.Config{BaseURL: url, AppID: 100, InstallationID: 200, PrivateKeyPEM: keys.AppKeyPEM(), Clock: clock.NewFake(start)})
	if err != nil {
		t.Fatal(err)
	}
	return gh
}

// cannedGraphQL is a GitHub that answers every GraphQL query with answer.
func cannedGraphQL(t *testing.T, answer string) *github.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/200/access_tokens" {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"token":"ghs_x","expires_at":"2026-11-16T10:00:00Z"}`)
			return
		}
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(srv.Close)
	return cannedClient(t, srv.URL)
}

func TestBoardProbePagesThroughEveryUpdatedItem(t *testing.T) {
	t.Parallel()
	seed := baseSeed()
	var issues []any
	for i := range 150 {
		issues = append(issues, map[string]any{
			argRef: "giantswarm/roadmap#" + itoa(300+i), argTitle: "item",
			argBoard: map[string]any{argStatus: optBacklog, argKind: optStory, argTeam: optBumblebee},
		})
	}
	issues = append(issues, map[string]any{argRef: "giantswarm/happa#1", argTitle: "private",
		argBoard: map[string]any{argStatus: optBacklog, argKind: optStory, argTeam: optBumblebee}})
	seed["issues"] = issues
	seed["pulls"] = []any{}
	e := newEnv(t, seed)
	e.clock.Advance(2 * time.Hour)
	// Two items change after the bound; the first-seeded one sits at the top of the board.
	e.do("github_board_set", map[string]any{argIssue: "giantswarm/roadmap#300", argStatus: optUpNext})
	e.clock.Advance(time.Minute)
	e.do("github_board_set", map[string]any{argIssue: "giantswarm/roadmap#449", argStatus: optUpNext})
	e.fake.MarkStep("probe")
	probe, err := e.gh.BoardProbe(ctx(), forge.ProbeQuery{Org: fixtureOrg, Project: 273, TeamOption: optBumblebee, First: 20,
		UpdatedSince: start.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	// Seeded at 09:00, the other items match the day filter but are older than the bound.
	if len(probe.Items) != 2 || probe.Truncated || probe.Items[0].IssueRef != "giantswarm/roadmap#449" || probe.Items[1].IssueRef != "giantswarm/roadmap#300" ||
		!probe.Items[0].IsIssue || !probe.Items[1].IsIssue {
		t.Fatalf("probe: %+v", probe)
	}
	// 151 items match the day filter: two pages of 100, 1 point each, whatever First asks.
	if c := e.calls(map[string]any{argSinceStep: "probe", argOperation: forge.OpBoardProbe}); c["count"] != 2 || c["graphqlPoints"] != 2 {
		t.Fatalf("pages: %v", c)
	}
	// Without a bound: the last 24 hours, every item, the redacted one not an issue.
	probe, err = e.gh.BoardProbe(ctx(), forge.ProbeQuery{Org: fixtureOrg, Project: 273, TeamOption: optBumblebee})
	if err != nil || len(probe.Items) != 151 {
		t.Fatalf("probe: %d items, %v", len(probe.Items), err)
	}
	var redacted int
	for _, it := range probe.Items {
		if !it.IsIssue {
			redacted++
			if it.IssueRef != "" || it.NodeID != "PVTI_giantswarm_273_giantswarm/happa#1" {
				t.Fatalf("redacted: %+v", it)
			}
		}
	}
	if redacted != 1 {
		t.Fatalf("redacted items: %d", redacted)
	}
}

func TestLinkTotalsTellTruncatedLists(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	for i := range 22 {
		ref := "giantswarm/roadmap#" + itoa(700+i)
		e.do("github_issue_create", map[string]any{argRef: ref, argTitle: "sub"})
		e.do("github_sub_issue", map[string]any{argParent: refRoadmap90, argChild: ref, argOp: "add"})
	}
	items, err := e.gh.BoardItemsByNode(ctx(), []string{"PVTI_giantswarm_273_giantswarm/roadmap#90", "PVTI_giantswarm_273_giantswarm/roadmap#102"})
	if err != nil {
		t.Fatal(err)
	}
	epic := items["PVTI_giantswarm_273_giantswarm/roadmap#90"].Issue
	// #102 was a sub-issue already: 23 in all, 20 listed.
	if len(epic.SubIssues) != 20 || epic.SubIssuesTotal != 23 || epic.BlockedByTotal != 0 {
		t.Fatalf("epic: %d listed, totals %d/%d", len(epic.SubIssues), epic.SubIssuesTotal, epic.BlockedByTotal)
	}
	up := items["PVTI_giantswarm_273_giantswarm/roadmap#102"].Issue
	if len(up.BlockedBy) != 1 || up.BlockedByTotal != 1 || up.SubIssuesTotal != 0 {
		t.Fatalf("blocked: %+v", up)
	}
	// The totals are free: the board query still costs 2 points.
	if _, err := e.gh.BoardTeamItems(ctx(), forge.TeamItemsQuery{Org: fixtureOrg, Project: 273, TeamOption: optBumblebee,
		Statuses: []string{optUpNext, optInProgress}, InboxStatus: optInbox, BugKind: optBug}); err != nil {
		t.Fatal(err)
	}
	if c := e.calls(map[string]any{argOperation: forge.OpBoardTeamItems}); c["graphqlPoints"] != 2 {
		t.Fatalf("cost: %v", c)
	}
}

func TestPartialGraphQLAnswers(t *testing.T) {
	t.Parallel()
	const item = `{"id":"PVTI_1","type":"ISSUE","updatedAt":"2026-11-16T09:00:00Z","team":{"name":"Bumblebee🐝"},
		"content":{"__typename":"Issue","id":"I_1","number":%d,"state":"OPEN","repository":{"nameWithOwner":"giantswarm/roadmap"},
		"subIssuesSummary":{"total":2},"subIssues":{"nodes":[{"number":7,"state":"OPEN","repository":{"nameWithOwner":"giantswarm/roadmap"}},null]}}}`
	team := forge.TeamItemsQuery{Org: fixtureOrg, Project: 273, TeamOption: optBumblebee, Statuses: []string{optUpNext}}
	page := func(errs string) string {
		return `{"data":{"rateLimit":{"cost":2},"organization":{"projectV2":{"id":"PVT","active":{"totalCount":2,"pageInfo":{"hasNextPage":false},"nodes":[` +
			fmt.Sprintf(item, 1) + `,` + fmt.Sprintf(item, 2) + `]}}}},"errors":` + errs + `}`
	}

	// An error about a link of the second item marks that item, and only it.
	gh := cannedGraphQL(t, page(`[{"type":"FORBIDDEN","message":"Resource not accessible by integration",
		"path":["organization","projectV2","active","nodes",1,"content","subIssues","nodes",1]}]`))
	items, err := gh.BoardTeamItems(ctx(), team)
	if err != nil {
		t.Fatal(err)
	}
	if len(items.Active) != 2 || items.Active[0].Incomplete || !items.Active[1].Incomplete {
		t.Fatalf("incomplete: %+v", items.Active)
	}
	// The unresolved sub-issue is left out of the list; the total still counts it.
	if is := items.Active[1].Issue; len(is.SubIssues) != 1 || is.SubIssuesTotal != 2 {
		t.Fatalf("links: %+v", is)
	}

	// An error that isn't about one item spoils the answer: a transient error, retried.
	for name, errs := range map[string]string{
		"no path":     `[{"message":"Something went wrong while executing your query."}]`,
		"a null item": `[{"type":"FORBIDDEN","message":"no","path":["organization","projectV2","active","nodes",1]}]`,
		"above items": `[{"type":"FORBIDDEN","message":"no","path":["organization","projectV2","active"]}]`,
	} {
		gh := cannedGraphQL(t, page(errs))
		_, err := gh.BoardTeamItems(ctx(), team)
		if fe := wantKind(t, err, forge.Transient); fe.Operation != forge.OpBoardTeamItems {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestPullClosingIssues(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.do("github_pr_open", map[string]any{argRef: "giantswarm/klaus#56", argAuthor: alice, argHead: "b1",
		argCloses: []any{refRoadmap105, refRoadmap103}})
	e.fake.MarkStep("read")
	got, err := e.gh.PullClosingIssues(ctx(), "giantswarm/klaus#56")
	if err != nil {
		t.Fatal(err)
	}
	want := []forge.LinkedIssue{
		{Ref: refRoadmap105, State: stateClosed, StateReason: "completed"},
		{Ref: refRoadmap103, State: stateOpen},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("closing issues: %+v", got)
	}
	if c := e.calls(map[string]any{argSinceStep: "read", argAPI: forge.APIGraphQL}); c["count"] != 1 || c["graphqlPoints"] != 1 {
		t.Fatalf("cost: %v", c)
	}
	if got, err := e.gh.PullClosingIssues(ctx(), refKlaus55); err != nil || len(got) != 1 || got[0].Ref != refRoadmap101 {
		t.Fatalf("klaus#55: %+v %v", got, err)
	}
	for _, ref := range []string{"giantswarm/klaus#999", refHappa7, "not-a-ref"} {
		_, err := e.gh.PullClosingIssues(ctx(), ref)
		if fe := wantKind(t, err, forge.Refused); fe.Operation != forge.OpPullClosingIssues {
			t.Fatalf("%s: %v", ref, err)
		}
	}
}
