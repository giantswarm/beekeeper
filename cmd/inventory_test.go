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
		return inventoryPage(t, pulls, calls == 1, deliveryNextCommand), nil
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
	readRepoPullRevision = func(_ context.Context, repo, etag string) (string, bool, error) {
		revision++
		if failed {
			return "", false, errors.New("unavailable")
		}
		return `"v1"`, etag == `"v1"`, nil
	}
	for i := range 3 {
		if _, _, err := l.repoRecentUnmerged(context.Background(), repo); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if _, _, err := l.repoRecentUnmerged(context.Background(), "O/R"); err != nil {
				t.Fatal(err)
			}
		}
		r.a.now = r.a.now.Add(unmergedCacheTTL)
	}
	if reads != 1 || revision != 3 {
		t.Fatalf("unchanged inventory: reads=%d probes=%d", reads, revision)
	}
	failed = true
	if _, _, err := l.repoRecentUnmerged(context.Background(), repo); err == nil {
		t.Fatal("failed revision served stale inventory")
	}
	failed = false
	r.a.now = r.a.now.Add(time.Hour)
	if _, _, err := l.repoRecentUnmerged(context.Background(), repo); err != nil {
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
	readRepoPullRevision = func(context.Context, string, string) (string, bool, error) { calls++; return "", false, nil }
	if _, _, err := l.repoRecentUnmerged(context.Background(), repo); err == nil || calls != 0 {
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
	spend.Spent = loopRESTLimit
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
			return []byte(fmt.Sprintf("HTTP/2.0 %d response\r\nETag: next\r\nContent-Length: 2\r\n\r\n{}", status)), nil
		}
		etag, unchanged, err := readRepoPullRevisionWithGH(context.Background(), run, repo, "inventory-etag-old")
		if status == 403 {
			if err == nil {
				t.Fatal("refusal accepted")
			}
			continue
		}
		if err != nil || unchanged != (status == 304) {
			t.Fatalf("status %d unchanged=%t err=%v", status, unchanged, err)
		}
		if status == 304 && etag != "inventory-etag-old" || status == 200 && etag != deliveryNextCommand {
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
	readLoopPull = func(context.Context, string) (superloop.Pull, error) {
		reads++
		return r.pull, nil
	}
	if _, err := l.observePullStep(context.Background(), "orphan", r.a.now, "sweep"); !errors.Is(err, errLoopLater) || reads != 0 {
		t.Fatalf("orphan bypassed exhausted REST allowance: reads=%d err=%v", reads, err)
	}
	if _, err := l.observePull(context.Background(), "tracked", r.a.now); err != nil || reads != 1 {
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
			for range loopRESTLimit / loopRESTReserve {
				if err := r.a.loopGitHub(context.Background(), "inventory", func() error {
					runs++
					remaining -= loopRESTReserve
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			// A restarted caller and a different scan type share the ledger.
			restarted := *r.a
			if err := restarted.loopGitHub(context.Background(), loopSweepStep, func() error {
				t.Fatal("orphan scan bypassed exhausted allowance")
				return nil
			}); !errors.Is(err, errLoopLater) {
				t.Fatalf("exhausted scan: %v", err)
			}
			if err := restarted.loopGitHub(context.Background(), loopPullStep, func() error {
				runs++
				return nil
			}); err != nil || runs != loopRESTLimit/loopRESTReserve+1 {
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
	if err := r.a.loopGitHub(context.Background(), loopSweepStep, func() error { return nil }); !errors.Is(err, errLoopLater) {
		t.Fatalf("unknown spend accepted: %v", err)
	}
	var spend loopRESTSpend
	if _, err := r.a.store.ReadFile(loopRESTFile, &spend); err != nil || spend.Spent != loopRESTReserve {
		t.Fatalf("reservation lost: %+v %v", spend, err)
	}
	if err := r.a.loopGitHub(context.Background(), "inventory", func() error {
		t.Fatal("unreadable budget admitted a scan")
		return nil
	}); !errors.Is(err, errLoopLater) {
		t.Fatalf("unreadable budget: %v", err)
	}
}
