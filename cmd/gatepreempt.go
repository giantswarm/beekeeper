package cmd

import (
	"fmt"
	"sync"
	"time"

	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
)

// preemptPoll is how often a merge's gate reads the holds while its devctl
// runs.
var preemptPoll = 5 * time.Second

// PreemptedError ends a merge's run that a hold took back out of its lane
// before devctl merged: the merge waits on in its place, why says for what.
type PreemptedError struct{ Why string }

func (e *PreemptedError) Error() string { return e.Why }

// watchPreempt ends the merge's devctl, while it runs, once a hold is set
// that lets another pull request through first (merge.Preempts): devctl is
// still waiting for CI, and merging would put the merge ahead of the
// hold's order. Only the merge's recorded merge-child is ended, as for a
// TaskStop. A promotion, a merge of a central lane, and one whose gate left
// it to run on (its tool limit) are not watched. stop ends the watch and
// returns the hold that ended devctl, if any.
func (g *GateRun) watchPreempt() (stop func() (state.Hold, bool)) {
	var mu sync.Mutex
	var hit *state.Hold
	done := make(chan struct{})
	if g.Pr != 0 && !g.Central {
		go func() {
			for {
				select {
				case <-done:
					return
				case <-clk.After(preemptPoll):
				}
				if h, ok := g.PreemptChild(); ok {
					mu.Lock()
					hit = &h
					mu.Unlock()
					return
				}
			}
		}()
	}
	return func() (state.Hold, bool) {
		close(done)
		mu.Lock()
		defer mu.Unlock()
		if hit == nil {
			return state.Hold{}, false
		}
		return *hit, true
	}
}

// PreemptChild ends the running merge's devctl when a hold preempts it.
func (g *GateRun) PreemptChild() (state.Hold, bool) {
	st, err := g.Host.Store().Read()
	if err != nil {
		return state.Hold{}, false
	}
	i := g.Mine(st, state.Running)
	if i < 0 {
		return state.Hold{}, false
	}
	m := st.Merges[i]
	h, ok := merge.Preempts(st, clk.Now(), m, g.Lane)
	if !ok || m.Child == 0 || Cmd.ReadPID(MergeBase(g.Host.Store().Dir(), g.Repo, g.Pr)) != m.Child {
		return state.Hold{}, false
	}
	GateLine("%s (%s) by %q lets %s through first: ending %s's devctl (pid %d) before it merges; it keeps its place in lane %s",
		Cmd.HoldTarget(h), h.Reason, h.By.Name, h.Except, g.Key(), m.Child, g.Lane.Name)
	if err := Cmd.EndChild(m.Child); err != nil {
		return state.Hold{}, false
	}
	return h, true
}

// Requeue puts a preempted merge back to waiting in its place, unless
// GitHub reports it merged all the same (devctl merged before it ended), or
// does not answer: then its run is recorded as any other.
func (g *GateRun) Requeue(base string, h state.Hold) error {
	p, err := PullState(g.Ctx, g.Repo, g.Pr)
	if err != nil || p.State == github.Merged {
		return nil
	}
	RemoveMergeFiles(base)
	return g.toWaiting(h, "devctl ended before it merged")
}

// toWaiting puts the running merge back to waiting in its place, a hold h
// letting another pull request through first; when says where its run was.
func (g *GateRun) toWaiting(h state.Hold, when string) error {
	why := fmt.Sprintf("held in lane %s (%s) by %q: %s goes first; %s keeps its place", g.Lane.Name, Cmd.HoldTarget(h), h.By.Name, h.Except, g.Key())
	_ = g.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
		i := g.Mine(st, state.Running)
		if i < 0 {
			return nil, nil
		}
		m := &st.Merges[i]
		m.Phase, m.Started, m.Roll, m.Child, m.HandCut = state.Waiting, time.Time{}, nil, 0, ""
		m.Seen = clk.Now().UTC()
		return []state.Event{Cmd.Event(g.Me, "merge.preempted", "%s: %s, %s", g.Key(), when, why)}, nil
	})
	g.Requeued = true
	return &PreemptedError{Why: why}
}
