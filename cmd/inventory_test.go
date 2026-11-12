package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// Model GraphQL's nested connection shape, including linked closing issues.
func inventoryPage(t *testing.T, pulls []unmergedPull, more bool, cursor string) []byte {
	t.Helper()
	nodes := []map[string]any{}
	for _, p := range pulls {
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		var node map[string]any
		if err := json.Unmarshal(raw, &node); err != nil {
			t.Fatal(err)
		}
		node["closingIssuesReferences"] = map[string]any{"nodes": p.Closing, "pageInfo": map[string]any{"hasNextPage": false}}
		nodes = append(nodes, node)
	}
	raw, err := json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequests": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": more, "endCursor": cursor}}}}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestInventoryCursorDoesNotSkipTimestampTies(t *testing.T) {
	now := time.Now()
	calls := 0
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		if github.Repo(ctx) != repo || !slices.Equal(args[:2], []string{workerGitHubAPIGroup, inventoryGraphQLCommand}) {
			t.Fatalf("routing: %v %s", args, github.Repo(ctx))
		}
		calls++
		start := 1
		if calls == 2 {
			start = 101
			if !slices.Contains(args, "after=next") {
				t.Fatal("cursor missing")
			}
		}
		pulls := make([]unmergedPull, 100)
		for i := range pulls {
			pulls[i] = unmergedPull{Number: start + i, CreatedAt: now, State: emergencyOpenState}
		}
		if calls == 2 {
			pulls[99].Body = "Closes https://github.com/o/r/issues/55"
		}
		return inventoryPage(t, pulls, calls == 1, DeliveryNextCommand), nil
	}
	pulls, err := readRepoOpenPullsWithGH(context.Background(), run, repo)
	if err != nil || len(pulls) != 200 || calls != 2 {
		t.Fatalf("inventory: %d calls=%d err=%v", len(pulls), calls, err)
	}
	if issue, _ := openPullClosingIssue(repo, pulls, "", []string{"o/r#55"}, nil); issue == "" {
		t.Fatal("tie hid duplicate")
	}
}

func TestInventoryGraphQLFailureNeverMeansEmpty(t *testing.T) {
	for _, raw := range []string{`{"errors":[{"message":"unavailable"}],"data":{"repository":null}}`, `{"data":{"repository":null}}`, `{"data":{"repository":{"pullRequests":null}}}`, `{}`} {
		_, err := readRepoOpenPullsWithGH(context.Background(), func(context.Context, ...string) ([]byte, error) { return []byte(raw), nil }, repo)
		if err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestInventoryConditionalReuseAndFailure(t *testing.T) {
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	readRepoOpenPulls = func(context.Context, string) ([]unmergedPull, error) {
		reads++
		return []unmergedPull{{Number: 1, State: emergencyOpenState, CreatedAt: r.a.timeNow()}}, nil
	}
	revision := 0
	failed := false
	ReadRepoPullRevision = func(_ context.Context, repo, etag string) (string, bool, error) {
		revision++
		if failed {
			return "", false, errors.New("unavailable")
		}
		return `"v1"`, etag == `"v1"`, nil
	}
	for i := range 3 {
		if _, _, err := l.RepoRecentUnmerged(context.Background(), repo); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if _, _, err := l.RepoRecentUnmerged(context.Background(), "O/R"); err != nil {
				t.Fatal(err)
			}
		}
		r.a.now = r.a.now.Add(UnmergedCacheTTL)
	}
	if reads != 1 || revision != 3 {
		t.Fatalf("unchanged inventory: reads=%d probes=%d", reads, revision)
	}
	failed = true
	if _, _, err := l.RepoRecentUnmerged(context.Background(), repo); err == nil {
		t.Fatal("failed revision served stale inventory")
	}
	failed = false
	r.a.now = r.a.now.Add(time.Hour)
	if _, _, err := l.RepoRecentUnmerged(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	if reads != 2 {
		t.Fatal("full refresh missing")
	}
}

func TestInventoryHonorsLoopBudget(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.GitHub.Floor = 2500
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	ReadRepoPullRevision = func(context.Context, string, string) (string, bool, error) { calls++; return "", false, nil }
	if _, _, err := l.RepoRecentUnmerged(context.Background(), repo); err == nil || calls != 0 {
		t.Fatalf("unbudgeted inventory calls=%d err=%v", calls, err)
	}
}

func TestInventoryRejectsTruncatedClosingReferences(t *testing.T) {
	raw := inventoryPage(t, []unmergedPull{{Number: 1, CreatedAt: time.Now()}}, false, "")
	raw = []byte(strings.ReplaceAll(string(raw), `"hasNextPage":false`, `"hasNextPage":true`))
	_, err := readRepoOpenPullsWithGH(context.Background(), func(context.Context, ...string) ([]byte, error) { return raw, nil }, repo)
	if err == nil {
		t.Fatal("truncated linked issues accepted")
	}
}

func TestLoopRESTAllowancePersistsAndLeavesFloor(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.GitHub.Floor = 2500
	now := r.a.timeNow()
	b := github.Budget{Remaining: 4000, Reset: now.Add(time.Hour)}
	spend, err := r.a.reserveLoopREST(b, "inventory")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.a.settleLoopREST(b, b, spend, "inventory"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.a.store.ReadFile(loopRESTFile, &spend); err != nil || spend.Spent != 0 {
		t.Fatalf("conditional response not refunded: %+v %v", spend, err)
	}
	// Above loopRESTLimit while the window has room: with 4000 remaining
	// over the floor 2500, the loop holds once its spend passes 1400, half of
	// the window above the floor besides the others' spend.
	spend.Spent = loopRESTLimit
	if err := r.a.store.WriteFile(loopRESTFile, spend); err != nil {
		t.Fatal(err)
	}
	if got, err := r.a.reserveLoopREST(b, "sweep"); err != nil || got.Spent != loopRESTLimit+loopRESTReserve {
		t.Fatalf("allowance capped while the window has room: %+v %v", got, err)
	}
	spend.Spent = 1450
	if err := r.a.store.WriteFile(loopRESTFile, spend); err != nil {
		t.Fatal(err)
	}
	if _, err := r.a.reserveLoopREST(b, "sweep"); err == nil {
		t.Fatal("scan exceeded its allowance")
	}
	// A GraphQL reset cannot renew an exhausted REST allowance.
	if _, err := r.a.reserveLoopREST(b, "pull"); err == nil {
		t.Fatal("restart renewed REST allowance")
	}
	b.Reset = b.Reset.Add(time.Hour)
	if _, err := r.a.reserveLoopREST(b, "inventory"); err != nil {
		t.Fatal(err)
	}
	b.Remaining = 2500
	if _, err := r.a.reserveLoopREST(b, "inventory"); err == nil {
		t.Fatal("scan spent worker floor")
	}
}

func TestPullRevisionUsesConditionalAllStateUpdates(t *testing.T) {
	for _, status := range []int{200, 304, 403} {
		run := func(ctx context.Context, args ...string) ([]byte, error) {
			if github.Repo(ctx) != repo || !slices.Contains(args, "If-None-Match: inventory-etag-old") || !slices.Contains(args, "repos/o/r/pulls?state=all&sort=updated&direction=desc&per_page=1") {
				t.Fatalf("revision route %v", args)
			}
			var runErr error
			if status != 200 {
				runErr = fmt.Errorf("exit status 1: gh: HTTP %d", status)
			}
			return []byte(fmt.Sprintf("HTTP/2.0 %d response\r\nETag: next\r\nContent-Length: 2\r\n\r\n{}", status)), runErr
		}
		etag, unchanged, err := ReadRepoPullRevisionWithGH(context.Background(), run, repo, "inventory-etag-old")
		if status == 403 {
			if err == nil {
				t.Fatal("refusal accepted")
			}
			continue
		}
		if err != nil || unchanged != (status == 304) {
			t.Fatalf("status %d unchanged=%t err=%v", status, unchanged, err)
		}
		if status == 304 && etag != "inventory-etag-old" || status == 200 && etag != DeliveryNextCommand {
			t.Fatalf("wrong validator %s", etag)
		}
	}
}

func TestOrphanDetailsRespectRESTAllowanceWhileTrackedPullsContinue(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.GitHub.Floor = 2500
	healthyLoopBudget(t, r)
	reset := r.a.now.Add(time.Hour)
	readLoopGraphQLBudget = func(*app, context.Context) (github.Budget, error) {
		return github.Budget{Remaining: 4900, Reset: reset,
			GraphQL: &github.GraphQL{Remaining: 4900, Limit: 5000, Reset: reset}}, nil
	}
	if err := r.a.store.WriteFile(loopRESTFile, loopRESTSpend{Reset: reset, Spent: loopRESTLimit}); err != nil {
		t.Fatal(err)
	}
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	ReadLoopPull = func(context.Context, string) (superloop.Pull, error) {
		reads++
		return r.pull, nil
	}
	if _, err := l.ObservePullStep(context.Background(), "orphan", r.a.now, "sweep"); !errors.Is(err, ErrLoopLater) || reads != 0 {
		t.Fatalf("orphan bypassed exhausted REST allowance: reads=%d err=%v", reads, err)
	}
	if _, err := l.ObservePull(context.Background(), "tracked", r.a.now); err != nil || reads != 1 {
		t.Fatalf("scan hold blocked tracked pull: reads=%d err=%v", reads, err)
	}
}

func TestLoopScanAllowanceSurvivesDisabledFloor(t *testing.T) {
	for _, floor := range []int{-1, 0} {
		t.Run(fmt.Sprint(floor), func(t *testing.T) {
			r := newLoopRig(t)
			r.a.cfg.GitHub.Floor = floor
			reset := r.a.timeNow().Add(time.Hour)
			remaining := 4900
			readLoopGraphQLBudget = func(_ *app, ctx context.Context) (github.Budget, error) {
				if fresh, _ := ctx.Value(freshLoopRESTKey{}).(bool); !fresh {
					t.Fatal("scan used a cached REST sample")
				}
				// A disabled GraphQL gate must not disable REST accounting.
				return github.Budget{Remaining: remaining, Reset: reset}, nil
			}
			runs := 0
			for runs <= remaining/loopRESTReserve {
				err := r.a.loopGitHub(context.Background(), "inventory", func() error {
					runs++
					remaining -= loopRESTReserve
					return nil
				})
				if errors.Is(err, ErrLoopLater) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			// The loop's share is half the window, above loopRESTLimit while
			// nothing else spends (#3724), and no more (#3465).
			if spent := runs * loopRESTReserve; spent != 2450 {
				t.Fatalf("loop scans spent %d of the window's 4900, want 2450", spent)
			}
			scans := runs
			// A restarted caller and a different scan type share the ledger.
			restarted := *r.a
			if err := restarted.loopGitHub(context.Background(), LoopSweepStep, func() error {
				t.Fatal("orphan scan bypassed exhausted allowance")
				return nil
			}); !errors.Is(err, ErrLoopLater) {
				t.Fatalf("exhausted scan: %v", err)
			}
			if err := restarted.loopGitHub(context.Background(), LoopPullStep, func() error {
				runs++
				return nil
			}); err != nil || runs != scans+1 {
				t.Fatalf("emergency tracked read blocked: runs=%d err=%v", runs, err)
			}
		})
	}
}

func TestLoopScanDisabledFloorRetainsUnknownSpend(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.GitHub.Floor = -1
	reset := r.a.timeNow().Add(time.Hour)
	probes := 0
	readLoopGraphQLBudget = func(*app, context.Context) (github.Budget, error) {
		probes++
		if probes > 1 {
			return github.Budget{}, errors.New("unavailable")
		}
		return github.Budget{Remaining: 4900, Reset: reset}, nil
	}
	if err := r.a.loopGitHub(context.Background(), LoopSweepStep, func() error { return nil }); !errors.Is(err, ErrLoopLater) {
		t.Fatalf("unknown spend accepted: %v", err)
	}
	var spend loopRESTSpend
	if _, err := r.a.store.ReadFile(loopRESTFile, &spend); err != nil || spend.Spent != loopRESTReserve {
		t.Fatalf("reservation lost: %+v %v", spend, err)
	}
	if err := r.a.loopGitHub(context.Background(), "inventory", func() error {
		t.Fatal("unreadable budget admitted a scan")
		return nil
	}); !errors.Is(err, ErrLoopLater) {
		t.Fatalf("unreadable budget: %v", err)
	}
}

func TestExhaustedRESTAllowanceSkipsGitHubProbeAndCachePersistsAcrossReexec(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.GitHub.Floor = -1
	reset := r.a.timeNow().Add(time.Hour)
	if err := r.a.store.WriteFile(loopRESTFile, loopRESTSpend{Reset: reset, Spent: 2500, Remaining: 2300}); err != nil {
		t.Fatal(err)
	}
	probes := 0
	readLoopGraphQLBudget = func(*app, context.Context) (github.Budget, error) {
		probes++
		return github.Budget{Remaining: 2300, Reset: reset}, nil
	}
	if err := r.a.loopScanREST(context.Background(), "inventory", func() error {
		t.Fatal("exhausted loopScanREST ran")
		return nil
	}); !errors.Is(err, ErrLoopLater) || probes != 0 {
		t.Fatalf("loopScanREST probed GitHub while held: probes=%d err=%v", probes, err)
	}
	if err := r.a.loopGitHub(context.Background(), "inventory", func() error {
		t.Fatal("exhausted inventory scan ran")
		return nil
	}); !errors.Is(err, ErrLoopLater) || probes != 0 {
		t.Fatalf("loopGitHub scan probed GitHub while REST held: probes=%d err=%v", probes, err)
	}

	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	now := r.a.timeNow()
	const cacheLogin = "fixture-user"
	const cacheTeam = "team-a"
	const cacheETag = `"etag-7"`
	l.Login = cacheLogin
	l.Teams = map[string]string{cacheTeam: cacheLogin}
	l.TeamsAt = now
	l.TidyAt = now
	l.OpenPulls = []string{loopPR}
	l.OpenPullsAt = now
	l.Unmerged = map[string]LoopUnmergedObservation{
		repo: {Pulls: []unmergedPull{{Number: 7, State: emergencyOpenState, CreatedAt: now}}, Etag: cacheETag, At: now, FullAt: now},
	}
	l.UnmergedCursor = repo
	l.DefaultBranches = map[string]string{repo: mainBranch}
	l.CachePull(loopPR, superloop.Pull{Ref: loopPR, State: superloop.Merged})
	if obs := l.Unmerged[repo]; obs.Etag != cacheETag || len(obs.Pulls) != 0 || !obs.At.IsZero() {
		t.Fatalf("cachePull did not preserve ETag while dropping merged PR: %+v", obs)
	}
	l.SaveCache()

	restarted, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	restarted.LoadCache()
	if restarted.Login != cacheLogin || restarted.Teams[cacheTeam] != cacheLogin || restarted.DefaultBranches[repo] != mainBranch || restarted.Unmerged[repo].Etag != cacheETag || restarted.UnmergedCursor != repo {
		t.Fatalf("restarted loop lost persisted cache: login=%q teams=%v branches=%v unmerged=%+v", restarted.Login, restarted.Teams, restarted.DefaultBranches, restarted.Unmerged)
	}
}

// The loop's own allowance holds no scan while the window has room: 991
// spent with 4523 remaining under a disabled floor (#3724).
func TestLoopRESTAllowanceFollowsWhatRemains(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.GitHub.Floor = -1
	reset := r.a.timeNow().Add(30 * time.Minute)
	if err := r.a.store.WriteFile(loopRESTFile, loopRESTSpend{Reset: reset, Spent: 991, Remaining: 4523}); err != nil {
		t.Fatal(err)
	}
	if err := r.a.loopRESTHeld(r.a.timeNow()); err != nil {
		t.Fatalf("held with room in the window: %v", err)
	}
	if _, err := r.a.reserveLoopREST(github.Budget{Remaining: 4523, Reset: reset}, "inventory"); err != nil {
		t.Fatalf("reservation refused with room in the window: %v", err)
	}
}
