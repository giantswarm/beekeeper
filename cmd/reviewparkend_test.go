package cmd

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	endTestServed = "o/r#15"
	endTestPullA  = "o/r#21"
	endTestPullB  = "o/r#22"
)

// A continuation session of a task finishes the post-merge work of one pull
// request. Another pull request of the same worker name still waits for its
// first review, and its post-merge work must survive.
func TestDoneOfOnePullKeepsOtherReviewRequest(t *testing.T) {
	r := newLoopRig(t)
	now := r.a.now
	work := "roll it out"
	r.pulls[multiParkSecondPR] = superloop.Pull{Ref: multiParkSecondPR, URL: secondURL, State: superloop.Open, Head: loopHead, Green: true, Author: loopDesk}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.ReviewParks = []state.ReviewPark{{On: multiParkSecondPR, At: now.Add(-30 * time.Second), ReviewHead: loopHead, Agent: loopWorker,
			Party: state.Party{Name: loopWorker, Session: "first"}, PostMerge: &work}}
		return []state.Event{{At: now.Add(-10 * time.Second), By: state.Party{Name: loopWorker, Session: "continuation"}, Verb: superloop.VerbDone, Detail: loopWorker + " done: post-merge of the first pull request"}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	rp, ok := st.ReviewParkFor(multiParkSecondPR)
	if !ok || rp.PostMerge == nil || *rp.PostMerge != work {
		t.Errorf("open pull request lost its review request or post-merge work: %+v", st.ReviewParks)
	}
	if r.called(agentsName, agentStartName, superloop.ReviewerName(multiParkSecondPR, 1)) == nil {
		t.Errorf("no reviewer for the open pull request: %v\n%s", r.calls, r.out)
	}
}

// The fixer of pull request A and the first session of pull request B share
// the worker name: startFixer reuses it. Ending A must leave B's request.
func TestEndMergedKeepsSameNameOtherSessionRequest(t *testing.T) {
	for _, live := range []bool{false, true} {
		name := "saved park"
		if live {
			name = "live park"
		}
		t.Run(name, func(t *testing.T) {
			r := newLoopRig(t)
			pm := ""
			now := time.Now()
			fixer := state.Party{Name: loopWorker, Session: "fixer"}
			if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.ReviewParks = []state.ReviewPark{
					{On: loopPR, At: now, ReviewHead: loopHead, Agent: loopWorker, Party: fixer, PostMerge: &pm},
					{On: multiParkSecondPR, At: now, ReviewHead: loopHead, Agent: loopWorker, Party: state.Party{Name: loopWorker, Session: "first"}, PostMerge: &pm},
				}
				if live {
					st.Agents = []state.Agent{{Party: fixer, Task: "fix " + loopURL, Park: &state.Park{On: loopPR, At: now, ReviewHead: loopHead, PostMerge: &pm}}}
				}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			st, err := r.a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			l := &supervisorLoop{a: r.a, merged: map[string]superloop.Pull{loopPR: {MergeCommit: loopFixed}}}
			if _, err := l.endMerged(t.Context(), st, superloop.Action{Key: "end " + loopPR, Kind: superloop.KindEnd, Ref: loopPR, URL: loopURL, Message: strings.ToLower(superloop.Merged)}); err != nil {
				t.Fatal(err)
			}
			if st, err = r.a.store.Read(); err != nil {
				t.Fatal(err)
			}
			if _, ok := st.ReviewParkFor(multiParkSecondPR); !ok {
				t.Errorf("ending A deleted B's request: %+v", st.ReviewParks)
			}
			if _, ok := st.ReviewParkFor(loopPR); ok {
				t.Errorf("ending A kept A's own request: %+v", st.ReviewParks)
			}
		})
	}
}

// The live worker runs the done command the loop told it to run after a merge.
func TestIdleDoneKeepsOtherOpenRequest(t *testing.T) {
	w := parkingWatch(t, nil)
	agent := state.Party{Session: "done-session", HostSession: "local_done-session", Name: agentOne}
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Party = agent
		st.Records = []state.Record{{Session: agent}}
		st.ReviewParks = []state.ReviewPark{{On: multiParkSecondPR, At: relayNow, ReviewHead: loopHead, Agent: agentOne, Party: agent}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	c := w.agentsCmd()
	c.SetOut(w.out)
	c.SetErr(w.out)
	c.SetArgs([]string{stateIdle, flagDone, flagReport, "first pull request done", flagProblem, noProblems})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	st, err := w.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !st.Agents[0].Done {
		t.Fatalf("worker did not finish: %+v", st.Agents[0])
	}
	if _, ok := st.ReviewParkFor(multiParkSecondPR); !ok {
		t.Errorf("done of the worker deleted its other open pull request's request")
	}
}

// The cache's clock runs on the wall: a pass frozen at its start stamps what
// it read late as read long ago, and the next pass reads all of it again.
func TestObservePullAfterLongPassStaysCached(t *testing.T) {
	r := newLoopRig(t)
	reads := 0
	prev := readLoopPull
	readLoopPull = func(ctx context.Context, ref string) (superloop.Pull, error) {
		reads++
		return prev(ctx, ref)
	}
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	r.a.loopNow = &atomic.Pointer[time.Time]{}
	passStart := time.Now().Add(-loopPullEvery - time.Minute)
	r.a.loopNow.Store(&passStart)
	changed := time.Now()
	if _, err := l.observePull(t.Context(), loopPR, changed); err != nil {
		t.Fatal(err)
	}
	next := time.Now()
	r.a.loopNow.Store(&next)
	if _, err := l.observePull(t.Context(), loopPR, changed); err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Fatalf("the pass after a long pass read the pull request %d times, want 1", reads)
	}
}

// endRig has two merged pull requests with saved parks, each serving a closed
// issue, for ends that reach the board.
func endRig(t *testing.T) (*loopRig, *supervisorLoop, *state.State) {
	t.Helper()
	r := newLoopRig(t)
	r.a.cfg.Board.Owner = "o"
	refStates = func(_ context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
		out := map[github.PR]github.RefState{}
		for _, ref := range refs {
			out[ref] = github.RefState{Kind: github.RefIssue, State: github.Closed}
		}
		return out, nil
	}
	pm := ""
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		for i, ref := range []string{endTestPullA, endTestPullB} {
			name := []string{"worker a", "worker b"}[i]
			st.ReviewParks = append(st.ReviewParks, state.ReviewPark{On: ref, At: time.Now(), ReviewHead: loopHead, Agent: name,
				Party: state.Party{Name: name, Session: name}, Serves: endTestServed, PostMerge: &pm})
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.once = true
	l.merged = map[string]superloop.Pull{endTestPullA: {MergeCommit: loopFixed}, endTestPullB: {MergeCommit: loopFixed}}
	return r, l, st
}

func endAction(ref string) superloop.Action {
	return superloop.Action{Key: "end " + ref, Kind: superloop.KindEnd, Ref: ref, URL: "https://github.com/" + strings.Replace(ref, "#", "/pull/", 1), Message: strings.ToLower(superloop.Merged)}
}

// An end carries no agent. Sharing one stream, an end stalled on the board
// would hold every other end, in this pass and each one after it.
func TestStalledEndDoesNotHoldAnotherEnd(t *testing.T) {
	r, l, st := endRig(t)
	var both sync.WaitGroup
	both.Add(2)
	met := make(chan struct{})
	go func() {
		both.Wait()
		close(met)
	}()
	var stalled atomic.Int32
	r.answer = func(args []string) (string, int) {
		if len(args) > 1 && args[0] == loopBoard && args[1] == "move" {
			both.Done()
			select {
			case <-met:
			case <-time.After(2 * time.Second):
				stalled.Add(1)
			}
		}
		return "", 0
	}
	actions := []superloop.Action{endAction(endTestPullA), endAction(endTestPullB)}
	if err := l.runPassActions(t.Context(), st, map[string]loopClaim{}, map[string]loopFailure{}, nil, actions); err != nil {
		t.Fatal(err)
	}
	if stalled.Load() != 0 {
		t.Fatalf("one end waited for the other: %d stalled\n%s", stalled.Load(), r.out)
	}
	if st, err := r.a.store.Read(); err != nil || len(st.ReviewParks) != 0 {
		t.Fatalf("ends did not finish: %+v, %v", st.ReviewParks, err)
	}
}

// A saved Serves can name a pull request. The board refuses it as no issue,
// and that must end the tracking all the same; any other refusal is a failure.
func TestEndMergedToleratesServedPullRequest(t *testing.T) {
	for _, tc := range []struct {
		name, refusal string
		fails         bool
	}{
		{name: "no issue", refusal: "beekeeper: " + endTestServed + " is no issue: Could not resolve to an Issue"},
		{name: "other refusal", refusal: "beekeeper: board refused the move", fails: true},
	} {
		for _, live := range []bool{false, true} {
			t.Run(tc.name+"/live="+map[bool]string{false: "no", true: "yes"}[live], func(t *testing.T) {
				r, l, st := endRig(t)
				r.answer = func(args []string) (string, int) {
					if len(args) > 1 && args[0] == loopBoard && args[1] == "move" {
						return tc.refusal, ExitRefused
					}
					return "", 0
				}
				if live {
					pm := ""
					party := state.Party{Name: "worker a", Session: "worker a"}
					if err := r.a.store.Update(func(current *state.State) ([]state.Event, error) {
						current.ReviewParks = current.ReviewParks[:0]
						current.Agents = []state.Agent{{Party: party, Task: "fix", Park: &state.Park{On: endTestPullA, At: time.Now(), ReviewHead: loopHead, PostMerge: &pm}}}
						current.Records = []state.Record{{Session: party, Issue: "https://github.com/o/r/issues/15"}}
						return nil, nil
					}); err != nil {
						t.Fatal(err)
					}
					var err error
					if st, err = r.a.store.Read(); err != nil {
						t.Fatal(err)
					}
				}
				_, err := l.endMerged(t.Context(), st, endAction(endTestPullA))
				if (err != nil) != tc.fails {
					t.Fatalf("endMerged: %v, want failure %t", err, tc.fails)
				}
				if r.called(loopBoard, "move", endTestServed, "Done") == nil {
					t.Fatalf("no board move: %v", r.calls)
				}
				if st, err = r.a.store.Read(); err != nil {
					t.Fatal(err)
				}
				if finished := len(st.WorkerReports) == 1; finished == tc.fails {
					t.Fatalf("task closed = %t with failure %t: %+v", finished, tc.fails, st.WorkerReports)
				}
			})
		}
	}
}

// A task that names a pull request serves no issue: Serves would send the end
// to the board with a pull request, or wake the worker for its own open one.
func TestServesIgnoresPullRequestInTask(t *testing.T) {
	const pullTask, issueTask = "take over https://github.com/o/r/pull/5", "fix https://github.com/o/r/issues/15"
	for _, tc := range []struct{ name, task, want string }{
		{"pull request", pullTask, ""},
		{"issue", issueTask, endTestServed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ag := state.Agent{Party: state.Party{Name: loopWorker}, Task: tc.task}
			if got := loopTaskServes(ag, ""); got != tc.want {
				t.Errorf("loopTaskServes = %q, want %q", got, tc.want)
			}
			st := &state.State{Agents: []state.Agent{ag}}
			if got := reviewParkServes(st, ag, loopPR); got != tc.want {
				t.Errorf("reviewParkServes = %q, want %q", got, tc.want)
			}
		})
	}
	// The park's own reference is no task of it.
	ag := state.Agent{Party: state.Party{Name: loopWorker}, Task: "fix https://github.com/o/r/issues/15"}
	if got := reviewParkServes(&state.State{Agents: []state.Agent{ag}}, ag, endTestServed); got != "" {
		t.Errorf("reviewParkServes of its own reference = %q", got)
	}
}

// A pass cut short by the allowance hold says nothing of the pull requests it
// did not read: an ownerless stretch must not end on that.
func TestHeldPassKeepsOwnerlessStretchOfUnreadPull(t *testing.T) {
	r := newLoopRig(t)
	const orphan = "o/r#9"
	r.pulls[orphan] = superloop.Pull{Ref: orphan, URL: "https://github.com/o/r/pull/9", State: superloop.Open, Head: loopHead, Green: true, Author: loopDesk, Branch: "fix-9"}
	listed := readOpenPulls
	t.Cleanup(func() { readOpenPulls = listed })
	readOpenPulls = func(context.Context, func() ([]string, error), []string) ([]string, error) {
		return []string{orphan}, nil
	}
	r.pass(t)
	stretches := func() []state.Event {
		return slices.DeleteFunc(r.logged(t, loopOwnerlessVerb), func(e state.Event) bool { return e.Subject != orphan })
	}
	if evs := stretches(); len(evs) != 1 {
		t.Fatalf("stretch did not start: %+v", evs)
	}
	r.park(t, loopPR, time.Now())
	prev := readLoopPull
	readLoopPull = func(ctx context.Context, ref string) (superloop.Pull, error) {
		if ref == loopPR {
			// The active pull request is read; the hold starts right after.
			return prev(ctx, ref)
		}
		return superloop.Pull{}, errLoopLater
	}
	r.pass(t)
	if evs := stretches(); len(evs) != 1 {
		t.Fatalf("a held pass ended the stretch of a pull request it did not read: %+v", evs)
	}
}

// Live review requests come first, saved ones next, then pull requests with a
// loop round, then the rest; newest change first within each.
func TestSortTrackedPullsOrdersActiveRequestsFirst(t *testing.T) {
	now := time.Now()
	in := superloop.Input{
		Workers: []superloop.Worker{
			{Name: "saved old", ParkedOn: sweepRef(2), ParkedAt: now.Add(-3 * time.Hour), ReviewRequested: true, Saved: true},
			{Name: "saved new", ParkedOn: sweepRef(3), ParkedAt: now.Add(-time.Hour), ReviewRequested: true, Saved: true},
			{Name: "live", ParkedOn: sweepRef(4), ParkedAt: now.Add(-5 * time.Hour), ReviewRequested: true},
			{Name: "plain", ParkedOn: sweepRef(1), ParkedAt: now},
		},
		Taken: []superloop.Taken{{Kind: superloop.KindReview, Ref: sweepRef(5), At: now.Add(-6 * time.Hour)}},
	}
	tracked := []string{sweepRef(1), sweepRef(2), sweepRef(3), sweepRef(4), sweepRef(5)}
	sortTrackedPulls(in, tracked)
	if want := []string{sweepRef(4), sweepRef(3), sweepRef(2), sweepRef(5), sweepRef(1)}; !slices.Equal(tracked, want) {
		t.Fatalf("order = %v, want %v", tracked, want)
	}
}
