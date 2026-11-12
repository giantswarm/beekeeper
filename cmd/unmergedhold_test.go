package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/loop"
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

	// Leave one slot beyond the pending review reservation so this test
	// exercises the repository backlog gate and its recovery.
	const backlogCapacity = `{"ceiling":2,"room":2}`
	var starts []string
	r.answer = func(args []string) (string, int) {
		if args[0] == loop.LoopCapacity {
			return backlogCapacity, 0
		}
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
	l.Once = true
	if err := l.Pass(context.Background()); err != nil {
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
	if _, err := l.StartItem(context.Background(), act, loop.LoopBoardItem{Ref: holdRef, URL: holdURL}); !errors.Is(err, loop.ErrLoopLater) {
		t.Fatalf("startItem on clogged repo = %v, want errLoopLater", err)
	}
	if !strings.Contains(r.out.String(), "SUPERVISOR START WAITS o/r#42: held: o/r has 10 unmerged pull requests opened in the last 7d") {
		t.Fatalf("expected SUPERVISOR START WAITS log, got:\n%s", r.out.String())
	}

	// A second attempt in the same backlog episode does not file a duplicate guide problem.
	if _, err := l.StartItem(context.Background(), act, loop.LoopBoardItem{Ref: holdRef, URL: holdURL}); !errors.Is(err, loop.ErrLoopLater) {
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
	l.Mu.Lock()
	l.Unmerged = nil
	l.Mu.Unlock()
	if _, err := l.StartItem(context.Background(), act, loop.LoopBoardItem{Ref: holdRef, URL: holdURL}); err != nil {
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

	const (
		dupRef  = "o/dup#12"
		dupURL  = "https://github.com/o/dup/issues/12"
		clogRef = "o/clogged#99"
		clogURL = "https://github.com/o/clogged/issues/99"
	)
	a := &app{cfg: &config.Config{}, now: now, out: &bytes.Buffer{}}
	err := a.openPullStartGate(context.Background(), "dup 12", dupURL, []string{dupRef}, false, false, false)
	if err == nil || !strings.Contains(err.Error(), "o/dup#12 already has an open pull request https://github.com/o/dup/pull/77") {
		t.Fatalf("openPullStartGate duplicate PR = %v, want duplicate PR refusal", err)
	}
	if err := a.openPullStartGate(context.Background(), "dup 12", dupURL, []string{dupRef}, true, false, false); err != nil {
		t.Fatalf("openPullStartGate for review should pass, got %v", err)
	}
	if err := a.openPullStartGate(context.Background(), "dup 12", dupURL, []string{dupRef}, false, true, true); err != nil {
		t.Fatalf("openPullStartGate with --force should pass, got %v", err)
	}

	err = a.openPullStartGate(context.Background(), "clogged 99", clogURL, []string{clogRef}, false, false, false)
	if err == nil || !strings.Contains(err.Error(), "held: o/clogged has 10 unmerged pull requests opened in the last 7d") {
		t.Fatalf("openPullStartGate clogged repo = %v, want unmerged backlog refusal", err)
	}
}

func TestExistingPullFixerAndIncidentRepairBypassUnmergedBacklogAndSelfDuplicate(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "repair-owner-session")
	r := newLoopRig(t)
	now := r.a.timeNow()

	readRepoOpenPulls = func(_ context.Context, targetRepo string) ([]unmergedPull, error) {
		if !strings.EqualFold(targetRepo, repo) {
			return nil, nil
		}
		var pulls []unmergedPull
		for i := 1; i <= 10; i++ {
			body := ""
			if i == 7 {
				body = "Closes https://github.com/o/r/issues/70"
			}
			if i == 8 {
				body = "Closes https://github.com/o/r/issues/88"
			}
			pulls = append(pulls, unmergedPull{
				Number:    i,
				URL:       fmt.Sprintf("https://github.com/o/r/pull/%d", i),
				CreatedAt: now.Add(-time.Duration(i) * time.Hour),
				Body:      body,
			})
		}
		return pulls, nil
	}

	// Unauthenticated callers spoofing PullWorkerName(loopPR), citing someone else's
	// parked PR, or passing a PR-only brief without an owned park or supervisor claim are refused.
	fixerBrief := "Fix CI on https://github.com/o/r/pull/7\nServes: https://github.com/o/r/issues/70"
	fixerIssues := briefIssues(fixerBrief)
	if err := r.a.openPullStartGate(context.Background(), superloop.PullWorkerName(loopPR), fixerBrief, fixerIssues, false, false, false); err == nil {
		t.Fatal("unauthenticated PullWorkerName start should be refused")
	}
	prOnlyBrief := "Fix review findings on https://github.com/o/r/pull/7"
	prOnlyIssues := briefIssues(prOnlyBrief)
	if err := r.a.openPullStartGate(context.Background(), "custom fixer", prOnlyBrief, prOnlyIssues, false, false, false); err == nil {
		t.Fatal("unauthenticated PR-only brief should be refused")
	}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.ReviewParks = append(st.ReviewParks, state.ReviewPark{
			On:     loopPR,
			Agent:  "custom fixer",
			Party:  func() state.Party { p, _ := r.a.caller(); return p }(),
			Serves: "o/r#70",
		})
		return []state.Event{
			about(event(loop.LoopParty, loop.LoopClaimVerb, "[red %s@%s] %s", loopPR, loopHead, superloop.PullWorkerName(loopPR)), loopPR),
		}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.a.openPullStartGate(context.Background(), "unrelated worker", fixerBrief, fixerIssues, false, false, false); err == nil {
		t.Fatal("unrelated worker citing another agent's parked PR should be refused")
	}

	// A name-only caller cannot borrow the retained owner's park, even when
	// the target PR and served issue are exactly right and its roster is gone.
	originalAs := r.a.as
	r.a.as = "custom fixer"
	if err := r.a.openPullStartGate(context.Background(), "custom fixer", fixerBrief, fixerIssues, false, false, false); err == nil {
		t.Fatal("name-only caller borrowed retained repair park")
	}
	r.a.as = originalAs

	// 1. Authenticated existing-PR fixer whose brief includes both the PR URL and served issue URL
	// passes openPullStartGate on a clogged repo and does not self-conflict.
	if err := r.a.openPullStartGate(context.Background(), superloop.PullWorkerName(loopPR), fixerBrief, fixerIssues, false, false, false); err != nil {
		t.Fatalf("fixer start for existing PR with served issue failed: %v", err)
	}
	// Bundling an unrelated issue in the same clogged repo is still refused.
	mixedBrief := "Fix CI on https://github.com/o/r/pull/7\n- https://github.com/o/r/issues/70\n- https://github.com/o/r/issues/71"
	if err := r.a.openPullStartGate(context.Background(), superloop.PullWorkerName(loopPR), mixedBrief, briefIssues(mixedBrief), false, false, false); err == nil {
		t.Fatal("fixer brief bundling unrelated issue o/r#71 should be refused by unmerged hold")
	}

	// 2. Authenticated PR-only repair brief (where custom fixer owns ReviewPark on o/r#7) passes.
	if err := r.a.openPullStartGate(context.Background(), "custom fixer", prOnlyBrief, prOnlyIssues, false, false, false); err != nil {
		t.Fatalf("PR-only repair brief failed: %v", err)
	}

	// 3. Named incident repair (--incident --except o/r#88) passes skipUnmergedBacklog,
	// l.unmergedStart, and openPullStartGate even while o/r has 10 unmerged PRs and an open PR for #88.
	const (
		incidentRef = "o/r#88"
		incidentURL = "https://github.com/o/r/issues/88"
	)
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Holds = append(st.Holds, state.Hold{
			Target:   repo,
			Reason:   "merge queue incident",
			Incident: true,
			Except:   incidentRef,
			At:       now,
		})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	cands := []board.Candidate{
		{Item: board.Item{Ref: incidentRef}, Step: stepUpNext},
		{Item: board.Item{Ref: "o/r#89"}, Step: stepUpNext},
	}
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	sc := pickScope{me: state.Party{Session: "me"}, alive: func(state.Party) bool { return false }, listed: now}
	r.a.skipUnmergedBacklog(context.Background(), &bytes.Buffer{}, cands, func() *board.Candidate {
		return nextFree(st, cands, sc).Pick
	})
	if res := nextFree(st, cands, sc); res.Pick == nil || res.Pick.Ref != incidentRef {
		t.Fatalf("incident repair candidate should remain the pick, got %+v", res.Pick)
	}
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.UnmergedStart(context.Background(), incidentRef); err != nil {
		t.Fatalf("unmergedStart for named incident repair = %v, want nil", err)
	}
	if err := l.UnmergedStart(context.Background(), "o/r#89"); !errors.Is(err, loop.ErrLoopLater) {
		t.Fatalf("unmergedStart for non-exempt issue = %v, want errLoopLater", err)
	}
	if err := r.a.openPullStartGate(context.Background(), "r 88", incidentURL, []string{incidentRef}, false, false, false); err != nil {
		t.Fatalf("openPullStartGate for named incident repair = %v, want nil", err)
	}
	if err := r.a.openPullStartGate(context.Background(), "r 88", incidentURL+"\nhttps://github.com/o/r/issues/89", []string{incidentRef, "o/r#89"}, false, false, false); err == nil {
		t.Fatal("incident repair bundled with non-exempt issue o/r#89 should be refused")
	}
}

func TestPRListErrorsFailClosedAndPaginationFindsDuplicatesBeyond100(t *testing.T) {
	r := newLoopRig(t)
	now := r.a.timeNow()

	// 1. PR-list read errors fail closed across openPullStartGate, unmergedStart,
	// skipUnmergedBacklog, and hookPRCreate.
	readRepoOpenPulls = func(_ context.Context, targetRepo string) ([]unmergedPull, error) {
		return nil, fmt.Errorf("github API 502 for %s", targetRepo)
	}
	const (
		issueRef = "o/r#55"
		issueURL = "https://github.com/o/r/issues/55"
	)
	if err := r.a.openPullStartGate(context.Background(), "r 55", issueURL, []string{issueRef}, false, false, false); err == nil || !strings.Contains(err.Error(), "cannot be read") {
		t.Fatalf("openPullStartGate on PR-list error = %v, want fail-closed refusal", err)
	}
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.UnmergedStart(context.Background(), issueRef); !errors.Is(err, loop.ErrLoopLater) {
		t.Fatalf("unmergedStart on PR-list error = %v, want errLoopLater", err)
	}
	cands := []board.Candidate{
		{Item: board.Item{Ref: issueRef}, Step: stepUpNext},
	}
	st := &state.State{}
	sc := pickScope{me: state.Party{Session: "me"}, alive: func(state.Party) bool { return false }, listed: now}
	r.a.skipUnmergedBacklog(context.Background(), &bytes.Buffer{}, cands, func() *board.Candidate {
		return nextFree(st, cands, sc).Pick
	})
	if res := nextFree(st, cands, sc); res.Pick != nil || len(res.Skipped) != 1 {
		t.Fatalf("skipUnmergedBacklog on PR-list error left pick = %+v, skipped = %+v", res.Pick, res.Skipped)
	}
	if reason := r.a.hookPRCreate("sess-1", repo, "branch-a", "fix: issue 55", "Closes "+issueURL); !strings.Contains(reason, "cannot be read") {
		t.Fatalf("hookPRCreate on PR-list error = %q, want fail-closed refusal", reason)
	}

	// 2. Pagination beyond 100 open PRs via readRepoOpenPullsWithGH finds PR #140
	// closing o/r#55 on the second page, and hookPRCreate refuses duplicate raw gh pr create
	// while recording session issue claims when admitted.
	calls := 0
	mockGH := func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		cmdLine := strings.Join(args, " ")
		if !strings.HasPrefix(cmdLine, "api graphql ") || !strings.Contains(cmdLine, "states:OPEN") {
			t.Fatalf("expected repository GraphQL connection: %v", args)
		}
		if strings.Contains(cmdLine, "after=next") {
			return inventoryPage(t, []unmergedPull{{Number: 140, URL: "https://github.com/o/r/pull/140", State: emergencyOpenState, CreatedAt: now.Add(-400 * time.Hour), HeadRefName: "worker-140", Body: "Closes " + issueURL}}, false, ""), nil
		}
		var page strings.Builder
		page.WriteByte('[')
		for i := 1; i <= 100; i++ {
			if i > 1 {
				page.WriteByte(',')
			}
			ts := now.Add(-time.Duration(200+i) * time.Hour).UTC().Format(time.RFC3339)
			fmt.Fprintf(&page, `{"number":%d,"url":"https://github.com/o/r/pull/%d","state":"OPEN","createdAt":%q,"headRefName":"worker-%d","body":""}`, i, i, ts, i)
		}
		page.WriteByte(']')
		var pulls []unmergedPull
		if err := json.Unmarshal([]byte(page.String()), &pulls); err != nil {
			t.Fatal(err)
		}
		return inventoryPage(t, pulls, true, loop.DeliveryNextCommand), nil
	}
	unmergedGH = mockGH
	readRepoOpenPulls = func(ctx context.Context, targetRepo string) ([]unmergedPull, error) {
		return readRepoOpenPullsWithGH(ctx, unmergedGH, targetRepo)
	}

	if err := r.a.openPullStartGate(context.Background(), "r 55", issueURL, []string{issueRef}, false, false, false); err == nil || !strings.Contains(err.Error(), "https://github.com/o/r/pull/140") {
		t.Fatalf("openPullStartGate did not find duplicate PR #140 beyond 100-item page: %v", err)
	}
	if calls < 2 {
		t.Fatalf("expected pagination across at least 2 gh calls, got %d", calls)
	}
	if reason := r.a.hookPRCreate("sess-1", repo, "branch-dup", "fix: issue 55", "Closes "+issueURL); !strings.Contains(reason, "https://github.com/o/r/pull/140") {
		t.Fatalf("hookPRCreate did not refuse duplicate PR #140: %q", reason)
	}
	// Admitted raw gh pr create for an unclaimed issue records the session's issue claim in state.
	const newIssueURL = "https://github.com/o/r/issues/56"
	if reason := r.a.hookPRCreate("sess-1", repo, "branch-56", "fix: issue 56", "Closes "+newIssueURL); reason != "" {
		t.Fatalf("hookPRCreate for new issue 56 refused: %q", reason)
	}
	stAfter, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(stAfter.Records, func(rec state.Record) bool { return strings.EqualFold(rec.Issue, "o/r#56") }) {
		t.Fatalf("hookPRCreate did not record issue claim for o/r#56 in st.Records: %+v", stAfter.Records)
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

func TestReview3325PaginationStalls(t *testing.T) {
	r := newLoopRig(t)
	now := r.a.timeNow()
	const (
		issueRef = "o/r#55"
		issueURL = "https://github.com/o/r/issues/55"
	)
	sameTS := now.Add(-200 * time.Hour).UTC().Format(time.RFC3339)
	var fullSameTSPage strings.Builder
	fullSameTSPage.WriteByte('[')
	for i := 1; i <= 100; i++ {
		if i > 1 {
			fullSameTSPage.WriteByte(',')
		}
		fmt.Fprintf(&fullSameTSPage, `{"number":%d,"url":"https://github.com/o/r/pull/%d","state":"OPEN","createdAt":%q,"headRefName":"worker-%d","body":""}`, i, i, sameTS, i)
	}
	fullSameTSPage.WriteByte(']')

	// More than 100 PRs can share the boundary timestamp. Refuse the incomplete
	// inventory rather than using a strict cursor that silently skips unseen ties.
	// The refusal must propagate to both start admission and raw creation.
	unmergedGH = func(_ context.Context, args ...string) ([]byte, error) {
		var pulls []unmergedPull
		if err := json.Unmarshal([]byte(fullSameTSPage.String()), &pulls); err != nil {
			t.Fatal(err)
		}
		return inventoryPage(t, pulls, true, "stalled"), nil
	}
	readRepoOpenPulls = func(ctx context.Context, targetRepo string) ([]unmergedPull, error) {
		return readRepoOpenPullsWithGH(ctx, unmergedGH, targetRepo)
	}

	if err := r.a.openPullStartGate(context.Background(), "r 55", issueURL, []string{issueRef}, false, false, false); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("openPullStartGate did not refuse incomplete inventory when 100 open PRs share one timestamp: %v", err)
	}
	if reason := r.a.hookPRCreate("sess-1", repo, "branch-dup", "fix: issue 55", "Closes "+issueURL); !strings.Contains(reason, "incomplete") {
		t.Fatalf("hookPRCreate did not refuse incomplete inventory when 100 open PRs share one timestamp: %q", reason)
	}
}

const review3325IterationCap = "iteration cap"

const review3325ShortMissingTimestamp = "short missing timestamp"

func TestReview3325RepairSessionOwnership(t *testing.T) {
	owner := state.Party{Name: "retained owner", Session: "original-session", Host: "host-a"}
	for _, kind := range []string{taskKindReview, "retained park", a2aRoleAgent} {
		t.Run(kind, func(t *testing.T) {
			st := &state.State{}
			switch kind {
			case taskKindReview:
				st.ReviewParks = []state.ReviewPark{{On: loopPR, Agent: owner.Name, Party: owner}}
			case "retained park":
				st.Parks = []state.Park{{On: loopPR, By: owner}}
			case a2aRoleAgent:
				st.Agents = []state.Agent{{Party: owner, Park: &state.Park{On: loopPR}}}
			}
			if !isTrackedRepairPull(st, owner, owner.Name, loopPR) {
				t.Fatal("genuine session refused")
			}
			for _, caller := range []state.Party{{Name: owner.Name}, {Name: owner.Name, Session: "replacement-session"}, {Name: owner.Name, Session: owner.Session, Host: "host-b"}} {
				if isTrackedRepairPull(st, caller, owner.Name, loopPR) {
					t.Fatalf("borrowed retained %s with %+v", kind, caller)
				}
			}
			if isTrackedRepairPull(st, owner, owner.Name, "o/r#999") {
				t.Fatal("unrelated PR exempted")
			}
		})
	}
}

func TestReview3325InventoryCapsFailClosed(t *testing.T) {
	for _, mode := range []string{"tie", "missing timestamp", review3325ShortMissingTimestamp, review3325IterationCap} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			gh := func(_ context.Context, args ...string) ([]byte, error) {
				calls++
				if !strings.Contains(strings.Join(args, " "), "states:OPEN") {
					t.Fatal("open filter missing")
				}
				page := make([]unmergedPull, 100)
				if mode == review3325ShortMissingTimestamp {
					page = page[:1]
				}
				for i := range page {
					page[i].Number = i + 1
					page[i].CreatedAt = time.Unix(100000, 0)
					if mode == review3325IterationCap {
						page[i].Number += 100 * calls
						page[i].CreatedAt = time.Unix(int64(100000-100*calls-i), 0)
					}
					if mode == "missing timestamp" || mode == review3325ShortMissingTimestamp {
						page[i].CreatedAt = time.Time{}
					}
				}
				cursor := "tie"
				if mode == review3325IterationCap {
					cursor = fmt.Sprint(calls)
				}
				return inventoryPage(t, page, true, cursor), nil
			}
			if _, err := readRepoOpenPullsWithGH(context.Background(), gh, repo); err == nil {
				t.Fatal("certified incomplete inventory")
			}
			if calls > 20 {
				t.Fatal("unbounded reads")
			}
		})
	}
	for _, match := range []bool{false, true} {
		t.Run(fmt.Sprint("fallback match=", match), func(t *testing.T) {
			gh := func(context.Context, ...string) ([]byte, error) {
				page := make([]unmergedPull, 100)
				for i := range page {
					page[i] = unmergedPull{Number: i + 1, Body: "Mentions https://github.com/o/r/issues/55"}
				}
				if match {
					page[99].Body = "Closes https://github.com/o/r/issues/55"
				}
				return json.Marshal(page)
			}
			issue, _, err := findOpenPullClosingIssues(context.Background(), gh, repo, make([]unmergedPull, 100), "", []string{"o/r#55"}, nil)
			if match && (err != nil || issue == "") {
				t.Fatalf("affirmative duplicate lost: %s %v", issue, err)
			}
			if !match && err == nil {
				t.Fatal("100 mentions certified absence of older closing PR")
			}
		})
	}
}
