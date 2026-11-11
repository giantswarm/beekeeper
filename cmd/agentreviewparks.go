package cmd

import (
	"cmp"
	"context"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"

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

// recoverReviewRequests upgrades explicit review waits only after re-proving
// their published worktree. Older parks omitted Worktree; their recorded start
// directory supplies it. An API failure leaves the request pending for a pass
// after budget recovery, never granting authority from the wait text alone.
func (l *supervisorLoop) recoverReviewRequests(ctx context.Context, st *state.State) error {
	if l.dryRun || st.Stop != nil {
		return nil
	}
	for _, ag := range st.Agents {
		if err := ctx.Err(); err != nil {
			return err
		}
		p := ag.Park
		if ag.Done || ag.Execution != nil || p == nil || p.External || p.Checks || p.ReviewHead != "" || !isDeskReviewWait(p.Waits) {
			continue
		}
		if _, ok := parseRef(p.On); !ok || l.agentUnreadRepo(st, ag, p.On) != "" {
			continue
		}
		dir := p.Worktree
		if dir == "" {
			if k := st.StartIndex(ag.Party); k >= 0 {
				dir = st.Starts[k].Dir
			}
		}
		if dir == "" {
			continue
		}
		if stop, err := l.stopPassForReexec(ctx); stop || err != nil {
			return err
		}
		l.lifecyclePhase(loopPhaseRecoverReviews)
		verifyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		head := recoverParkReviewHead(verifyCtx, p.On, dir)
		cancel()
		if head == "" {
			continue
		}
		verified := false
		if err := l.a.store.Update(func(current *state.State) ([]state.Event, error) {
			for i := range current.Agents {
				a := &current.Agents[i]
				if !a.Is(ag.Party) || a.Done || a.Park == nil || a.Park.On != p.On || !a.Park.At.Equal(p.At) || a.Park.Waits != p.Waits || a.Park.ReviewHead != "" {
					continue
				}
				a.Park.ReviewHead, a.Park.Worktree, a.Park.RefKind = head, dir, github.RefPullRequest
				recordAgentReviewPark(current, *a)
				verified = true
				return []state.Event{event(loopParty, "agents.review-verified", "%s: review request verified for %s at pushed HEAD %s", a.Name, p.On, head)}, nil
			}
			return nil, nil
		}); err != nil {
			return err
		}
		if verified {
			if err := l.a.recordFatalRepair(ag.Name, p.On); err != nil {
				return err
			}
		}
	}
	return nil
}

var recoverParkReviewHead = func(ctx context.Context, on, dir string) string {
	return publishedParkHead(ctx, on, dir, runGit, github.RunGH)
}
