package cmd

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// failingStartRig is the refill rig whose board always offers an item and
// whose agents start fails while fail is set, counting the board reads.
func failingStartRig(t *testing.T, doneN int) (*loopRig, *supervisorLoop, *int, *bool) {
	t.Helper()
	r, _ := fillRig(t)
	original := r.answer
	reads, fail := 0, true
	r.answer = func(args []string) (string, int) {
		if args[0] == loopBoard {
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
	l.once = true
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
	l.mu.Lock()
	ran := l.stall.ran
	l.mu.Unlock()
	if ran.IsZero() {
		t.Fatal("the refill did not run behind the next backlog")
	}
}

// Starts that do not move raise the alarm through the file channel after
// loopStallAfter, repeat it, and say its end once a worker starts.
func TestLoopAlarmsWhenWorkerStartsStall(t *testing.T) {
	r, l, _, fail := failingStartRig(t, 0)
	stalls := func() []state.Event {
		var out []state.Event
		for _, e := range r.logged(t, "supervisor.file") {
			if strings.Contains(e.Detail, loopStallKey) {
				out = append(out, e)
			}
		}
		return out
	}
	for range int(loopStallAfter/(30*time.Second)) - 2 {
		r.loopPass(t, l)
	}
	if got := stalls(); len(got) != 0 {
		t.Fatalf("alarm before %s: %+v", loopStallAfter, got)
	}
	for range 4 {
		r.loopPass(t, l)
	}
	got := stalls()
	if len(got) != 1 {
		t.Fatalf("%d alarms after %s, want 1:\n%s", len(got), loopStallAfter, r.out)
	}
	if d := got[0].Detail; !strings.Contains(d, "Worker starts are stalled") || !strings.Contains(d, "https://github.com/o/r/issues/10") || !strings.Contains(d, "start failed") {
		t.Errorf("the alarm does not name the item and its cause: %s", d)
	}
	for range int(loopStallAfter / (30 * time.Second)) {
		r.loopPass(t, l)
	}
	if got := stalls(); len(got) != 2 {
		t.Fatalf("%d alarms after 2×%s, want the alarm repeated once", len(got), loopStallAfter)
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
	if err := l.checkStall(); err != nil {
		t.Fatal(err)
	}
	r.a.now = r.a.now.Add(loopStallAfter)
	if err := l.checkStall(); err != nil {
		t.Fatal(err)
	}
	if err := l.checkStall(); err != nil {
		t.Fatal(err)
	}
	evs := r.logged(t, "supervisor.file")
	if len(evs) != 1 || !strings.Contains(evs[0].Detail, "the capacity refill has not run since") {
		t.Fatalf("want one alarm on a refill that does not run: %+v", evs)
	}
	l.fillRan()
	if !strings.Contains(r.out.String(), "SUPERVISOR STALL ENDED") {
		t.Fatalf("the stall's end is not said:\n%s", r.out)
	}
}

// A review request whose start keeps waiting raises the alarm after
// loopStallAfter, names the pull request, repeats, and says its end once
// the request is no longer planned (https://github.com/giantswarm/beekeeper/issues/3388).
func TestLoopAlarmsWhenReviewStartsWait(t *testing.T) {
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.once = true
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	r.answer = func(args []string) (string, int) {
		if len(args) > 1 && args[0] == agentsName && args[1] == agentStartName {
			return "no room", ExitRefused
		}
		return "", 0
	}
	stalls := func() []state.Event {
		var out []state.Event
		for _, e := range r.logged(t, "supervisor.file") {
			if strings.Contains(e.Detail, loopReviewStallKey) {
				out = append(out, e)
			}
		}
		return out
	}
	acts := []superloop.Action{{Kind: superloop.KindReview, Key: "review waits", Ref: loopPR, URL: loopURL, Head: loopHead, Round: 1}}
	pass := func(acts []superloop.Action) {
		t.Helper()
		if err := l.runPassActions(t.Context(), st, nil, nil, nil, acts); err != nil {
			t.Fatal(err)
		}
		if err := l.checkStall(); err != nil {
			t.Fatal(err)
		}
		r.a.now = r.a.now.Add(time.Minute)
	}
	for range int(loopStallAfter / time.Minute) {
		pass(acts)
	}
	if got := stalls(); len(got) != 0 {
		t.Fatalf("alarm before %s: %+v", loopStallAfter, got)
	}
	pass(acts)
	got := stalls()
	if len(got) != 1 {
		t.Fatalf("%d alarms after %s, want 1:\n%s", len(got), loopStallAfter, r.out)
	}
	if d := got[0].Detail; !strings.Contains(d, "Reviews are stalled") || !strings.Contains(d, loopURL) || !strings.Contains(d, "its start waits") {
		t.Errorf("the alarm does not name the pull request and its cause: %s", d)
	}
	for range int(loopStallAfter / time.Minute) {
		pass(acts)
	}
	if got := stalls(); len(got) != 2 {
		t.Fatalf("%d alarms after 2×%s, want the alarm repeated once", len(got), loopStallAfter)
	}
	pass(nil)
	if !strings.Contains(r.out.String(), "SUPERVISOR STALL ENDED: no review request waits") {
		t.Fatalf("the stall's end is not said:\n%s", r.out)
	}
}
