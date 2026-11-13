//go:build unix

package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const reviewTestPath = "main.go"

// reviewCaseRedCI is the review start case whose head has failing checks.
const reviewCaseRedCI = "red CI"

func TestLightReview(t *testing.T) {
	for _, tt := range []struct {
		name                    string
		lines, threshold, count int
		files                   []string
		want                    bool
	}{
		{"below boundary", 149, 150, 1, []string{reviewTestPath}, true},
		{"boundary", 150, 150, 1, []string{reviewTestPath}, false},
		{"configured", 180, 200, 1, []string{reviewTestPath}, true},
		{"disabled", 1, -1, 1, []string{reviewTestPath}, false},
		{"docs and tests", 900, -1, 2, []string{"docs/guide.md", "cmd/main_test.go"}, true},
		{"partial paths", 900, 150, 2, []string{"README.md"}, false},
		{"mixed", 900, 150, 2, []string{"README.md", "cmd/main.go"}, false},
		{"no paths", 900, 150, 0, nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := lightReview(superloop.Pull{Additions: tt.lines, Files: tt.files, ChangedFiles: tt.count}, tt.threshold)
			if got != tt.want {
				t.Fatalf("light = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReviewRoundPinsHeadAcrossRestarts(t *testing.T) {
	p := superloop.Pull{Ref: loopPR, Head: loopHead}
	for _, st := range []*state.State{
		{ReviewRequests: []state.ReviewRequest{{Ref: p.Ref, Head: p.Head, Round: 1, PID: os.Getpid()}}},
		{DeskReviews: []state.DeskReview{{Ref: p.Ref, Head: p.Head, Round: 1}}},
	} {
		if _, _, err := reviewRound(st, p); Code(err) != ExitRefused {
			t.Fatalf("duplicate not refused: %v", err)
		}
	}
	p.Reviews = []superloop.Review{{Head: loopFixed, Verdict: superloop.ChangesRequested, Body: "main.go:7: broken → fix"}}
	round, previous, err := reviewRound(&state.State{}, p)
	if err != nil || round != 2 || previous != p.Reviews[0].Body {
		t.Fatalf("round=%d previous=%q err=%v", round, previous, err)
	}
	p.Head = loopFixed
	if _, _, err := reviewRound(&state.State{}, p); Code(err) != ExitRefused {
		t.Fatalf("reviewed head not refused: %v", err)
	}
}

func TestAgentsReviewRefusesBeforeStarting(t *testing.T) {
	for _, name := range []string{reviewCaseRedCI, "draft PR", "closed", "reviewed", "requested"} {
		t.Run(name, func(t *testing.T) {
			r := newLoopRig(t)
			switch name {
			case reviewCaseRedCI:
				r.pull.Green = false
			case "draft PR":
				r.pull.Draft = true
			case "closed":
				r.pull.State = superloop.Merged
			case "reviewed":
				r.pull.Reviews = []superloop.Review{{Head: r.pull.Head, Verdict: superloop.Approved, Member: true}}
			case "requested":
				if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
					st.ReviewRequests = []state.ReviewRequest{{Ref: r.pull.Ref, Head: r.pull.Head, Round: 1, PID: os.Getpid()}}
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			c := r.a.agentReviewCmd()
			c.SetArgs([]string{loopPR})
			if err := c.ExecuteContext(t.Context()); Code(err) != ExitRefused {
				t.Fatalf("not refused: %v", err)
			}
			if r.called(agentsName, agentStartName) != nil {
				t.Fatal("started a reviewer")
			}
		})
	}
}

func TestAgentsReviewStartsInCheckoutWithFilledBrief(t *testing.T) {
	r := newLoopRig(t)
	dir := t.TempDir()
	gitT(t, dir, "init")
	gitT(t, dir, "remote", "add", "origin", "https://github.com/o/r.git")
	r.a.cfg.Agents.Dir = dir
	r.pull.Closes = []string{epicTen}
	r.pull.Additions = 20
	oldParents := reviewParents
	t.Cleanup(func() { reviewParents = oldParents })
	reviewParents = func(_ context.Context, issues []string) ([]string, error) {
		if !slices.Equal(issues, r.pull.Closes) {
			t.Fatalf("issues: %v", issues)
		}
		return []string{issueOneURL}, nil
	}
	c := r.a.agentReviewCmd()
	c.SetArgs([]string{loopPR, modelFlag, defaultReviewModel})
	if err := c.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	args := r.called(agentsName, agentStartName)
	if len(args) < 4 {
		t.Fatalf("no start: %v", args)
	}
	for flag, want := range map[string]string{"--dir": dir, modelFlag: defaultReviewModel} {
		i := slices.Index(args, flag)
		if i < 0 || i+1 >= len(args) || args[i+1] != want {
			t.Fatalf("start lacks %s %s: %v", flag, want, args)
		}
	}
	body, err := os.ReadFile(filepath.Clean(args[3]))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{loopURL, loopHead, "CI green at " + loopHead, "Light review:", epicTen, issueOneURL, "review round 1", "none (first round)", deskReviewApproved} {
		if !strings.Contains(string(body), want) {
			t.Errorf("brief lacks %q", want)
		}
	}
	c = r.a.agentReviewCmd()
	c.SetArgs([]string{loopPR})
	if err := c.ExecuteContext(t.Context()); Code(err) != ExitRefused {
		t.Fatalf("second review: %v", err)
	}
}

func TestRequireReviewRejectsGoWithoutFixedCommentAndAcceptsPinnedApproval(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Merge.RequireReview = true
	if err := r.a.store.Record(about(event(loop.LoopParty, "supervisor.go", "[go %s@%s] %s: merge go", loopPR, loopHead, loopWorker), loopPR)); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		body, head string
		want       bool
	}{
		{"", loopHead, false},
		{loopApproved, loopHead, true},
		{loopApproved, loopFixed, false},
		{deskReviewApproved, loopFixed, false},
		{deskReviewApproved, loopHead, true},
		{"Desk review (opus): changes requested", loopHead, false},
	} {
		r.pull.Reviews = []superloop.Review{{Head: tt.head, Verdict: superloop.Verdict("COMMENTED", tt.body), Body: tt.body, Member: true}}
		why, _, err := r.a.checkMergeHeadAuthority(t.Context(), nil, r.pull, r.pull.Head)
		if err != nil || (why == "") != tt.want {
			t.Errorf("body=%q head=%s: why=%q err=%v", tt.body, tt.head, why, err)
		}
	}
}

func TestSupervisorNitAttestationKeepsApprovalWithoutSecondRound(t *testing.T) {
	r := newLoopRig(t)
	r.a.as = loop.LoopParty.Name
	r.a.cfg.Merge.RequireReview = true
	r.park(t, loopPR, r.a.timeNow())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "Desk review (opus): approved\ncmd/x.go:3: spelling → correct spelling")
	r.pull.Head = loopFixed
	old := readChangedPast
	t.Cleanup(func() { readChangedPast = old })
	readChangedPast = func(context.Context, superloop.Pull, string) ([]string, error) { return []string{"cmd/x.go"}, nil }
	if err := r.a.checkReviewNits(t.Context(), r.pull, loopHead); err != nil {
		t.Fatal(err)
	}
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	why, _, err := r.a.checkMergeHeadAuthority(t.Context(), st, r.pull, r.pull.Head)
	if err != nil || why != "" {
		t.Fatalf("attested nits refused: %q %v", why, err)
	}
	r.pass(t)
	if start := r.called(agentsName, agentStartName); start != nil {
		t.Fatalf("second review started: %v", start)
	}
	if merge := r.called(GateCmdName); merge == nil {
		t.Fatalf("no merge go after nits: %s", r.out)
	}
	r.pull.Head = "3333333333333333333333333333333333333333"
	why, _, err = r.a.checkMergeHeadAuthority(t.Context(), st, r.pull, r.pull.Head)
	if err != nil || why == "" {
		t.Fatalf("attestation covered a later push: %q %v", why, err)
	}
}

func TestAgentsReviewFailureReleasesUnstartedRequest(t *testing.T) {
	r := newLoopRig(t)
	dir := t.TempDir()
	gitT(t, dir, "init")
	gitT(t, dir, "remote", "add", "origin", "https://github.com/o/r.git")
	r.a.cfg.Agents.Dir = dir
	r.answer = func(args []string) (string, int) {
		if len(args) > 1 && args[0] == agentsName && args[1] == agentStartName {
			return "review start failed", 1
		}
		return "", 0
	}
	c := r.a.agentReviewCmd()
	c.SetArgs([]string{loopPR})
	if err := c.ExecuteContext(t.Context()); err == nil {
		t.Fatal("start failure ignored")
	}
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.ReviewRequests) != 0 {
		t.Fatalf("unstarted request retained: %+v", st.ReviewRequests)
	}
}

func TestReviewParentReadFailsClosed(t *testing.T) {
	old := github.RunGH
	t.Cleanup(func() { github.RunGH = old })
	for _, raw := range []string{`{"errors":[{"message":"review start failed"}]}`, `{"data":{"repository":{"issue":null}}}`, `not-json-review-parent`} {
		github.RunGH = func(context.Context, ...string) ([]byte, error) { return []byte(raw), nil }
		if _, err := reviewParents(t.Context(), []string{refOne}); err == nil {
			t.Fatalf("unreadable parent accepted: %s", raw)
		}
	}
	github.RunGH = func(context.Context, ...string) ([]byte, error) {
		return []byte(`{"data":{"repository":{"issue":{"parent":null}}}}`), nil
	}
	if parents, err := reviewParents(t.Context(), []string{refOne}); err != nil || len(parents) != 0 {
		t.Fatalf("absent parent: %v %v", parents, err)
	}
}

func TestReviewReservationIsSharedWithAutomaticLoop(t *testing.T) {
	r := newLoopRig(t)
	act := superloop.Action{Ref: loopPR, Head: loopHead, Round: 1}
	if err := r.a.reserveReview(act); err != nil {
		t.Fatal(err)
	}
	if err := r.a.reserveReview(act); Code(err) != ExitRefused {
		t.Fatalf("duplicate start not refused: %v", err)
	}
	act.Head = loopFixed
	if err := r.a.reserveReview(act); Code(err) != ExitRefused {
		t.Fatalf("round collision not refused: %v", err)
	}
	act.Round = 2
	if err := r.a.reserveReview(act); err != nil {
		t.Fatalf("new head and round refused: %v", err)
	}
}

// A request alone is intent, not evidence that a reviewer ever launched.
func TestReviewReservationRecoversOrphan(t *testing.T) {
	for _, round := range []int{1, 2} {
		t.Run(superloop.ReviewerName(loopPR, round), func(t *testing.T) {
			r := newLoopRig(t)
			act := superloop.Action{Ref: loopPR, Head: loopHead, Round: 1}
			if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.ReviewRequests = []state.ReviewRequest{{Ref: loopPR, Head: loopHead, Round: round}}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			st, err := r.a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if got, _, err := reviewRound(st, r.pull); err != nil || got != 1 {
				t.Fatalf("orphan influenced round: %d %v", got, err)
			}
			if err := r.a.reserveReview(act); err != nil {
				t.Fatal(err)
			}
			if err := r.a.reserveReview(act); Code(err) != ExitRefused {
				t.Fatalf("concurrent start allowed: %v", err)
			}
			// A historical start event must not keep this failed attempt reserved.
			if err := r.a.store.Record(event(loop.LoopParty, verbStart, "%s: old failed start", superloop.ReviewerName(loopPR, 1))); err != nil {
				t.Fatal(err)
			}
			r.a.finishReviewRequest(act, false)
			st, err = r.a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if len(st.ReviewRequests) != 0 {
				t.Fatalf("orphan retained: %+v", st.ReviewRequests)
			}
		})
	}
}

func TestReviewReservationKeepsRegisteredReviewer(t *testing.T) {
	r := newLoopRig(t)
	act := superloop.Action{Ref: loopPR, Head: loopHead, Round: 1}
	if err := r.a.reserveReview(act); err != nil {
		t.Fatal(err)
	}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = append(st.Agents, state.Agent{Party: state.Party{Name: superloop.ReviewerName(loopPR, 1)}})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	// Delivery can fail after launch: retain the registered reviewer's request.
	r.a.finishReviewRequest(act, false)
	if err := r.a.reserveReview(act); Code(err) != ExitRefused {
		t.Fatalf("registered reviewer duplicated: %v", err)
	}
}

// An assignment whose reviewer is gone (off the roster, no live process, no
// running execution) and left no result is released by the next start of
// the review at its head, instead of refusing every retry of the sweep
// with "already assigned review of this head". Anything that still shows
// the reviewer, or its reported result, keeps the head assigned.
func TestReviewReservationReleasesGoneReviewer(t *testing.T) {
	reviewer := state.Party{Session: "gone-reviewer", Name: superloop.ReviewerName(loopPR, 1)}
	running := &state.ExecutionRecord{Phase: execution.Running, Observation: execution.Observation{ProcessState: execution.Running}}
	exited := &state.ExecutionRecord{Phase: execution.Running, Observation: execution.Observation{ProcessState: execution.Exited}}
	for _, tt := range []struct {
		name     string
		held     func(*loopRig, *state.State)
		released bool
	}{
		{name: "no roster entry, process or execution", released: true},
		{name: "done on the roster", held: func(_ *loopRig, st *state.State) {
			st.Agents = append(st.Agents, state.Agent{Party: reviewer, Done: true})
		}, released: true},
		{name: "remote execution ended", held: func(_ *loopRig, st *state.State) {
			st.Starts = append(st.Starts, state.Start{Party: reviewer, Execution: exited})
		}, released: true},
		{name: "on the roster", held: func(_ *loopRig, st *state.State) {
			st.Agents = append(st.Agents, state.Agent{Party: reviewer})
		}},
		{name: "on the roster under its name only", held: func(_ *loopRig, st *state.State) {
			st.Agents = append(st.Agents, state.Agent{Party: state.Party{Session: "successor", Name: reviewer.Name}})
		}},
		{name: "live process", held: func(r *loopRig, _ *state.State) {
			r.sessions = []*claude.Session{{ID: reviewer.Session}}
		}},
		{name: "process table unreadable", held: func(*loopRig, *state.State) {
			loopSessions = func(*app) ([]*claude.Session, *proc.Table, error) {
				return nil, nil, errors.New("process table unreadable")
			}
		}},
		{name: "remote execution running", held: func(_ *loopRig, st *state.State) {
			st.Starts = append(st.Starts, state.Start{Party: reviewer, Execution: running})
		}},
		{name: "reported result", held: func(_ *loopRig, st *state.State) {
			st.DeskReviews[0].Result = &state.DeskReviewResult{ID: "r1", Verdict: superloop.ChangesRequested, Head: loopHead, By: reviewer}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newLoopRig(t)
			if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.DeskReviews = []state.DeskReview{{ID: "round-1", Ref: loopPR, Head: loopHead, Round: 1, Worker: state.Party{Session: workerRole, Name: loopWorker}, Reviewer: reviewer, AssignedAt: r.a.timeNow().Add(-time.Hour)}}
				// The finished start of the gone reviewer left its request.
				st.ReviewRequests = []state.ReviewRequest{{Ref: loopPR, Head: loopHead, Round: 1}}
				if tt.held != nil {
					tt.held(r, st)
				}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			retry := superloop.Action{Ref: loopPR, Head: loopHead, Round: 2}
			err := r.a.reserveReview(retry)
			st, rerr := r.a.store.Read()
			if rerr != nil {
				t.Fatal(rerr)
			}
			if !tt.released {
				if Code(err) != ExitRefused {
					t.Fatalf("held assignment not refused: %v", err)
				}
				if !st.DeskReviews[0].Released.IsZero() {
					t.Fatalf("held assignment released: %+v", st.DeskReviews[0])
				}
				return
			}
			if err != nil {
				t.Fatalf("gone reviewer still blocks its head: %v", err)
			}
			if st.DeskReviews[0].Released.IsZero() {
				t.Fatalf("gone assignment not released: %+v", st.DeskReviews[0])
			}
			if !slices.ContainsFunc(st.ReviewRequests, func(q state.ReviewRequest) bool { return q.Round == 2 && q.Head == loopHead }) || slices.ContainsFunc(st.ReviewRequests, func(q state.ReviewRequest) bool { return q.Round == 1 }) {
				t.Fatalf("restart not reserved in place of the gone one: %+v", st.ReviewRequests)
			}
			events, err := r.a.store.Events(0, func(e state.Event) bool { return e.Verb == reviewReleasedVerb })
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 1 || !strings.Contains(events[0].Detail, reviewer.Name) {
				t.Fatalf("release not logged: %+v", events)
			}
			// The released round no longer holds the request or the
			// delivery's review, and the same retry is refused once.
			if !reviewRequestActive(st, state.ReviewRequest{Ref: loopPR, Head: loopHead, Round: 2, PID: os.Getpid()}) {
				t.Fatal("restart's own reservation inactive")
			}
			if reviewRequestOwned(st, state.ReviewRequest{Ref: loopPR, Head: loopHead, Round: 1}) {
				t.Fatal("released round still owns its request")
			}
			if err := r.a.reserveReview(retry); Code(err) != ExitRefused {
				t.Fatalf("duplicate restart not refused: %v", err)
			}
		})
	}
}

// A released round keeps its number: the head's next reviewer gets the next.
func TestReleasedReviewKeepsItsRoundButNotItsHead(t *testing.T) {
	st := &state.State{DeskReviews: []state.DeskReview{{Ref: loopPR, Head: loopHead, Round: 1, Released: time.Unix(1, 0)}}}
	if round, _, err := reviewRound(st, superloop.Pull{Ref: loopPR, Head: loopHead}); err != nil || round != 2 {
		t.Fatalf("released round: round %d, %v", round, err)
	}
}

// A restart can carry the number of the round it released (agents review
// numbers rounds from member verdicts the loop's ledger does not count): its
// reviewer still gets an assignment of its own, the one its result and the
// merge gate read, instead of the released round standing in for it.
func TestReleasedRoundDoesNotStandInForItsRestart(t *testing.T) {
	a, r := executionTestApp(t)
	head := strings.Repeat("a", 40)
	ref := r.Repository + "#7"
	act := superloop.Action{Ref: ref, Head: head, Round: 1, Agent: "local-author"}
	gone := state.Party{Session: "gone-reviewer", Name: superloop.ReviewerName(ref, 1)}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		start := &st.Starts[0]
		start.Name = superloop.ReviewerName(ref, 1)
		start.By = loop.LoopParty
		start.At = time.Now().Add(-time.Minute)
		st.DeskReviews = []state.DeskReview{{ID: "released", Ref: ref, Head: head, Round: 1, Worker: state.Party{Name: act.Agent}, Reviewer: gone, AssignedAt: time.Now().Add(-time.Hour), Released: time.Now().Add(-2 * time.Minute)}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	// Twice: the restart's own assignment stays idempotent.
	for range 2 {
		if err := a.assignExecutionReview(context.Background(), act); err != nil {
			t.Fatal(err)
		}
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.DeskReviews) != 2 {
		t.Fatalf("restart not assigned once beside its released round: %+v", st.DeskReviews)
	}
	restart := st.DeskReviews[1]
	if !restart.Released.IsZero() || restart.Round != 1 || !sameReviewSession(restart.Reviewer, st.Starts[0].Party) {
		t.Fatalf("restart's assignment: %+v", restart)
	}
	if round := executionReviewRound(st, execution.MergeUse{Repository: r.Repository, PR: 7}); round == nil || round.ID != restart.ID {
		t.Fatalf("merge gate reads %+v, not the restart's assignment", round)
	}
}

func TestReviewRequestFromExitedStarterIsInactive(t *testing.T) {
	request := state.ReviewRequest{Ref: loopPR, Head: loopHead, Round: 1, PID: 1 << 30}
	if reviewRequestActive(&state.State{}, request) {
		t.Fatal("exited starter still owns request")
	}
	st := &state.State{DeskReviews: []state.DeskReview{{Ref: loopPR, Head: loopHead, Round: 1}}}
	if !reviewRequestActive(st, request) {
		t.Fatal("assigned review lost its reservation")
	}
}

func TestRequireReviewCarriesApprovalOverBaseOnlyMerge(t *testing.T) {
	for _, covered := range []bool{true, false} {
		r := newLoopRig(t)
		r.a.cfg.Merge.RequireReview = true
		r.pull.Head = loopFixed
		r.pull.Reviews = []superloop.Review{{Head: loopHead, Verdict: superloop.Approved, Body: deskReviewApproved, Member: true}}
		r.baseMerge = func(p superloop.Pull, approved string) (bool, error) {
			if p.Head != loopFixed || approved != loopHead {
				t.Fatalf("wrong coverage comparison: %s..%s", approved, p.Head)
			}
			return covered, nil
		}
		st, err := r.a.store.Read()
		if err != nil {
			t.Fatal(err)
		}
		why, _, err := r.a.checkMergeHeadAuthority(t.Context(), st, r.pull, r.pull.Head)
		if err != nil || (why == "") != covered {
			t.Fatalf("covered %v: why=%q err=%v", covered, why, err)
		}
	}
}
