package cmd

import (
	"fmt"
	"sync"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
)

// preemptPoll is how often a merge's gate reads the holds while its devctl
// runs.
var preemptPoll = 5 * time.Second

// preemptedError ends a merge's run that a hold took back out of its lane
// before devctl merged: the merge waits on in its place, why says for what.
type preemptedError struct{ why string }

func (e *preemptedError) Error() string { return e.why }

// watchPreempt ends the merge's devctl, while it runs, once a hold is set
// that lets another pull request through first (merge.Preempts): devctl is
// still waiting for CI, and merging would put the merge ahead of the
// hold's order. Only the merge's recorded merge-child is ended, as for a
// TaskStop. A promotion, a merge of a central lane, and one whose gate left
// it to run on (its tool limit) are not watched. stop ends the watch and
// returns the hold that ended devctl, if any.
func (g *gateRun) watchPreempt() (stop func() (state.Hold, bool)) {
	var mu sync.Mutex
	var hit *state.Hold
	done := make(chan struct{})
	if g.pr != 0 && !g.central {
		go func() {
			for {
				select {
				case <-done:
					return
				case <-time.After(preemptPoll):
				}
				if h, ok := g.preemptChild(); ok {
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

// preemptChild ends the running merge's devctl when a hold preempts it.
func (g *gateRun) preemptChild() (state.Hold, bool) {
	st, err := g.store.Read()
	if err != nil {
		return state.Hold{}, false
	}
	i := g.mine(st, state.Running)
	if i < 0 {
		return state.Hold{}, false
	}
	m := st.Merges[i]
	h, ok := merge.Preempts(st, time.Now(), m, g.lane)
	if !ok || m.Child == 0 || readPID(mergeBase(g.store.Dir(), g.repo, g.pr)) != m.Child {
		return state.Hold{}, false
	}
	gateLine("%s (%s) by %q lets %s through first: ending %s's devctl (pid %d) before it merges; it keeps its place in lane %s",
		holdTarget(h), h.Reason, h.By.Name, h.Except, g.key(), m.Child, g.lane.Name)
	if err := endChild(m.Child); err != nil {
		return state.Hold{}, false
	}
	return h, true
}

// requeue puts a preempted merge back to waiting in its place, unless
// GitHub reports it merged all the same (devctl merged before it ended), or
// does not answer: then its run is recorded as any other.
func (g *gateRun) requeue(base string, h state.Hold) error {
	p, err := pullState(g.ctx, g.repo, g.pr)
	if err != nil || p.State == github.Merged {
		return nil
	}
	removeMergeFiles(base)
	return g.toWaiting(h, "devctl ended before it merged")
}

// toWaiting puts the running merge back to waiting in its place, a hold h
// letting another pull request through first; when says where its run was.
func (g *gateRun) toWaiting(h state.Hold, when string) error {
	why := fmt.Sprintf("held in lane %s (%s) by %q: %s goes first; %s keeps its place", g.lane.Name, holdTarget(h), h.By.Name, h.Except, g.key())
	_ = g.store.Update(func(st *state.State) ([]state.Event, error) {
		i := g.mine(st, state.Running)
		if i < 0 {
			return nil, nil
		}
		m := &st.Merges[i]
		m.Phase, m.Started, m.Roll, m.Child, m.HandCut = state.Waiting, time.Time{}, nil, 0, ""
		m.Seen = time.Now().UTC()
		return []state.Event{event(g.me, "merge.preempted", "%s: %s, %s", g.key(), when, why)}, nil
	})
	g.requeued = true
	return &preemptedError{why: why}
}
