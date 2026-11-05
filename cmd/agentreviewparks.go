package cmd

import (
	"cmp"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/state"
)

// recordAgentReviewPark saves each proven request before the worker can park
// on another PR. Agent.Park remains its current wait; ReviewParks is per PR.
func recordAgentReviewPark(st *state.State, ag state.Agent) {
	p := ag.Park
	if p == nil || p.ReviewHead == "" || loopParkOn(p) == "" {
		return
	}
	rp := state.ReviewPark{On: loopParkOn(p), At: p.At, ReviewHead: p.ReviewHead,
		Agent: ag.Name, Party: ag.Party, Execution: ag.Execution, Model: ag.Model,
		Serves: reviewParkServes(st, ag, p.On), PostMerge: p.PostMerge, Worktree: p.Worktree}
	if k := st.StartIndex(ag.Party); k >= 0 {
		start := st.Starts[k]
		rp.Model = cmp.Or(rp.Model, start.Model)
		rp.Worktree = cmp.Or(rp.Worktree, start.Dir)
		if start.Execution != nil {
			rp.Selector = start.Execution.Selector
		}
	}
	st.UpsertReviewPark(rp)
}

func clearAgentReviewParks(st *state.State, party state.Party, agent string) {
	st.ReviewParks = slices.DeleteFunc(st.ReviewParks, func(rp state.ReviewPark) bool {
		return (party.Name != "" && rp.Party.Is(party)) || (agent != "" && strings.EqualFold(rp.Agent, agent))
	})
}

func (l *supervisorLoop) otherReviewParks(party state.Party, ref string) bool {
	st, err := l.a.store.Read()
	if err != nil {
		// Preserve the task if pending requests cannot be read.
		return true
	}
	for _, rp := range st.ReviewParks {
		if rp.Party.Is(party) && !strings.EqualFold(rp.On, ref) {
			return true
		}
	}
	return false
}

// reviewPostMerge inherits earlier work only when the new park omits it.
// A supplied empty string explicitly declares that the task ends at merge.
func reviewPostMerge(st *state.State, on string, current *state.Park, declared *string) *string {
	if declared != nil {
		return declared
	}
	if current != nil && strings.EqualFold(current.On, on) && current.PostMerge != nil {
		return current.PostMerge
	}
	if rp, ok := st.ReviewParkFor(on); ok && rp.PostMerge != nil {
		return rp.PostMerge
	}
	return new(string)
}
