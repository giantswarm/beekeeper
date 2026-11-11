package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
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
 "commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"%s","contexts":{"nodes":[
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
	first, err := fetchLoopPullBatch(t.Context(), refs, nil, 2500)
	if err != nil || len(first) != 60 || *calls != 1 {
		t.Fatalf("cold poll: calls=%d found=%d err=%v", *calls, len(first), err)
	}
	status = checkSuccess // CI does not update the PR timestamp or any desk event.
	next, err := fetchLoopPullBatch(t.Context(), refs, first, 2500)
	if err != nil || *calls != 2 {
		t.Fatalf("CI-only poll: calls=%d err=%v", *calls, err)
	}
	for _, seen := range next {
		if !seen.Pull.Green || seen.Pending || seen.Pull.Body != "kept detail" || len(seen.Pull.Reviews) != 1 {
			t.Fatalf("lost refreshed checks or cached details: %+v", seen)
		}
	}
	head, updated = loopFixed, "2026-01-01T00:01:00Z"
	next, err = fetchLoopPullBatch(t.Context(), refs, next, 2500)
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
			var seen map[string]watchedLoopPull
			for range 120 { // an hour of short intervals, regardless of session count
				var err error
				seen, err = fetchLoopPullBatch(t.Context(), refs, seen, 0)
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
	observe, sweep := readObservedLoopPull, readLoopSweep
	r := newLoopRig(t)
	readObservedLoopPull, readLoopSweep = observe, sweep
	r.park(t, loopPR, r.a.now)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.once = true
	w := l.pullWatcher()
	p := r.pull
	p.Green = false
	w.snapshot = loopPullSnapshot{Pulls: map[string]watchedLoopPull{loopPR: {Pull: p, At: r.a.now, Pending: true}}, ListedAt: time.Now()}
	w.set([]string{loopPR}, nil, nil, 0, nil)
	// Fail if a future pass change restores the old per-PR read path.
	readLoopPull = func(context.Context, string) (superloop.Pull, error) {
		t.Error("pass read a tracked PR from GitHub")
		return superloop.Pull{}, errors.New("unexpected read")
	}
	old := readLoopPullBatch
	t.Cleanup(func() { readLoopPullBatch = old })
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	readLoopPullBatch = func(ctx context.Context, _ []string, _ map[string]watchedLoopPull, _ int) (map[string]watchedLoopPull, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return map[string]watchedLoopPull{loopPR: {Pull: p, At: r.a.now, Pending: true}}, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { defer close(done); _, _ = w.poll(ctx, l) }()
	<-entered
	if err := l.pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := l.pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	r.a.now = r.a.now.Add(loopPullMaxAge + time.Second)
	if err := l.pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.out.String(), "observation is") || !strings.Contains(w.view().ageLine(r.a.now), "1 stale") {
		t.Fatalf("stale age missing: %s", r.out)
	}
}

func TestPullWatcherGreenReachesDeliveryWithoutDeskEvent(t *testing.T) {
	observe, sweep := readObservedLoopPull, readLoopSweep
	r := newLoopRig(t)
	readObservedLoopPull, readLoopSweep = observe, sweep
	r.park(t, loopPR, r.a.now)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.once = true
	w := l.pullWatcher()
	w.set([]string{loopPR}, nil, nil, 0, nil)
	w.snapshot.ListedAt = time.Now()
	p := r.pull
	p.Green = false
	old := readLoopPullBatch
	t.Cleanup(func() { readLoopPullBatch = old })
	readLoopPullBatch = func(context.Context, []string, map[string]watchedLoopPull, int) (map[string]watchedLoopPull, error) {
		return map[string]watchedLoopPull{loopPR: {Pull: p, At: r.a.now, Pending: !p.Green}}, nil
	}
	if next, err := w.poll(t.Context(), l); err != nil || next != loopPullShort {
		t.Fatalf("pending: %s %v", next, err)
	}
	if err := l.pass(t.Context()); err != nil {
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
	r.a.now = r.a.now.Add(loopPullShort)
	p.Green = true
	if next, err := w.poll(t.Context(), l); err != nil || next != loopPullLong {
		t.Fatalf("settled: %s %v", next, err)
	}
	// The watcher wakes a pass even when the configured loop interval is long.
	r.a.cfg.Supervisor.LoopInterval.Duration = time.Hour
	if !l.waitNextPass(t.Context()) {
		t.Fatal("watcher did not wake pass")
	}
	if err := l.pass(t.Context()); err != nil {
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
	w := l.pullWatcher()
	w.set([]string{loopPR}, nil, nil, 2500, nil)
	w.snapshot.ListedAt = time.Now()
	old := readLoopPullBatch
	t.Cleanup(func() { readLoopPullBatch = old })
	calls := 0
	readLoopPullBatch = func(_ context.Context, refs []string, _ map[string]watchedLoopPull, _ int) (map[string]watchedLoopPull, error) {
		calls++
		if calls > 1 && len(refs) != 0 {
			t.Fatal("terminal PR queried again")
		}
		p := r.pull
		p.State = superloop.Merged
		return map[string]watchedLoopPull{loopPR: {Pull: p, At: r.a.now}}, nil
	}
	if _, err := w.poll(t.Context(), l); !errors.Is(err, errLoopLater) || calls != 0 {
		t.Fatalf("unknown floor: calls=%d err=%v", calls, err)
	}
	healthyLoopBudget(t, r)
	if _, err := w.poll(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	if _, err := w.poll(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	if _, err := w.read(loopPR, r.a.now.Add(time.Hour)); err != nil {
		t.Fatal("lost final observation", err)
	}
	for n, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute} {
		if got := pullWatchRetry(n + 1); got != want {
			t.Fatalf("retry %d: %s want %s", n, got, want)
		}
	}
}

func TestPullWatcherRateLimitLogsOnceAndRecovers(t *testing.T) {
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	w := l.pullWatcher()
	w.snapshot.ListedAt = time.Now()
	w.set([]string{loopPR}, nil, nil, 0, nil)
	old := readLoopPullBatch
	t.Cleanup(func() { readLoopPullBatch = old })
	fail := true
	readLoopPullBatch = func(context.Context, []string, map[string]watchedLoopPull, int) (map[string]watchedLoopPull, error) {
		if fail {
			return nil, errors.New("rate limit")
		}
		return map[string]watchedLoopPull{loopPR: {Pull: r.pull, At: r.a.now}}, nil
	}
	schedule := loopPullSchedule{}
	for i := 1; i <= 8; i++ {
		next, err := w.poll(t.Context(), l)
		if got := schedule.after(l, next, err); got != pullWatchRetry(i) {
			t.Fatalf("retry %d: %s", i, got)
		}
	}
	if n := strings.Count(r.out.String(), "SUPERVISOR PULL WATCH held"); n != 1 {
		t.Fatalf("failure logged %d times", n)
	}
	fail = false
	next, err := w.poll(t.Context(), l)
	if err != nil || schedule.after(l, next, err) != loopPullLong || schedule.failures != 0 {
		t.Fatalf("did not recover: %v", err)
	}
	if _, err := w.read(loopPR, r.a.now); err != nil {
		t.Fatal(err)
	}
	fail = true
	next, err = w.poll(t.Context(), l)
	if schedule.after(l, next, err) != time.Minute {
		t.Fatal("backoff did not reset")
	}
	if n := strings.Count(r.out.String(), "SUPERVISOR PULL WATCH held"); n != 2 {
		t.Fatalf("new failure episode logged %d times", n)
	}
}

func TestPullWatcherPaginatesBeforeCallingChecksGreen(t *testing.T) {
	calls := fakePullGraphQL(t, func(query string, call int) map[string]any {
		status := checkSuccess
		if call == 2 {
			status = checkFailure
		}
		p := pullGraphQLFixture(t, query, status, loopHead, watchFixtureUpdated)
		c := pullConnection(p, pullContexts)
		if call == 1 {
			c[pullPageInfo] = map[string]any{pullHasNext: true, pullEndCursor: "more"}
		}
		if call == 2 && !strings.Contains(query, `contexts(first:100,after:"more")`) {
			t.Fatal("missing check cursor")
		}
		return p
	})
	seen, err := fetchLoopPullBatch(t.Context(), []string{loopPR}, nil, 0)
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
		return pullGraphQLFixture(t, query, checkSuccess, loopHead, watchFixtureUpdated)
	})
	seen, err := fetchLoopPullBatch(t.Context(), []string{loopPR, multiParkSecondPR}, nil, 0)
	if err != nil || !seen[loopPR].Pull.Green || seen[multiParkSecondPR].Pull.State != "" {
		t.Fatalf("partial lookup: %+v %v", seen, err)
	}
}

func TestPullWatcherHasOneFetchAndCancelsOnStop(t *testing.T) {
	start := startLoopPullWatcher
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	old := readLoopPullBatch
	t.Cleanup(func() { readLoopPullBatch = old })
	entered := make(chan struct{})
	readLoopPullBatch = func(ctx context.Context, _ []string, _ map[string]watchedLoopPull, _ int) (map[string]watchedLoopPull, error) {
		close(entered) // a concurrent second fetch would panic
		<-ctx.Done()
		return nil, ctx.Err()
	}
	stop := start(t.Context(), l)
	<-entered
	l.pullWatcher().set([]string{loopPR}, nil, nil, 0, nil)
	stop()

	polledOnce := false
	pollOncePullWatcher = func(context.Context, *supervisorLoop) { polledOnce = true }
	l.once = true
	now := r.a.now
	r.a.loopNow = &atomic.Pointer[time.Time]{}
	r.a.loopNow.Store(&now)
	if err := l.pass(t.Context()); err != nil || !polledOnce {
		t.Fatalf("once pass poll: polled=%v err=%v", polledOnce, err)
	}
}

func TestPullWatcherPreservesSavedListedAtAndPollsDryRunWhenMissing(t *testing.T) {
	observe, sweep := readObservedLoopPull, readLoopSweep
	r := newLoopRig(t)
	readObservedLoopPull, readLoopSweep = observe, sweep
	r.a.cfg.Supervisor.Ownership.Authors = []string{loopDesk}
	r.a.cfg.GitHub.AllowedOrgs = []string{"o"}
	r.a.cfg.GitHub.Floor = 2500
	healthyLoopBudget(t, r)
	r.park(t, loopPR, r.a.now)

	listed := r.a.now.Add(-time.Minute)
	saved := loopPullSnapshot{
		Pulls:    map[string]watchedLoopPull{loopPR: {Pull: r.pull, At: r.a.now}},
		ListedAt: listed,
	}
	if err := r.a.store.WriteFile(loopPullWatchFile, saved); err != nil {
		t.Fatal(err)
	}
	polled := 0
	pollOncePullWatcher = func(context.Context, *supervisorLoop) { polled++ }
	if err := r.a.dryRunSupervisorLoop(t.Context()); err != nil {
		t.Fatalf("dryRun with fresh saved snapshot: %v", err)
	}
	if polled != 0 {
		t.Fatalf("dryRun polled %d times despite fresh saved snapshot", polled)
	}

	// When the saved snapshot is missing a tracked PR (or absent during upgrade),
	// dry-run polls once in memory without overwriting the saved file.
	if err := r.a.store.WriteFile(loopPullWatchFile, loopPullSnapshot{Pulls: map[string]watchedLoopPull{}}); err != nil {
		t.Fatal(err)
	}
	oldBatch, oldOpen := readLoopPullBatch, readOpenPulls
	t.Cleanup(func() { readLoopPullBatch, readOpenPulls = oldBatch, oldOpen })
	readOpenPulls = func(context.Context, func() ([]string, error), []string) ([]string, error) {
		return []string{loopPR}, nil
	}
	readLoopPullBatch = func(context.Context, []string, map[string]watchedLoopPull, int) (map[string]watchedLoopPull, error) {
		return map[string]watchedLoopPull{loopPR: {Pull: r.pull, At: time.Now()}}, nil
	}
	pollOncePullWatcher = func(ctx context.Context, l *supervisorLoop) {
		if _, err := l.pullWatcher().poll(ctx, l); err != nil {
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
	var disk loopPullSnapshot
	if ok, err := r.a.store.ReadFile(loopPullWatchFile, &disk); !ok || err != nil || len(disk.Pulls) != 0 {
		t.Fatalf("dryRun mutated saved snapshot on disk: ok=%v err=%v pulls=%d", ok, err, len(disk.Pulls))
	}
}
