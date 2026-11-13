package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const watchFixtureUpdated = "2026-01-01T00:00:00Z"

// Fake the actual GraphQL transport, including its connection shape, rather
// than replacing the batch function for request-budget and decoding tests.
func fakePullGraphQL(t *testing.T, answer func(string, int) map[string]any) *int {
	t.Helper()
	old := github.RunGH
	t.Cleanup(func() { github.RunGH = old })
	calls := new(int)
	alias := regexp.MustCompile(`p([0-9]+): repository\(owner:"([^"]+)",name:"([^"]+)"\) \{ pullRequest\(number:([0-9]+)\)`)
	github.RunGH = func(_ context.Context, args ...string) ([]byte, error) {
		*calls++
		if len(args) != 4 || args[0] != workerGitHubAPIGroup || args[1] != inventoryGraphQLCommand {
			t.Fatalf("non-batched request: %v", args)
		}
		query := strings.TrimPrefix(args[3], "query=")
		matches := alias.FindAllStringSubmatchIndex(query, -1)
		data := map[string]any{"rateLimit": map[string]any{"remaining": 4900}}
		for n, m := range matches {
			end := len(query)
			if n+1 < len(matches) {
				end = matches[n+1][0]
			}
			key := "p" + query[m[2]:m[3]]
			data[key] = map[string]any{"pullRequest": answer(query[m[0]:end], *calls)}
		}
		raw, err := json.Marshal(map[string]any{"data": data})
		if err != nil {
			t.Fatal(err)
		}
		return raw, nil
	}
	return calls
}

func pullGraphQLFixture(t *testing.T, query, status, head, updated string) map[string]any {
	t.Helper()
	raw := fmt.Sprintf(`{
 "state":"OPEN","url":"%s","headRefOid":"%s","updatedAt":"%s",
 "mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","reviewDecision":"APPROVED",
 "author":{"login":"desk"},"title":"fix: observed","body":"kept detail","changedFiles":1,
 "files":{"nodes":[{"path":"main.go"}]},"closingIssuesReferences":{"nodes":[]},
 "reviews":{"nodes":[{"id":"R1","author":{"login":"desk"},"state":"APPROVED","commit":{"oid":"%s"}}]},
 "commits":{"nodes":[{"commit":{"checkSuites":{"nodes":[]},"statusCheckRollup":{"state":"%s","contexts":{"nodes":[
 {"__typename":"StatusContext","context":"CI","state":"%s","targetUrl":"https://example.test/ci"}
 ]}}}}]}}`, loopURL, head, updated, head, status, status)
	var p map[string]any
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "author { login }") {
		for _, key := range []string{"author", publicationTitle, "body", "files", "reviews", "closingIssuesReferences", "changedFiles"} {
			delete(p, key)
		}
	}
	return p
}

func TestPullWatcherBatchesSixtyAndRefreshesOnlyMovedDetails(t *testing.T) {
	updated := watchFixtureUpdated
	head, status := loopHead, "PENDING"
	calls := fakePullGraphQL(t, func(query string, _ int) map[string]any { return pullGraphQLFixture(t, query, status, head, updated) })
	refs := make([]string, 60)
	for i := range refs {
		refs[i] = fmt.Sprintf("o/r#%d", i+1)
	}
	first, err := loop.FetchLoopPullBatch(t.Context(), refs, nil, 2500)
	if err != nil || len(first) != 60 || *calls != 1 {
		t.Fatalf("cold poll: calls=%d found=%d err=%v", *calls, len(first), err)
	}
	status = loop.CheckSuccess // CI does not update the PR timestamp or any desk event.
	next, err := loop.FetchLoopPullBatch(t.Context(), refs, first, 2500)
	if err != nil || *calls != 2 {
		t.Fatalf("CI-only poll: calls=%d err=%v", *calls, err)
	}
	for _, seen := range next {
		if !seen.Pull.Green || seen.Pending || seen.Pull.Body != "kept detail" || len(seen.Pull.Reviews) != 1 {
			t.Fatalf("lost refreshed checks or cached details: %+v", seen)
		}
	}
	head, updated = loopFixed, "2026-01-01T00:01:00Z"
	next, err = loop.FetchLoopPullBatch(t.Context(), refs, next, 2500)
	if err != nil || *calls != 4 {
		t.Fatalf("moved poll: calls=%d err=%v", *calls, err)
	}
	for _, seen := range next {
		if seen.Pull.Head != loopFixed || seen.Pull.Reviews[0].Commit != loopFixed {
			t.Fatalf("mixed heads: %+v", seen)
		}
	}
}

func TestPullWatcherHourlyRequestsDoNotScaleWithTrackedCount(t *testing.T) {
	for _, size := range []int{1, 60} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			calls := fakePullGraphQL(t, func(query string, _ int) map[string]any {
				return pullGraphQLFixture(t, query, "PENDING", loopHead, watchFixtureUpdated)
			})
			refs := make([]string, size)
			for i := range refs {
				refs[i] = fmt.Sprintf("o/r#%d", i+1)
			}
			var seen map[string]loop.WatchedLoopPull
			for range 120 { // an hour of short intervals, regardless of session count
				var err error
				seen, err = loop.FetchLoopPullBatch(t.Context(), refs, seen, 0)
				if err != nil {
					t.Fatal(err)
				}
			}
			if *calls != 120 {
				t.Fatalf("%d PRs cost %d requests/hour", size, *calls)
			}
		})
	}
}

func TestPullWatcherPassUsesObservationsWhileFetchIsBlocked(t *testing.T) {
	observe, sweep := loop.ReadObservedLoopPull, loop.ReadLoopSweep
	r := newLoopRig(t)
	loop.ReadObservedLoopPull, loop.ReadLoopSweep = observe, sweep
	r.park(t, loopPR, r.a.now)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.Once = true
	w := l.PullWatcher()
	p := r.pull
	p.Green = false
	w.Snapshot = loop.LoopPullSnapshot{Pulls: map[string]loop.WatchedLoopPull{loopPR: {Pull: p, At: r.a.now, Pending: true}}, ListedAt: time.Now()}
	w.Set([]string{loopPR}, nil, nil, 0, nil)
	// Fail if a future pass change restores the old per-PR read path.
	loop.ReadLoopPull = func(context.Context, string) (superloop.Pull, error) {
		t.Error("pass read a tracked PR from GitHub")
		return superloop.Pull{}, errors.New("unexpected read")
	}
	old := loop.ReadLoopPullBatch
	t.Cleanup(func() { loop.ReadLoopPullBatch = old })
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	loop.ReadLoopPullBatch = func(ctx context.Context, _ []string, _ map[string]loop.WatchedLoopPull, _ int) (map[string]loop.WatchedLoopPull, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return map[string]loop.WatchedLoopPull{loopPR: {Pull: p, At: r.a.now, Pending: true}}, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { defer close(done); _, _ = w.Poll(ctx, l) }()
	<-entered
	if err := l.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := l.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	r.a.now = r.a.now.Add(loop.LoopPullMaxAge + time.Second)
	if err := l.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.out.String(), "observation is") || !strings.Contains(w.View().AgeLine(r.a.now), "1 stale") {
		t.Fatalf("stale age missing: %s", r.out)
	}
}

// GitHub leaves the mergeability of some old pull requests unknown. One
// nobody updated for ten minutes no longer keeps the watch at the 30-second
// interval, while one updated a minute ago is still read again there
// (#3769).
func TestPullWatcherStopsWaitingForMergeabilityThatNeverComes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		updated time.Duration
		want    time.Duration
	}{
		{name: "updated a minute ago", updated: time.Minute, want: loop.LoopPullShort},
		{name: "updated long ago", updated: time.Hour, want: loop.LoopPullLong},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLoopRig(t)
			l, err := r.a.newSupervisorLoop(false)
			if err != nil {
				t.Fatal(err)
			}
			w := l.PullWatcher()
			w.Set([]string{loopPR}, nil, nil, 0, nil)
			w.Snapshot.ListedAt = time.Now()
			p := r.pull
			p.Mergeable, p.Updated = "UNKNOWN", time.Now().Add(-tc.updated)
			old := loop.ReadLoopPullBatch
			t.Cleanup(func() { loop.ReadLoopPullBatch = old })
			loop.ReadLoopPullBatch = func(context.Context, []string, map[string]loop.WatchedLoopPull, int) (map[string]loop.WatchedLoopPull, error) {
				return map[string]loop.WatchedLoopPull{loopPR: {Pull: p, At: r.a.now}}, nil
			}
			if next, err := w.Poll(t.Context(), l); err != nil || next != tc.want {
				t.Fatalf("next poll in %s (err %v), want %s", next, err, tc.want)
			}
		})
	}
}

func TestPullWatcherGreenReachesDeliveryWithoutDeskEvent(t *testing.T) {
	observe, sweep := loop.ReadObservedLoopPull, loop.ReadLoopSweep
	r := newLoopRig(t)
	loop.ReadObservedLoopPull, loop.ReadLoopSweep = observe, sweep
	r.park(t, loopPR, r.a.now)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.Once = true
	w := l.PullWatcher()
	w.Set([]string{loopPR}, nil, nil, 0, nil)
	w.Snapshot.ListedAt = time.Now()
	p := r.pull
	p.Green = false
	old := loop.ReadLoopPullBatch
	t.Cleanup(func() { loop.ReadLoopPullBatch = old })
	loop.ReadLoopPullBatch = func(context.Context, []string, map[string]loop.WatchedLoopPull, int) (map[string]loop.WatchedLoopPull, error) {
		return map[string]loop.WatchedLoopPull{loopPR: {Pull: p, At: r.a.now, Pending: !p.Green}}, nil
	}
	if next, err := w.Poll(t.Context(), l); err != nil || next != loop.LoopPullShort {
		t.Fatalf("pending: %s %v", next, err)
	}
	if err := l.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	check := func(want string) {
		t.Helper()
		st, err := r.a.store.Read()
		if err != nil {
			t.Fatal(err)
		}
		v := readDelivery(st, 0, r.a.now)
		for _, d := range v.Deliveries {
			if d.Ref == loopPR {
				if d.Step != want {
					t.Fatalf("delivery step=%s want %s", d.Step, want)
				}
				return
			}
		}
		t.Fatalf("delivery missing: %+v", v)
	}
	check(state.DeliveryCI)
	r.a.now = r.a.now.Add(loop.LoopPullShort)
	p.Green = true
	if next, err := w.Poll(t.Context(), l); err != nil || next != loop.LoopPullLong {
		t.Fatalf("settled: %s %v", next, err)
	}
	// The watcher wakes a pass even when the configured loop interval is long.
	r.a.cfg.Supervisor.LoopInterval.Duration = time.Hour
	if !l.WaitNextPass(t.Context()) {
		t.Fatal("watcher did not wake pass")
	}
	if err := l.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	check(state.DeliveryReviewQueued)
}

func TestPullWatcherFloorAndTerminalRemoval(t *testing.T) {
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	w := l.PullWatcher()
	w.Set([]string{loopPR}, nil, nil, 2500, nil)
	w.Snapshot.ListedAt = time.Now()
	old := loop.ReadLoopPullBatch
	t.Cleanup(func() { loop.ReadLoopPullBatch = old })
	calls := 0
	loop.ReadLoopPullBatch = func(_ context.Context, refs []string, _ map[string]loop.WatchedLoopPull, _ int) (map[string]loop.WatchedLoopPull, error) {
		calls++
		if calls > 1 && len(refs) != 0 {
			t.Fatal("terminal PR queried again")
		}
		p := r.pull
		p.State = superloop.Merged
		return map[string]loop.WatchedLoopPull{loopPR: {Pull: p, At: r.a.now}}, nil
	}
	if _, err := w.Poll(t.Context(), l); !errors.Is(err, loop.ErrLoopLater) || calls != 0 {
		t.Fatalf("unknown floor: calls=%d err=%v", calls, err)
	}
	healthyLoopBudget(t, r)
	if _, err := w.Poll(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Poll(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Read(loopPR, r.a.now.Add(time.Hour)); err != nil {
		t.Fatal("lost final observation", err)
	}
	for n, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute} {
		if got := loop.PullWatchRetry(n + 1); got != want {
			t.Fatalf("retry %d: %s want %s", n, got, want)
		}
	}
}

func TestPullWatcherRateLimitBacksOffAndRecovers(t *testing.T) {
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	w := l.PullWatcher()
	w.Snapshot.ListedAt = time.Now()
	w.Set([]string{loopPR}, nil, nil, 0, nil)
	old := loop.ReadLoopPullBatch
	t.Cleanup(func() { loop.ReadLoopPullBatch = old })
	fail := true
	loop.ReadLoopPullBatch = func(context.Context, []string, map[string]loop.WatchedLoopPull, int) (map[string]loop.WatchedLoopPull, error) {
		if fail {
			return nil, errors.New("rate limit")
		}
		return map[string]loop.WatchedLoopPull{loopPR: {Pull: r.pull, At: r.a.now}}, nil
	}
	schedule := loop.LoopPullSchedule{}
	for i := 1; i <= 8; i++ {
		next, err := w.Poll(t.Context(), l)
		if got := schedule.After(l, next, err); got != loop.PullWatchRetry(i) {
			t.Fatalf("retry %d: %s", i, got)
		}
	}
	if n := strings.Count(r.out.String(), "SUPERVISOR PULL WATCH held: try "); n != 8 {
		t.Fatalf("failed tries said %d times, want each of 8", n)
	}
	fail = false
	next, err := w.Poll(t.Context(), l)
	if err != nil || schedule.After(l, next, err) != loop.LoopPullLong || schedule.Failures != 0 {
		t.Fatalf("did not recover: %v", err)
	}
	if _, err := w.Read(loopPR, r.a.now); err != nil {
		t.Fatal(err)
	}
	fail = true
	next, err = w.Poll(t.Context(), l)
	if schedule.After(l, next, err) != time.Minute {
		t.Fatal("backoff did not reset")
	}
	if !strings.Contains(r.out.String(), "SUPERVISOR PULL WATCH recovered") || !strings.HasSuffix(strings.TrimSpace(r.out.String()), "rate limit; next try in 1m0s") || !strings.Contains(r.out.String(), "held: try 1 failed") {
		t.Fatalf("new failure episode:\n%s", r.out.String())
	}
}

func TestPullWatcherPaginatesBeforeCallingChecksGreen(t *testing.T) {
	calls := fakePullGraphQL(t, func(query string, call int) map[string]any {
		status := loop.CheckSuccess
		if call == 2 {
			status = loop.CheckFailure
		}
		p := pullGraphQLFixture(t, query, status, loopHead, watchFixtureUpdated)
		c := loop.PullConnection(p, loop.PullContexts)
		if call == 1 {
			c[loop.PullPageInfo] = map[string]any{loop.PullHasNext: true, loop.PullEndCursor: "more"}
		}
		if call == 2 && !strings.Contains(query, `contexts(first:100,after:"more")`) {
			t.Fatal("missing check cursor")
		}
		return p
	})
	seen, err := loop.FetchLoopPullBatch(t.Context(), []string{loopPR}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 2 || seen[loopPR].Pull.Green || len(seen[loopPR].Pull.Red) != 1 {
		t.Fatalf("truncated green: calls=%d seen=%+v", *calls, seen)
	}
}

func TestPullWatcherNullReferenceDoesNotDiscardOtherPulls(t *testing.T) {
	fakePullGraphQL(t, func(query string, _ int) map[string]any {
		if strings.Contains(query, "number:8)") {
			return nil
		}
		return pullGraphQLFixture(t, query, loop.CheckSuccess, loopHead, watchFixtureUpdated)
	})
	seen, err := loop.FetchLoopPullBatch(t.Context(), []string{loopPR, multiParkSecondPR}, nil, 0)
	if err != nil || !seen[loopPR].Pull.Green || seen[multiParkSecondPR].Pull.State != "" {
		t.Fatalf("partial lookup: %+v %v", seen, err)
	}
}

func TestPullWatcherHasOneFetchAndCancelsOnStop(t *testing.T) {
	start := loop.StartLoopPullWatcher
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	old := loop.ReadLoopPullBatch
	t.Cleanup(func() { loop.ReadLoopPullBatch = old })
	listed := loop.ReadOpenPulls
	t.Cleanup(func() { loop.ReadOpenPulls = listed })
	loop.ReadOpenPulls = func(context.Context, func() ([]string, error), []string) ([]string, error) {
		return []string{loopPR}, nil
	}
	entered := make(chan struct{})
	loop.ReadLoopPullBatch = func(ctx context.Context, _ []string, _ map[string]loop.WatchedLoopPull, _ int) (map[string]loop.WatchedLoopPull, error) {
		close(entered) // a concurrent second fetch would panic
		<-ctx.Done()
		return nil, ctx.Err()
	}
	stop := start(t.Context(), l)
	<-entered
	l.PullWatcher().Set([]string{loopPR}, nil, nil, 0, nil)
	stop()

	polledOnce := false
	loop.PollOncePullWatcher = func(context.Context, *loop.Loop) { polledOnce = true }
	l.Once = true
	now := r.a.now
	r.a.loopNow = &atomic.Pointer[time.Time]{}
	r.a.loopNow.Store(&now)
	if err := l.Pass(t.Context()); err != nil || !polledOnce {
		t.Fatalf("once pass poll: polled=%v err=%v", polledOnce, err)
	}
}

func TestPullWatcherPreservesSavedListedAtAndPollsDryRunWhenMissing(t *testing.T) {
	observe, sweep := loop.ReadObservedLoopPull, loop.ReadLoopSweep
	r := newLoopRig(t)
	loop.ReadObservedLoopPull, loop.ReadLoopSweep = observe, sweep
	r.a.cfg.Supervisor.Ownership.Authors = []string{loopDesk}
	r.a.cfg.GitHub.AllowedOrgs = []string{"o"}
	r.a.cfg.GitHub.Floor = 2500
	healthyLoopBudget(t, r)
	r.park(t, loopPR, r.a.now)

	listed := r.a.now.Add(-time.Minute)
	saved := loop.LoopPullSnapshot{
		Pulls:    map[string]loop.WatchedLoopPull{loopPR: {Pull: r.pull, At: r.a.now}},
		ListedAt: listed,
	}
	if err := r.a.store.WriteFile(loop.LoopPullWatchFile, saved); err != nil {
		t.Fatal(err)
	}
	polled := 0
	loop.PollOncePullWatcher = func(context.Context, *loop.Loop) { polled++ }
	if err := r.a.dryRunSupervisorLoop(t.Context()); err != nil {
		t.Fatalf("dryRun with fresh saved snapshot: %v", err)
	}
	if polled != 0 {
		t.Fatalf("dryRun polled %d times despite fresh saved snapshot", polled)
	}

	// When the saved snapshot is missing a tracked PR (or absent during upgrade),
	// dry-run polls once in memory without overwriting the saved file.
	if err := r.a.store.WriteFile(loop.LoopPullWatchFile, loop.LoopPullSnapshot{Pulls: map[string]loop.WatchedLoopPull{}}); err != nil {
		t.Fatal(err)
	}
	oldBatch, oldOpen := loop.ReadLoopPullBatch, loop.ReadOpenPulls
	t.Cleanup(func() { loop.ReadLoopPullBatch, loop.ReadOpenPulls = oldBatch, oldOpen })
	loop.ReadOpenPulls = func(context.Context, func() ([]string, error), []string) ([]string, error) {
		return []string{loopPR}, nil
	}
	loop.ReadLoopPullBatch = func(context.Context, []string, map[string]loop.WatchedLoopPull, int) (map[string]loop.WatchedLoopPull, error) {
		return map[string]loop.WatchedLoopPull{loopPR: {Pull: r.pull, At: time.Now()}}, nil
	}
	loop.PollOncePullWatcher = func(ctx context.Context, l *loop.Loop) {
		if _, err := l.PullWatcher().Poll(ctx, l); err != nil {
			t.Fatal(err)
		}
		polled++
	}
	if err := r.a.dryRunSupervisorLoop(t.Context()); err != nil {
		t.Fatalf("dryRun with missing snapshot: %v", err)
	}
	if polled != 1 {
		t.Fatalf("dryRun polled %d times, want 1", polled)
	}
	var disk loop.LoopPullSnapshot
	if ok, err := r.a.store.ReadFile(loop.LoopPullWatchFile, &disk); !ok || err != nil || len(disk.Pulls) != 0 {
		t.Fatalf("dryRun mutated saved snapshot on disk: ok=%v err=%v pulls=%d", ok, err, len(disk.Pulls))
	}
}

// The listing finds the bots' pull requests beside the desk's own, as
// GitHub's search names an app, and keeps only those in repositories the
// desk owns: until the team files are read, only the review list's.
func TestPullWatcherListsTheDesksBotPullRequests(t *testing.T) {
	const (
		botTeam     = "team-a"
		botName     = "renovate[bot]"
		botTool     = "o/tool"
		botOther    = "o/other"
		botPull     = "o/x-configs#4"
		botLogin    = "app/renovate"
		badLogin    = "app/renovat"
		orgWildcard = "o/*"
		sharedRepo  = "shared/one"
		sharedPull  = "shared/one#7"
	)
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	listed, batch := loop.ReadOpenPulls, loop.ReadLoopPullBatch
	t.Cleanup(func() { loop.ReadOpenPulls, loop.ReadLoopPullBatch = listed, batch })
	var searched []string
	var botFails bool
	loop.ReadOpenPulls = func(_ context.Context, authors func() ([]string, error), orgs []string) ([]string, error) {
		logins, err := authors()
		if err != nil {
			return nil, err
		}
		if !slices.Equal(orgs, []string{"o"}) {
			t.Errorf("orgs = %v", orgs)
		}
		searched = append(searched, logins...)
		if botFails && slices.Equal(logins, []string{badLogin}) {
			return nil, errors.New("the listed users cannot be searched")
		}
		if slices.Equal(logins, []string{botLogin}) {
			return []string{idleRef, "o/other#3", botPull, "o/tool#6"}, nil
		}
		return []string{reserveIssue}, nil
	}
	loop.ReadLoopPullBatch = func(context.Context, []string, map[string]loop.WatchedLoopPull, int) (map[string]loop.WatchedLoopPull, error) {
		return map[string]loop.WatchedLoopPull{}, nil
	}
	w := l.PullWatcher()
	w.Set(nil, []string{"me"}, []string{"o"}, 0, []string{botTool})
	desk := superloop.Desk{Orgs: []string{"o"}, Team: botTeam, Review: []string{"o/*-configs"}, Never: []string{botTool}}
	w.SetBots([]string{botName}, desk, nil, false)
	if _, err := w.Poll(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(searched, []string{"me", botLogin}) {
		t.Errorf("searched %v", searched)
	}
	if got, want := w.View().Open, []string{reserveIssue, botPull}; !slices.Equal(got, want) {
		t.Errorf("teams unread: open = %v, want %v", got, want)
	}
	// Once read, the team files admit the team's own repositories: the
	// listing runs again at once.
	w.SetBots([]string{botName}, desk, map[string]string{repo: botTeam, botOther: "team-b", botTool: botTeam}, true)
	if !w.View().ListedAt.IsZero() {
		t.Fatal("team files read without a new listing")
	}
	if _, err := w.Poll(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	if got, want := w.View().Open, []string{reserveIssue, idleRef, botPull}; !slices.Equal(got, want) {
		t.Errorf("teams read: open = %v, want %v", got, want)
	}
	if w.View().Incomplete {
		t.Error("a short bot listing marked the desk's incomplete")
	}
	// One bot's failed search (a login GitHub's search refuses) leaves the
	// desk's own listing and the other bots' whole, and is said once.
	botFails = true
	searched = nil
	w.SetBots([]string{"renovat", botName}, desk, map[string]string{repo: botTeam}, true)
	for range 2 {
		if _, err := w.Poll(t.Context(), l); err != nil {
			t.Fatal(err)
		}
		if v := w.View(); v.ListedAt.IsZero() || v.Incomplete || !slices.Equal(v.Open, []string{reserveIssue, idleRef, botPull}) {
			t.Errorf("a bot's search failed: listed %v, incomplete %v, open %v", v.ListedAt, v.Incomplete, v.Open)
		}
		w.Mu.Lock()
		w.Snapshot.ListedAt = time.Time{}
		w.Mu.Unlock()
	}
	if !slices.Equal(searched, []string{"me", badLogin, botLogin, "me", badLogin, botLogin}) {
		t.Errorf("a bot's search failed: searched %v", searched)
	}
	if n := strings.Count(r.out.String(), "cannot list the pull requests of app/renovat "); n != 1 {
		t.Errorf("a bot's failed search said %d times, want once:\n%s", n, r.out.String())
	}
	botFails = false
	// No bots: one search, the desk's.
	searched = nil
	w.SetBots(nil, desk, nil, true)
	if _, err := w.Poll(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(searched, []string{"me"}) || !slices.Equal(w.View().Open, []string{reserveIssue}) {
		t.Errorf("without bots: searched %v, open %v", searched, w.View().Open)
	}
	// A multi-org desk without a team file that names its repositories in
	// supervisor.ownership.review searches only those scopes for bots, not
	// whole shared orgs.
	var gotScopes [][]string
	loop.ReadOpenPulls = func(_ context.Context, authors func() ([]string, error), scopes []string) ([]string, error) {
		logins, err := authors()
		if err != nil {
			return nil, err
		}
		gotScopes = append(gotScopes, slices.Clone(scopes))
		if slices.Equal(logins, []string{botLogin}) {
			return []string{idleRef, sharedPull, "shared/other#8"}, nil
		}
		return []string{reserveIssue}, nil
	}
	w.Set(nil, []string{"me"}, []string{"o", litShared}, 0, nil)
	scoped := superloop.Desk{Orgs: []string{"o", litShared}, Review: []string{orgWildcard, sharedRepo}}
	w.SetBots([]string{botName}, scoped, nil, false)
	if _, err := w.Poll(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	if len(gotScopes) != 2 || !slices.Equal(gotScopes[0], []string{"o", litShared}) || !slices.Equal(gotScopes[1], []string{"o", sharedRepo}) {
		t.Errorf("scoped bot search scopes = %v", gotScopes)
	}
	if got, want := w.View().Open, []string{reserveIssue, idleRef, sharedPull}; !slices.Equal(got, want) {
		t.Errorf("scoped open = %v, want %v", got, want)
	}
}
