package merge

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// Ten merges one session queued, each in a lane of its own, poll GitHub as
// two would: at most two run (each running devctl polls), the other eight
// wait without reading it, and all ten start in the order they queued,
// whichever order their gates ask in. Another session's merge is not held
// by them.
func TestTenQueuedMergesPollAsTwo(t *testing.T) {
	now := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	sweep := state.Party{Session: "s1", Name: "sweep"}
	other := state.Party{Session: "s2", Name: "elsewhere"}
	st := &state.State{}
	for i := range 10 {
		st.Merges = append(st.Merges, state.Merge{Repo: fmt.Sprintf("o/r%d", i), PR: 1, Lane: fmt.Sprintf("o/r%d", i), By: sweep,
			PID: 100 + i, Phase: state.Waiting, Joined: now.Add(time.Duration(i) * time.Second)})
	}
	st.Merges = append(st.Merges, state.Merge{Repo: "o/x", PR: 1, Lane: "o/x", By: other, PID: 200, Phase: state.Waiting, Joined: now.Add(time.Minute)})
	alive := func(pid int) bool { return pid != 0 }
	present := func(m state.Merge) bool { return alive(m.PID) }
	turn := func(m state.Merge) bool {
		_, _, ok := SessionTurn(st, m, 2, present, alive)
		return ok
	}
	if !turn(st.Merges[10]) {
		t.Fatalf("another session's merge waits on the sweep's")
	}
	var started []string
	for tick := 0; len(started) < 10; tick++ {
		if tick > 100 {
			t.Fatalf("the sweep stalls after %v", started)
		}
		// Each waiting gate steps, the latest queued first: the order the
		// gates ask in is not the order they start in. Those starting in
		// one tick are taken in queue order.
		var tickStarted []int
		for i := 9; i >= 0; i-- {
			m := &st.Merges[i]
			if m.Phase == state.Waiting && turn(*m) {
				m.Phase, m.Started = state.Running, now
				tickStarted = append(tickStarted, i)
			}
		}
		slices.Sort(tickStarted)
		for _, i := range tickStarted {
			started = append(started, st.Merges[i].Key())
		}
		polling := 0
		for _, m := range st.Merges[:10] {
			if m.Phase == state.Running {
				polling++
			}
		}
		if polling > 2 {
			t.Fatalf("%d of the sweep's merges poll GitHub at once, want at most 2", polling)
		}
		// The oldest running merge ends: its devctl and gate are gone.
		if i := slices.IndexFunc(st.Merges[:10], func(m state.Merge) bool { return m.Phase == state.Running }); i >= 0 {
			st.Merges[i].Phase, st.Merges[i].PID = state.Settling, 0
		}
	}
	want := make([]string, 10)
	for i := range want {
		want[i] = fmt.Sprintf("o/r%d#1", i)
	}
	if !slices.Equal(started, want) {
		t.Errorf("started %v, want the queue order %v", started, want)
	}
}

// A merge of the session queued earlier that cannot start (behind another
// session's merge in its lane, or its lane settling) does not keep a free
// slot from a later one.
func TestSessionTurnSkipsAnEarlierMergeThatCannotStart(t *testing.T) {
	now := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	sweep := state.Party{Session: "s1", Name: "sweep"}
	other := state.Party{Session: "s2", Name: "elsewhere"}
	st := &state.State{Merges: []state.Merge{
		{Repo: "o/p", PR: 1, Lane: "a", By: other, PID: 1, Phase: state.Waiting, Joined: now},
		{Repo: "o/p", PR: 2, Lane: "a", By: sweep, PID: 2, Phase: state.Waiting, Joined: now.Add(time.Second)},
		{Repo: "o/q", PR: 1, Lane: "b", By: sweep, PID: 3, Phase: state.Settling, Joined: now},
		{Repo: "o/q", PR: 2, Lane: "b", By: sweep, PID: 4, Phase: state.Waiting, Joined: now.Add(2 * time.Second)},
		{Repo: "o/s", PR: 1, Lane: "c", By: sweep, PID: 5, Phase: state.Running, Joined: now},
		{Repo: "o/t", PR: 1, Lane: "d", By: sweep, PID: 6, Phase: state.Waiting, Joined: now.Add(3 * time.Second)},
	}}
	alive := func(int) bool { return true }
	running, first, ok := SessionTurn(st, st.Merges[5], 2, presentIf(alive), alive)
	if !ok || first != nil || len(running) != 1 {
		t.Errorf("o/t#1 does not start in the free slot: running %v, first %v, ok %v", running, first, ok)
	}
	// With o/p's lane free, o/p#2 queued first takes the one free slot.
	st.Merges[0].Phase = state.Settling
	st.Merges[0].Lane = "gone"
	_, first, ok = SessionTurn(st, st.Merges[5], 2, presentIf(alive), alive)
	if ok || first == nil || first.Key() != "o/p#2" {
		t.Errorf("o/t#1 does not yield to o/p#2: first %v, ok %v", first, ok)
	}
	if _, _, ok := SessionTurn(st, st.Merges[5], -1, presentIf(alive), alive); !ok {
		t.Errorf("a negative cap waits")
	}
}

// presentIf is Present by the gate's pid alone.
func presentIf(alive func(int) bool) func(state.Merge) bool {
	return func(m state.Merge) bool { return alive(m.PID) }
}
