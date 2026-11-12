package cmd

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	staleNextVerb  = "supervisor." + superloop.KindNext
	staleNextFresh = "next fresh"
	staleNextAgent = "fresh worker"
)

// staleNextLoop is a loop on the fill rig (ceiling 3) that runs each pass
// to its end.
func staleNextLoop(t *testing.T, r *loopRig) *loop.Loop {
	t.Helper()
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.Once = true
	return l
}

// freshNext is a next action for a done event that just happened.
func freshNext(r *loopRig) superloop.Action {
	return superloop.Action{Kind: superloop.KindNext, Key: staleNextFresh, Agent: staleNextAgent, Event: &superloop.Event{At: r.a.timeNow(), Verb: executionDone, Agent: staleNextAgent}}
}

// nextKeyed are the logged next lines for done events (not the refill's).
func nextKeyed(t *testing.T, r *loopRig) []state.Event {
	t.Helper()
	var out []state.Event
	for _, e := range r.logged(t, staleNextVerb) {
		if strings.HasPrefix(e.Detail, "[next agents.done@") {
			out = append(out, e)
		}
	}
	return out
}

// A backlog of hundreds of done events hours old settles in one write as
// superseded, without a board read for each, and the refill fills every free
// slot in the same pass; the next plan holds none of them
// (https://github.com/giantswarm/beekeeper/issues/3485).
func TestLoopStaleNextBacklogSettlesAndFillsInTheSamePass(t *testing.T) {
	const n = 300
	r, busy := fillRig(t)
	reads := 0
	original := r.answer
	r.answer = func(args []string) (string, int) {
		if args[0] == loop.LoopBoard {
			reads++
		}
		return original(args)
	}
	*busy = 3 // no room while the done events take their agents off the roster
	l := staleNextLoop(t, r)
	var evs []state.Event
	for i := range n {
		name := fmt.Sprintf("worker %d", i+1)
		e := event(state.Party{Name: name}, executionDone, "%s done: fix it", name)
		e.At = e.At.Add(time.Duration(i) * time.Microsecond)
		evs = append(evs, e)
	}
	if err := r.a.store.Record(evs...); err != nil {
		t.Fatal(err)
	}
	r.loopPass(t, l)

	*busy = 0
	r.a.now = r.a.now.Add(2 * loop.StaleNextAfter)
	before := reads
	r.loopPass(t, l)
	if *busy != 3 {
		t.Fatalf("the refill filled %d of 3 free slots behind the stale backlog:\n%s", *busy, r.out)
	}
	if got := reads - before; got > 4 {
		t.Fatalf("the pass read the board %d times, want only the refill's", got)
	}
	settled := nextKeyed(t, r)
	if len(settled) != n {
		t.Fatalf("%d of %d stale next actions settled:\n%s", len(settled), n, r.out)
	}
	for _, e := range settled {
		if !strings.Contains(e.Detail, loop.StaleNextWhat) {
			t.Fatalf("a stale next action started instead of settling: %s", e.Detail)
		}
	}
	if got := strings.Count(r.out.String(), "SUPERVISOR NEXT SKIPPED"); got != 1 {
		t.Fatalf("the skip is said %d times, want once:\n%s", got, r.out)
	}

	// A new loop, with nothing remembered: the plan reads the settled keys
	// back as taken and holds none of them.
	l2 := staleNextLoop(t, r)
	r.loopPass(t, l2)
	if got := len(nextKeyed(t, r)); got != len(settled) {
		t.Fatalf("the next plan held settled stale actions again: %d lines, want %d", got, len(settled))
	}
}

// A next action that fails does not keep the refill from filling the room.
func TestLoopFailingNextDoesNotStopTheFill(t *testing.T) {
	r, busy := fillRig(t)
	l := staleNextLoop(t, r)
	r.failRecord = func(e state.Event) bool {
		return e.Verb == staleNextVerb && strings.HasPrefix(e.Detail, "["+staleNextFresh+"]")
	}
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	failures := map[string]loop.LoopFailure{}
	if err := l.RunPassActions(t.Context(), st, nil, failures, nil, []superloop.Action{freshNext(r)}); err != nil {
		t.Fatalf("a next action's failure left the stream: %v", err)
	}
	if *busy != 3 {
		t.Fatalf("the refill filled %d of 3 slots after a failing next action:\n%s", *busy, r.out)
	}
	if _, ok := failures[staleNextFresh]; !ok {
		t.Fatalf("the next action's failure is not recorded:\n%s", r.out)
	}
}

// An intake still in flight from an earlier pass skips only the intake: the
// next action and the refill run.
func TestLoopIntakeInFlightDoesNotStopNextOrFill(t *testing.T) {
	r, busy := fillRig(t)
	l := staleNextLoop(t, r)
	if !l.BeginInFlight("stream:intake") {
		t.Fatal("intake already in flight")
	}
	defer l.EndInFlight("stream:intake")
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	acts := []superloop.Action{
		{Kind: superloop.KindDone, Key: "done retired", Agent: "retired"},
		{Kind: superloop.KindIntake, Key: "intake o/r#98", Ref: "o/r#98", URL: "https://github.com/o/r/issues/98"},
		freshNext(r),
	}
	if err := l.RunPassActions(t.Context(), st, nil, map[string]loop.LoopFailure{}, nil, acts); err != nil {
		t.Fatal(err)
	}
	if !l.IsCompleted(staleNextFresh) {
		t.Fatalf("the next action did not run behind an intake in flight:\n%s", r.out)
	}
	if *busy != 3 {
		t.Fatalf("the refill filled %d of 3 slots behind an intake in flight:\n%s", *busy, r.out)
	}
	if l.IsCompleted("intake o/r#98") {
		t.Fatal("the intake ran beside the one in flight")
	}
}

// A start refused because the item already has an open pull request settles
// its next action: the delivery is in flight.
func TestLoopNextSettlesOnAnOpenPullRequestRefusal(t *testing.T) {
	r, _ := fillRig(t)
	original := r.answer
	r.answer = func(args []string) (string, int) {
		if len(args) > 2 && args[0] == agentsName && args[1] == agentStartName && args[2] == "r 10" {
			return "beekeeper: o/r#10 " + loop.OpenPullRefusal + " https://github.com/o/r/pull/11; update it instead", ExitRefused
		}
		return original(args)
	}
	l := staleNextLoop(t, r)
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	failures := map[string]loop.LoopFailure{}
	if err := l.RunPassActions(t.Context(), st, nil, failures, nil, []superloop.Action{freshNext(r)}); err != nil {
		t.Fatal(err)
	}
	if !l.IsCompleted(staleNextFresh) {
		t.Fatalf("the refused next action did not settle:\n%s", r.out)
	}
	if _, ok := failures[staleNextFresh]; ok {
		t.Fatal("the refusal was recorded as a failure")
	}
	var found bool
	for _, e := range r.logged(t, staleNextVerb) {
		if strings.HasPrefix(e.Detail, "["+staleNextFresh+"]") && strings.Contains(e.Detail, "superseded by the capacity refill") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the next action was not delegated to refill:\n%s", r.out)
	}
}

// A previous pass can remain blocked on intake or next bookkeeping. Those
// streams must not prevent this pass from filling newly available capacity.
func TestLoopFillWhileNextAndIntakeAreInFlight(t *testing.T) {
	r, busy := fillRig(t)
	l := staleNextLoop(t, r)
	for _, key := range []string{"stream:next", "stream:intake", "stream:worker-retirement"} {
		if !l.BeginInFlight(key) {
			t.Fatalf("stream already reserved: %s", key)
		}
		defer l.EndInFlight(key)
	}
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	acts := []superloop.Action{
		freshNext(r),
		{Kind: superloop.KindDone, Key: "done retired", Agent: staleNextAgent},
		{Kind: superloop.KindIntake, Key: "intake o/r#98", Ref: "o/r#98"},
	}
	if err := l.RunPassActions(t.Context(), st, nil, nil, nil, acts); err != nil {
		t.Fatal(err)
	}
	if *busy != 3 {
		t.Fatalf("filled %d of 3 slots while earlier streams remain busy: %s", *busy, r.out)
	}
	if l.IsCompleted(staleNextFresh) || l.IsCompleted("intake o/r#98") {
		t.Fatal("executed an already reserved stream")
	}
}
