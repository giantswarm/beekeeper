package cmd

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

func TestBoardNextSkipsRepositoryWithTenUnmergedPullsInSevenDays(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	oldRead := readRepoOpenPulls
	t.Cleanup(func() { readRepoOpenPulls = oldRead })

	readRepoOpenPulls = func(_ context.Context, repo string) ([]unmergedPull, error) {
		switch repo {
		case "o/clogged":
			pulls := []unmergedPull{
				{Number: 90, URL: "https://github.com/o/clogged/pull/90", CreatedAt: now.Add(-10 * 24 * time.Hour)},
				{Number: 91, URL: "https://github.com/o/clogged/pull/91", CreatedAt: now.Add(-2 * time.Hour), IsDraft: true},
			}
			for i := 1; i <= 10; i++ {
				pulls = append(pulls, unmergedPull{
					Number:    i,
					URL:       fmt.Sprintf("https://github.com/o/clogged/pull/%d", i),
					CreatedAt: now.Add(-time.Duration(11-i) * 12 * time.Hour),
				})
			}
			return pulls, nil
		case "o/clear":
			return []unmergedPull{
				{Number: 1, URL: "https://github.com/o/clear/pull/1", CreatedAt: now.Add(-24 * time.Hour)},
			}, nil
		default:
			return nil, fmt.Errorf("unexpected repo %s", repo)
		}
	}

	cands := []board.Candidate{
		{Item: board.Item{Ref: "o/clogged#1"}, Step: stepUpNext},
		{Item: board.Item{Ref: "o/clogged#2"}, Step: stepUpNext},
		{Item: board.Item{Ref: "o/clear#1"}, Step: stepUpNext},
	}
	st := &state.State{}
	sc := pickScope{me: state.Party{Session: "me"}, alive: func(state.Party) bool { return false }, listed: now}
	a := &app{cfg: &config.Config{}, now: now, out: &bytes.Buffer{}}

	a.skipUnmergedBacklog(context.Background(), &bytes.Buffer{}, cands, func() *board.Candidate {
		return nextFree(st, cands, sc).Pick
	})
	res := nextFree(st, cands, sc)
	if res.Pick == nil || res.Pick.Ref != "o/clear#1" {
		t.Fatalf("pick = %+v, want o/clear#1", res.Pick)
	}
	if len(res.Skipped) != 2 {
		t.Fatalf("skipped = %+v, want 2 clogged items", res.Skipped)
	}
	wantReason := "held: o/clogged has 10 unmerged pull requests opened in the last 7d (limit 10: https://github.com/o/clogged/pull/1 .. https://github.com/o/clogged/pull/10)"
	for _, c := range res.Skipped {
		if c.Skip != wantReason {
			t.Fatalf("skip reason for %s = %q, want %q", c.Ref, c.Skip, wantReason)
		}
		if skipKind(c.Skip) != kindBlocked {
			t.Fatalf("skipKind(%q) = %q, want %q", c.Skip, skipKind(c.Skip), kindBlocked)
		}
	}
}

func TestSupervisorLoopHoldsBackNewStartsAndEscalatesUnmergedBacklogWhileReviewsContinue(t *testing.T) {
	r := newLoopRig(t)
	now := r.a.timeNow()

	cloggedPulls := make([]unmergedPull, 0, 10)
	for i := 1; i <= 10; i++ {
		cloggedPulls = append(cloggedPulls, unmergedPull{
			Number:    i,
			URL:       fmt.Sprintf("https://github.com/o/r/pull/%d", i),
			CreatedAt: now.Add(-time.Duration(11-i) * 6 * time.Hour),
		})
	}
	openPulls := cloggedPulls
	readRepoOpenPulls = func(_ context.Context, targetRepo string) ([]unmergedPull, error) {
		if strings.EqualFold(targetRepo, repo) {
			return openPulls, nil
		}
		return nil, nil
	}

	var starts []string
	r.answer = func(args []string) (string, int) {
		if len(args) >= 3 && args[0] == agentsName && args[1] == agentStartName {
			starts = append(starts, args[2])
		}
		return "", 0
	}

	// Park a worker on o/r#7 so the loop reviews existing work while o/r is clogged.
	r.park(t, loopPR, r.a.now)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.once = true
	if err := l.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Existing PR review still starts on o/r#7 despite the unmerged backlog.
	if !slices.Contains(starts, loopReviewer) {
		t.Fatalf("expected reviewer %q to start on existing PR while clogged, got starts %v", loopReviewer, starts)
	}

	// Starting a new board item in o/r is held back and escalated once to the guide.
	const (
		holdRef = "o/r#42"
		holdURL = "https://github.com/o/r/issues/42"
	)
	act := superloop.Action{Key: "next " + holdRef, Kind: superloop.KindNext, Ref: holdRef, URL: holdURL}
	if _, err := l.startItem(context.Background(), act, loopBoardItem{Ref: holdRef, URL: holdURL}); err != errLoopLater {
		t.Fatalf("startItem on clogged repo = %v, want errLoopLater", err)
	}
	if !strings.Contains(r.out.String(), "SUPERVISOR START WAITS o/r#42: held: o/r has 10 unmerged pull requests opened in the last 7d") {
		t.Fatalf("expected SUPERVISOR START WAITS log, got:\n%s", r.out.String())
	}

	// A second attempt in the same backlog episode does not file a duplicate guide problem.
	if _, err := l.startItem(context.Background(), act, loopBoardItem{Ref: holdRef, URL: holdURL}); err != errLoopLater {
		t.Fatalf("second startItem on clogged repo = %v, want errLoopLater", err)
	}
	evs, err := r.a.store.Events(0, func(e state.Event) bool {
		return strings.HasSuffix(e.Verb, ".file") && strings.Contains(e.Detail, "Repository o/r has 10 open pull requests created in the last 7d")
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("expected 1 guide escalation event for the episode, got %d: %+v", len(evs), evs)
	}

	// Once the backlog drains below 10, new starts in o/r proceed and the stage clears.
	openPulls = cloggedPulls[:4]
	l.mu.Lock()
	l.unmerged = nil
	l.mu.Unlock()
	if _, err := l.startItem(context.Background(), act, loopBoardItem{Ref: holdRef, URL: holdURL}); err != nil {
		t.Fatalf("startItem after backlog drained failed: %v", err)
	}
	if !slices.Contains(starts, "r 42") {
		t.Fatalf("expected worker 'r 42' to start once backlog drained, got %v", starts)
	}
}

func TestBriefIssuesAndRecordStartClaimAllBulkIssues(t *testing.T) {
	const (
		batchA = "o/batch#201"
		batchB = "o/batch#202"
	)
	brief := "Ship this batch:\n" +
		"- https://github.com/o/batch/issues/201\n" +
		"- https://github.com/o/batch/issues/202\n" +
		"- https://github.com/o/batch/issues/201 (duplicate link)\n" +
		"Follow-up to https://github.com/o/batch/issues/50.\n" +
		"Blocked by https://github.com/o/batch/issues/60.\n" +
		"See https://github.com/o/batch/pull/99 for context."
	got := briefIssues(brief)
	want := []string{batchA, batchB}
	if !slices.Equal(got, want) {
		t.Fatalf("briefIssues() = %v, want %v", got, want)
	}

	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	who := state.Party{Session: "sess-batch", Name: "r batch"}
	st := &state.State{}
	s := state.Start{Party: who, By: who, At: now, Harness: claudeComm, Model: defaultReviewModel}
	if _, err := recordStart(st, s, "batch task", strings.Join(got, ","), func(state.Party) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if len(st.Records) != 2 || st.Records[0].Issue != batchA || st.Records[1].Issue != batchB {
		t.Fatalf("st.Records = %+v, want %s and %s", st.Records, batchA, batchB)
	}
}

func TestOpenPullStartGateRefusesDuplicateIssueAndUnmergedLimit(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	oldRead := readRepoOpenPulls
	t.Cleanup(func() { readRepoOpenPulls = oldRead })

	readRepoOpenPulls = func(_ context.Context, targetRepo string) ([]unmergedPull, error) {
		switch targetRepo {
		case "o/dup":
			return []unmergedPull{
				{
					Number:    77,
					URL:       "https://github.com/o/dup/pull/77",
					CreatedAt: now.Add(-time.Hour),
					Body:      "Closes https://github.com/o/dup/issues/12",
				},
			}, nil
		case "o/clogged":
			var pulls []unmergedPull
			for i := 1; i <= 10; i++ {
				pulls = append(pulls, unmergedPull{
					Number:    i,
					URL:       fmt.Sprintf("https://github.com/o/clogged/pull/%d", i),
					CreatedAt: now.Add(-time.Duration(i) * time.Hour),
				})
			}
			return pulls, nil
		default:
			return nil, nil
		}
	}

	const dupRef = "o/dup#12"
	a := &app{cfg: &config.Config{}, now: now, out: &bytes.Buffer{}}
	err := a.openPullStartGate(context.Background(), "dup 12", []string{dupRef}, false, false, false)
	if err == nil || !strings.Contains(err.Error(), "o/dup#12 already has an open pull request https://github.com/o/dup/pull/77") {
		t.Fatalf("openPullStartGate duplicate PR = %v, want duplicate PR refusal", err)
	}
	if err := a.openPullStartGate(context.Background(), "dup 12", []string{dupRef}, true, false, false); err != nil {
		t.Fatalf("openPullStartGate for review should pass, got %v", err)
	}
	if err := a.openPullStartGate(context.Background(), "dup 12", []string{dupRef}, false, true, true); err != nil {
		t.Fatalf("openPullStartGate with --force should pass, got %v", err)
	}

	err = a.openPullStartGate(context.Background(), "clogged 99", []string{"o/clogged#99"}, false, false, false)
	if err == nil || !strings.Contains(err.Error(), "held: o/clogged has 10 unmerged pull requests opened in the last 7d") {
		t.Fatalf("openPullStartGate clogged repo = %v, want unmerged backlog refusal", err)
	}
}

func TestOpenPullClosingIssueAndRecordSessionIssues(t *testing.T) {
	const (
		closeRepo = "o/close"
		closeA    = "o/close#200"
		closeB    = "o/close#205"
	)
	pulls := []unmergedPull{
		{
			Number:      70,
			URL:         "https://github.com/o/close/pull/70",
			HeadRefName: "worker-a",
			Body:        "Fixes https://github.com/o/close/issues/200",
		},
		{
			Number:      71,
			URL:         "https://github.com/o/close/pull/71",
			HeadRefName: "worker-b",
			IsDraft:     true,
			Body:        "Fixes https://github.com/o/close/issues/205",
		},
	}

	issue, prURL := openPullClosingIssue(closeRepo, pulls, "worker-c", []string{closeA}, nil)
	if issue != closeA || prURL != "https://github.com/o/close/pull/70" {
		t.Fatalf("openPullClosingIssue() = (%q, %q), want (%s, https://github.com/o/close/pull/70)", issue, prURL, closeA)
	}
	// Own branch is ignored so retries and updates pass.
	if issue, _ := openPullClosingIssue(closeRepo, pulls, "worker-a", []string{closeA}, nil); issue != "" {
		t.Fatalf("openPullClosingIssue on own branch = %q, want empty", issue)
	}
	// Absorbed PRs in a combining PR are ignored.
	if issue, _ := openPullClosingIssue(closeRepo, pulls, "worker-combine", []string{closeA}, []string{"o/close#70"}); issue != "" {
		t.Fatalf("openPullClosingIssue on absorbed PR = %q, want empty", issue)
	}
	// Draft PRs do not block.
	if issue, _ := openPullClosingIssue(closeRepo, pulls, "worker-c", []string{closeB}, nil); issue != "" {
		t.Fatalf("openPullClosingIssue on draft PR = %q, want empty", issue)
	}

	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	who := state.Party{Session: "sess-1", Name: "r 200"}
	st := &state.State{
		Starts:  []state.Start{{Party: who, At: now}},
		Records: []state.Record{{Session: who, Issue: closeA, By: who, At: now}},
	}
	evs := recordSessionIssues(st, who, who, now, []string{closeA, closeB, "o/close#206"})
	if len(evs) != 2 {
		t.Fatalf("recordSessionIssues events = %d, want 2 new records", len(evs))
	}
	if len(st.Records) != 3 {
		t.Fatalf("st.Records = %+v, want 3 active records", st.Records)
	}
}

func TestServingWorkClaimsWorkerCreatedIssues(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	who := state.Party{Session: "sess-creator", Name: "r creator"}
	st := &state.State{
		Agents: []state.Agent{{Party: who, Task: "fix batch"}},
		Starts: []state.Start{{Party: who, At: now}},
	}
	sessions := []*claude.Session{
		{ID: "sess-creator", Name: "r creator", LastActive: now},
	}
	work := []claude.Work{
		{Created: []string{"o/r#321"}},
	}
	sw := servingWork(st, sessions, work, now.Add(-time.Hour))
	if len(sw) != 1 || !slices.Contains(sw[0].refs, "o/r#321") {
		t.Fatalf("servingWork did not include created issue o/r#321: %+v", sw)
	}
}
