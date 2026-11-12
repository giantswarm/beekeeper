package cmd

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	stallNoRoom = "no room for the review"
	stallHead   = "0123abcd"
	// stallURL is the pull request of epicTen, stallHold a hold a merge
	// decision's run read.
	stallURL  = "https://github.com/o/r/pull/10"
	stallHold = "it is held by hold:o/r"
)

// failingStartRig is the refill rig whose board always offers an item and
// whose agents start fails while fail is set, counting the board reads.
func failingStartRig(t *testing.T, doneN int) (*loopRig, *loop.Loop, *int, *bool) {
	t.Helper()
	r, _ := fillRig(t)
	original := r.answer
	reads, fail := 0, true
	r.answer = func(args []string) (string, int) {
		if args[0] == loop.LoopBoard {
			reads++
		}
		if fail && args[0] == agentsName && args[1] == agentStartName {
			return "beekeeper: start refused", 1
		}
		return original(args)
	}
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.Once = true
	var evs []state.Event
	for i := range doneN {
		name := fmt.Sprintf("worker %d", i+1)
		e := event(state.Party{Name: name}, "agents.done", "%s done: fix it", name)
		e.At = e.At.Add(time.Duration(i) * time.Microsecond)
		evs = append(evs, e)
	}
	if err := r.a.store.Record(evs...); err != nil {
		t.Fatal(err)
	}
	return r, l, &reads, &fail
}

// A backlog of done events whose starts cannot go through walks the board
// once in a pass, not once for each, and the refill still runs after it
// (https://github.com/giantswarm/beekeeper/issues/3388).
func TestLoopNextBacklogStopsAtTheFirstUnsettledStart(t *testing.T) {
	const n = 20
	r, l, reads, _ := failingStartRig(t, n)
	r.loopPass(t, l) // the done events take their agents off the roster
	before := *reads
	r.loopPass(t, l)
	if got := *reads - before; got > 2 {
		t.Fatalf("%d pending next actions read the board %d times in one pass, want at most 2 (one next action, the refill)", n, got)
	}
	l.Mu.Lock()
	ran := l.Stall.Ran
	l.Mu.Unlock()
	if ran.IsZero() {
		t.Fatal("the refill did not run behind the next backlog")
	}
}

// Starts that do not move raise the alarm through the file channel after
// loop.LoopStallAfter, repeat it, and say its end once a worker starts.
func TestLoopAlarmsWhenWorkerStartsStall(t *testing.T) {
	r, l, _, fail := failingStartRig(t, 0)
	stalls := func() []state.Event {
		var out []state.Event
		for _, e := range r.logged(t, "supervisor.file") {
			if strings.Contains(e.Detail, loop.LoopStallKey) {
				out = append(out, e)
			}
		}
		return out
	}
	for range int(loop.LoopStallAfter/(30*time.Second)) - 2 {
		r.loopPass(t, l)
	}
	if got := stalls(); len(got) != 0 {
		t.Fatalf("alarm before %s: %+v", loop.LoopStallAfter, got)
	}
	for range 4 {
		r.loopPass(t, l)
	}
	got := stalls()
	if len(got) != 1 {
		t.Fatalf("%d alarms after %s, want 1:\n%s", len(got), loop.LoopStallAfter, r.out)
	}
	if d := got[0].Detail; !strings.Contains(d, "Worker starts are stalled") || !strings.Contains(d, "https://github.com/o/r/issues/10") || !strings.Contains(d, "start failed") {
		t.Errorf("the alarm does not name the item and its cause: %s", d)
	}
	for range int(loop.LoopStallAfter / (30 * time.Second)) {
		r.loopPass(t, l)
	}
	if got := stalls(); len(got) != 1 {
		t.Fatalf("%d alarms after 2×%s, want only the original alarm", len(got), loop.LoopStallAfter)
	}
	*fail = false
	// The refill's pause after the failed reads ends within its backoff.
	for range int(4 * r.a.cfg.Supervisor.FillRetryEvery.Duration / (30 * time.Second)) {
		r.loopPass(t, l)
	}
	if !strings.Contains(r.out.String(), "SUPERVISOR STALL ENDED") {
		t.Fatalf("the stall's end is not said:\n%s", r.out)
	}
}

func TestLoopAlarmsWhenTheRefillDoesNotRun(t *testing.T) {
	r, _ := fillRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.CheckStall(); err != nil {
		t.Fatal(err)
	}
	r.a.now = r.a.now.Add(loop.LoopStallAfter)
	if err := l.CheckStall(); err != nil {
		t.Fatal(err)
	}
	if err := l.CheckStall(); err != nil {
		t.Fatal(err)
	}
	evs := r.logged(t, "supervisor.file")
	if len(evs) != 1 || !strings.Contains(evs[0].Detail, "the capacity refill has not run since") {
		t.Fatalf("want one alarm on a refill that does not run: %+v", evs)
	}
	l.FillRan()
	if !strings.Contains(r.out.String(), "SUPERVISOR STALL ENDED") {
		t.Fatalf("the stall's end is not said:\n%s", r.out)
	}
}

// A review request whose start keeps waiting raises the alarm after
// loop.LoopStallAfter, names the pull request, repeats, and says its end once
// the request is no longer planned (https://github.com/giantswarm/beekeeper/issues/3388).
func TestLoopAlarmsWhenReviewStartsWait(t *testing.T) {
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.Once = true
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	r.answer = func(args []string) (string, int) {
		if len(args) > 1 && args[0] == agentsName && args[1] == agentStartName {
			return stallNoRoom, ExitRefused
		}
		return "", 0
	}
	stalls := func() []state.Event {
		var out []state.Event
		for _, e := range r.logged(t, "supervisor.file") {
			if strings.Contains(e.Detail, loop.LoopReviewStallKey) {
				out = append(out, e)
			}
		}
		return out
	}
	acts := []superloop.Action{{Kind: superloop.KindReview, Key: "review waits", Ref: loopPR, URL: loopURL, Head: loopHead, Round: 1}}
	pass := func(acts []superloop.Action) {
		t.Helper()
		if err := l.RunPassActions(t.Context(), st, nil, nil, nil, acts); err != nil {
			t.Fatal(err)
		}
		if err := l.CheckStall(); err != nil {
			t.Fatal(err)
		}
		r.a.now = r.a.now.Add(time.Minute)
	}
	for range int(loop.LoopStallAfter / time.Minute) {
		pass(acts)
	}
	if got := stalls(); len(got) != 0 {
		t.Fatalf("alarm before %s: %+v", loop.LoopStallAfter, got)
	}
	pass(acts)
	got := stalls()
	if len(got) != 1 {
		t.Fatalf("%d alarms after %s, want 1:\n%s", len(got), loop.LoopStallAfter, r.out)
	}
	if d := got[0].Detail; !strings.Contains(d, "Reviews are stalled") || !strings.Contains(d, loopURL) || !strings.Contains(d, "its start waits") {
		t.Errorf("the alarm does not name the pull request and its cause: %s", d)
	}
	for range int(loop.LoopStallAfter / time.Minute) {
		pass(acts)
	}
	if got := stalls(); len(got) != 1 {
		t.Fatalf("%d alarms after 2×%s, want only the original alarm", len(got), loop.LoopStallAfter)
	}
	pass(nil)
	if !strings.Contains(r.out.String(), "SUPERVISOR STALL ENDED: no review request waits") {
		t.Fatalf("the stall's end is not said:\n%s", r.out)
	}
}

// Progress in the refill does not excuse a review or merge decision that
// never settles. Each lane repeats its alarm, and an unread observation
// must not reset the wait.
func TestLoopAlarmsOnPendingMergeDecisions(t *testing.T) {
	for _, kind := range []string{superloop.KindGo} {
		t.Run(kind, func(t *testing.T) {
			r, _ := fillRig(t)
			l, err := r.a.newSupervisorLoop(false)
			if err != nil {
				t.Fatal(err)
			}
			act := superloop.Action{Kind: kind, Key: kind + " " + epicTen + "@" + stallHead, Ref: epicTen, URL: stallURL, Head: stallHead}
			in := superloop.Input{Pulls: map[string]superloop.Pull{act.Ref: {Head: act.Head, Green: true, State: superloop.Open}}}
			failures := map[string]loop.LoopFailure{act.Key: {Message: "specific command failure"}}
			check := func(input superloop.Input, actions []superloop.Action) {
				t.Helper()
				if err := l.CheckFlowWaits(input, actions, failures); err != nil {
					t.Fatal(err)
				}
			}
			check(in, []superloop.Action{act})
			r.a.now = r.a.now.Add(loop.LoopStallAfter - time.Second)
			check(in, []superloop.Action{act})
			if got := r.logged(t, "supervisor.file"); len(got) != 0 {
				t.Fatalf("early alarm: %+v", got)
			}
			r.a.now = r.a.now.Add(time.Second)
			l.FillRan()
			check(in, []superloop.Action{act})
			check(in, []superloop.Action{act})
			got := r.logged(t, "supervisor.file")
			if len(got) != 1 || !strings.Contains(got[0].Detail, act.URL) || !strings.Contains(got[0].Detail, "specific command failure") {
				t.Fatalf("want one alarm with URL and observed cause: %+v", got)
			}
			check(superloop.Input{}, nil)
			r.a.now = r.a.now.Add(loop.LoopStallAfter)
			check(in, []superloop.Action{act})
			if got := r.logged(t, "supervisor.file"); len(got) != 1 {
				t.Fatalf("unread observation must retain the original alarm: %+v", got)
			}
			check(in, nil)
			if len(l.FlowWaits) != 0 || !strings.Contains(r.out.String(), "SUPERVISOR STALL ENDED") {
				t.Fatalf("settled decision did not clear: %+v\n%s", l.FlowWaits, r.out)
			}
		})
	}
}

func TestLoopSettlesOvertakenMergeDecisionsBeforeAlarm(t *testing.T) {
	for _, tc := range []struct {
		name string
		pull superloop.Pull
	}{
		{"merged PR", superloop.Pull{State: superloop.Merged, Head: stallHead}},
		{"closed PR", superloop.Pull{State: superloop.Closed, Head: stallHead}},
		{"superseded", superloop.Pull{State: superloop.Open, Head: stallHead + "-new"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := fillRig(t)
			l, err := r.a.newSupervisorLoop(false)
			if err != nil {
				t.Fatal(err)
			}
			act := superloop.Action{Kind: superloop.KindGo, Key: "go " + epicTen + "@" + stallHead, Ref: epicTen, Head: stallHead}
			in := superloop.Input{Pulls: map[string]superloop.Pull{epicTen: {State: superloop.Open, Head: stallHead}}}
			if err := l.CheckFlowWaits(in, []superloop.Action{act}, nil); err != nil {
				t.Fatal(err)
			}
			r.a.now = r.a.now.Add(loop.LoopStallAfter)
			in.Pulls[epicTen] = tc.pull
			// A stale action from an earlier observation must not keep the
			// episode alive or file an alarm at the deadline.
			if err := l.CheckFlowWaits(in, []superloop.Action{act}, nil); err != nil {
				t.Fatal(err)
			}
			if len(l.FlowWaits) != 0 || len(r.logged(t, "supervisor.file")) != 0 {
				t.Fatalf("overtaken merge decision remains pending: %+v", l.FlowWaits)
			}
		})
	}
}

// stallAlarms are the merge decision alarms filed so far, oldest first.
func stallAlarms(t *testing.T, r *loopRig) []string {
	t.Helper()
	var out []string
	for _, e := range r.logged(t, "supervisor.file") {
		if strings.Contains(e.Detail, "stall decision ") {
			out = append(out, e.Detail)
		}
	}
	return out
}

// The alarm names what holds a pending merge decision, and names it again
// when the hold changes: once per pending head and class of hold, no sooner
// than loop.LoopStallAfter after the head's last alarm
// (https://github.com/giantswarm/beekeeper/issues/3820).
func TestLoopMergeDecisionAlarmNamesEachHoldOnce(t *testing.T) {
	r, _ := fillRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	const otherURL = "https://github.com/o/r/pull/11"
	const laneClosed = "merge gate refused: the lane is closed"
	act := superloop.Action{Kind: superloop.KindGo, Key: superloop.KindGo + " " + epicTen + "@" + stallHead, Ref: epicTen, URL: stallURL, Head: stallHead}
	other := superloop.Action{Kind: superloop.KindGo, Key: superloop.KindGo + " " + planningPull + "@" + stallHead, Ref: planningPull, URL: otherURL, Head: stallHead}
	in := superloop.Input{Pulls: map[string]superloop.Pull{act.Ref: {Head: act.Head, Green: true, State: superloop.Open}}}
	failures := map[string]loop.LoopFailure{}
	check := func(input superloop.Input, actions ...superloop.Action) {
		t.Helper()
		if err := l.CheckFlowWaits(input, actions, failures); err != nil {
			t.Fatal(err)
		}
	}
	want := func(n int, names ...string) {
		t.Helper()
		got := stallAlarms(t, r)
		if len(got) != n {
			t.Fatalf("%d alarms, want %d:\n%s", len(got), n, strings.Join(got, "\n"))
		}
		for _, name := range names {
			if !strings.Contains(got[n-1], name) {
				t.Fatalf("the alarm does not name %q: %s", name, got[n-1])
			}
		}
	}
	check(in, act)
	l.MergeWaited(act, loop.MergeHeld(loop.WaitHold, "%s", stallHold))
	r.a.now = r.a.now.Add(loop.LoopStallAfter)
	check(in, act)
	want(1, act.URL, act.Head, stallHold)

	// Its command starts: a changed hold is named, but not before
	// loop.LoopStallAfter passed since the head's last alarm.
	if !l.BeginInFlight(act.Key) {
		t.Fatal("the decision was in flight already")
	}
	l.MergeStarted([]superloop.Action{act}, 0)
	r.a.now = r.a.now.Add(time.Minute)
	check(in, act)
	want(1)
	r.a.now = r.a.now.Add(loop.LoopStallAfter)
	check(in, act)
	want(2, "its merge command has been in flight since")

	// Another pull request's command runs ahead of it in its lane's stream.
	l.MergeStarted([]superloop.Action{other, act}, 0)
	r.a.now = r.a.now.Add(loop.LoopStallAfter)
	check(in, act)
	want(3, "behind "+otherURL)
	l.MergeStreamEnded([]superloop.Action{other, act})
	l.EndInFlight(act.Key)

	// The first hold again: its class was named for this head.
	r.a.now = r.a.now.Add(loop.LoopStallAfter)
	check(in, act)
	want(3)

	// No observation of the pull request: nothing plans the decision.
	r.a.now = r.a.now.Add(loop.LoopStallAfter)
	check(superloop.Input{})
	want(4, "no current observation of "+act.Ref)

	// A failure newer than the wait is the cause.
	failures[act.Key] = loop.LoopFailure{Message: laneClosed, At: r.a.now}
	r.a.now = r.a.now.Add(loop.LoopStallAfter)
	check(in, act)
	want(5, "its last run failed: "+laneClosed)

	check(in)
	if len(l.FlowWaits) != 0 || len(l.MergeNotes) != 0 || !strings.Contains(r.out.String(), "SUPERVISOR STALL ENDED") {
		t.Fatalf("the settled decision did not clear: %+v %+v\n%s", l.FlowWaits, l.MergeNotes, r.out)
	}
}

// The stall of 2026-11-12: the merge gate failed, the decision waited in its
// repair stage for a worker that capacity did not start, and held GitHub
// observations ended the passes before planning. Neither wait completes the
// decision or records a failure, so each must be named by the alarm instead
// of "no successful completion recorded"
// (https://github.com/giantswarm/beekeeper/issues/3820).
func TestLoopNamesAMergeRepairWaitingForAWorkerAndHeldPasses(t *testing.T) {
	r, l, _ := emptyBoardFill(t)
	fill := r.answer
	r.answer = func(args []string) (string, int) {
		if len(args) > 1 && args[0] == agentsName && args[1] == agentStartName {
			return "beekeeper: no start: no choice has room", ExitRefused
		}
		return fill(args)
	}
	act := superloop.Action{Kind: superloop.KindGo, Key: superloop.KindGo + " " + loopPR + "@" + loopHead, Ref: loopPR, URL: loopURL, Head: loopHead, Agent: loopWorker}
	// The gate's merge-result check failed on an earlier pass: the repair
	// stage stands, and the worker left the roster with its review park.
	l.Mu.Lock()
	l.Stages = map[string]bool{act.Key + " repair": true}
	l.StageDetail = map[string]string{act.Key + " repair": "(exit 1): merge result (main + " + shortSHA(loopHead) + ") failed go test: --- FAIL: TestOfTheHost"}
	l.Mu.Unlock()
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	in := superloop.Input{Pulls: map[string]superloop.Pull{loopPR: r.pull}}
	failures := map[string]loop.LoopFailure{}
	pass := func() {
		t.Helper()
		if err := l.CheckFlowWaits(in, []superloop.Action{act}, failures); err != nil {
			t.Fatal(err)
		}
		if err := l.ExecAction(t.Context(), st, nil, failures, act); err != nil {
			t.Fatal(err)
		}
	}
	pass()
	if got := stallAlarms(t, r); len(got) != 0 {
		t.Fatalf("alarm before %s: %v", loop.LoopStallAfter, got)
	}
	if len(r.logged(t, "supervisor.merge")) != 0 || len(r.logged(t, loop.LoopFailureVerb)) != 0 {
		t.Fatalf("a repair waiting for its worker completed or failed the decision:\n%s", r.out)
	}
	r.a.now = r.a.now.Add(loop.LoopStallAfter)
	pass()
	got := stallAlarms(t, r)
	if len(got) != 1 {
		t.Fatalf("%d alarms after %s, want 1:\n%s", len(got), loop.LoopStallAfter, r.out)
	}
	for _, name := range []string{loopURL, loopHead, "merge result (main + ", "the branch repair it requested waits for a worker", "no choice has room"} {
		if !strings.Contains(got[0], name) {
			t.Errorf("the alarm does not name %q: %s", name, got[0])
		}
	}
	if strings.Contains(got[0], "no successful completion recorded") {
		t.Errorf("the alarm names no hold: %s", got[0])
	}
	r.a.now = r.a.now.Add(loop.LoopStallAfter)
	pass()
	if got := stallAlarms(t, r); len(got) != 1 {
		t.Fatalf("the same hold was alarmed again: %v", got)
	}

	// GitHub observations are held: every pass ends before it plans, and
	// the pending decision is still measured, with the hold as its cause.
	r.a.cfg.GitHub.Floor = 2500
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		now := r.a.now
		st.Budget = &state.Budget{Remaining: 4000, Limit: 5000, Reset: now.Add(2 * time.Hour), At: now,
			GraphQL: &state.GraphQL{Remaining: 100, Limit: 5000, Reset: now.Add(2 * time.Hour), At: now}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.loopPass(t, l)
	if !strings.Contains(r.out.String(), "SUPERVISOR GITHUB HELD") {
		t.Fatalf("the pass was not held:\n%s", r.out)
	}
	got = stallAlarms(t, r)
	if len(got) != 2 || !strings.Contains(got[1], loopURL) || !strings.Contains(got[1], "github.floor") {
		t.Fatalf("a held pass did not alarm the pending decision with its hold: %v\n%s", got, r.out)
	}
	for range 3 {
		r.loopPass(t, l)
	}
	if got := stallAlarms(t, r); len(got) != 2 {
		t.Fatalf("the held passes repeated the alarm: %v", got)
	}
}

// A restart keeps the wait: its clock, the holds already named and the hold
// its last run read. A new release must not silence a pending decision for
// another loop.LoopStallAfter, nor repeat its alarm.
func TestLoopKeepsMergeDecisionWaitsAcrossARestart(t *testing.T) {
	r, _ := fillRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	act := superloop.Action{Kind: superloop.KindGo, Key: superloop.KindGo + " " + epicTen + "@" + stallHead, Ref: epicTen, URL: stallURL, Head: stallHead}
	in := superloop.Input{Pulls: map[string]superloop.Pull{act.Ref: {Head: act.Head, Green: true, State: superloop.Open}}}
	if err := l.CheckFlowWaits(in, []superloop.Action{act}, nil); err != nil {
		t.Fatal(err)
	}
	since := l.FlowWaits[act.Key].Since
	l.MergeWaited(act, loop.MergeHeld(loop.WaitHold, "%s", stallHold))
	r.a.now = r.a.now.Add(loop.LoopStallAfter - time.Minute)
	l.SaveCache()

	restarted, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	restarted.LoadCache()
	if got := restarted.FlowWaits[act.Key]; !got.Since.Equal(since) || got.Act.URL != act.URL || got.Act.Head != act.Head {
		t.Fatalf("the restart lost the wait: %+v, want since %s", got, since)
	}
	r.a.now = r.a.now.Add(time.Minute)
	if err := restarted.CheckFlowWaits(in, []superloop.Action{act}, nil); err != nil {
		t.Fatal(err)
	}
	got := stallAlarms(t, r)
	if len(got) != 1 || !strings.Contains(got[0], stallHold) || !strings.Contains(got[0], since.UTC().Format(time.RFC3339)) {
		t.Fatalf("the restarted loop did not alarm on the kept wait and its hold: %v", got)
	}
	restarted.SaveCache()
	again, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	again.LoadCache()
	r.a.now = r.a.now.Add(2 * loop.LoopStallAfter)
	if err := again.CheckFlowWaits(in, []superloop.Action{act}, nil); err != nil {
		t.Fatal(err)
	}
	if got := stallAlarms(t, r); len(got) != 1 {
		t.Fatalf("a restart repeated the alarm of a named hold: %v", got)
	}
}

// A claimed merge whose pull request cannot be read yet waits for the next
// pass with that cause: it neither fails the pass (which ends the loop) nor
// waits unnamed.
func TestLoopClaimedMergeWithAnUnreadPullWaitsNamed(t *testing.T) {
	r, _ := fillRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	loop.ReadObservedLoopPull = func(*loop.Loop, context.Context, string, time.Time, string) (loop.LoopPullObservation, error) {
		return loop.LoopPullObservation{}, fmt.Errorf("%s observation is 19m43s old (limit 5m0s): %w", loopPR, loop.ErrLoopLater)
	}
	act := superloop.Action{Kind: superloop.KindGo, Key: superloop.KindGo + " " + loopPR + "@" + loopHead, Ref: loopPR, URL: loopURL, Head: loopHead, Agent: loopWorker}
	claims := map[string]loop.LoopClaim{act.Key: {At: r.a.now, Target: loopWorker}}
	if err := l.ExecAction(t.Context(), st, claims, map[string]loop.LoopFailure{}, act); err != nil {
		t.Fatalf("an unread pull request failed the pass: %v", err)
	}
	l.Mu.Lock()
	note := l.MergeNotes[act.Key]
	l.Mu.Unlock()
	if note.Class != loop.WaitUnread || !strings.Contains(note.Cause, "observation is 19m43s old") {
		t.Fatalf("the wait is not named: %+v", note)
	}
	if r.called(gateCmdName) != nil || len(r.logged(t, loop.LoopFailureVerb)) != 0 {
		t.Fatalf("an unread claim ran the gate or recorded a failure: %v\n%s", r.calls, r.out)
	}
}
