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
		if github.Repo(ctx) != "o/r" || !slices.Equal(args[:2], []string{"api", "graphql"}) {
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
			pulls[i] = unmergedPull{Number: start + i, CreatedAt: now, State: "OPEN"}
		}
		if calls == 2 {
			pulls[99].Body = "Closes https://github.com/o/r/issues/55"
		}
		return inventoryPage(t, pulls, calls == 1, "next"), nil
	}
	pulls, err := readRepoOpenPullsWithGH(context.Background(), run, "o/r")
	if err != nil || len(pulls) != 200 || calls != 2 {
		t.Fatalf("inventory: %d calls=%d err=%v", len(pulls), calls, err)
	}
	if issue, _ := openPullClosingIssue("o/r", pulls, "", []string{"o/r#55"}, nil); issue == "" {
		t.Fatal("tie hid duplicate")
	}
}

func TestInventoryGraphQLFailureNeverMeansEmpty(t *testing.T) {
	for _, raw := range []string{`{"errors":[{"message":"unavailable"}],"data":{"repository":null}}`, `{"data":{"repository":null}}`, `{"data":{"repository":{"pullRequests":null}}}`, `{}`} {
		_, err := readRepoOpenPullsWithGH(context.Background(), func(context.Context, ...string) ([]byte, error) { return []byte(raw), nil }, "o/r")
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
		return []unmergedPull{{Number: 1, State: "OPEN", CreatedAt: r.a.timeNow()}}, nil
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
		if _, _, err := l.repoRecentUnmerged(context.Background(), "o/r"); err != nil {
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
	if _, _, err := l.repoRecentUnmerged(context.Background(), "o/r"); err == nil {
		t.Fatal("failed revision served stale inventory")
	}
	failed = false
	r.a.now = r.a.now.Add(time.Hour)
	if _, _, err := l.repoRecentUnmerged(context.Background(), "o/r"); err != nil {
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
	if _, _, err := l.repoRecentUnmerged(context.Background(), "o/r"); err == nil || calls != 0 {
		t.Fatalf("unbudgeted inventory calls=%d err=%v", calls, err)
	}
}

func TestInventoryRejectsTruncatedClosingReferences(t *testing.T) {
	raw := inventoryPage(t, []unmergedPull{{Number: 1, CreatedAt: time.Now()}}, false, "")
	raw = []byte(strings.ReplaceAll(string(raw), `"hasNextPage":false`, `"hasNextPage":true`))
	_, err := readRepoOpenPullsWithGH(context.Background(), func(context.Context, ...string) ([]byte, error) { return raw, nil }, "o/r")
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
			if github.Repo(ctx) != "o/r" || !slices.Contains(args, "If-None-Match: previous") || !slices.Contains(args, "repos/o/r/pulls?state=all&sort=updated&direction=desc&per_page=1") {
				t.Fatalf("revision route %v", args)
			}
			return []byte(fmt.Sprintf("HTTP/2.0 %d response\r\nETag: next\r\nContent-Length: 2\r\n\r\n{}", status)), nil
		}
		etag, unchanged, err := readRepoPullRevisionWithGH(context.Background(), run, "o/r", "previous")
		if status == 403 {
			if err == nil {
				t.Fatal("refusal accepted")
			}
			continue
		}
		if err != nil || unchanged != (status == 304) {
			t.Fatalf("status %d unchanged=%t err=%v", status, unchanged, err)
		}
		if status == 304 && etag != "previous" || status == 200 && etag != "next" {
			t.Fatalf("wrong validator %s", etag)
		}
	}
}
