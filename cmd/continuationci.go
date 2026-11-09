package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

// handCITimerToReview handles a successful typed CI wait without an LLM
// turn. Re-read the rollup: a timer may have queued against an older head.
// Red, closed and expired waits need reasoning and use a fresh continuation.
func (w *watcher) handCITimerToReview(ctx context.Context, f timerFire, ag state.Agent) (bool, error) {
	kind, ref, _ := strings.Cut(f.t.When, " ")
	if kind != "pr-checks" || ref != ag.Park.On || f.timedOut {
		return false, nil
	}
	c := checkOf(f.t)
	out, err := readProbe(ctx, c.cmd)
	if err != nil {
		return false, err
	}
	p, err := parseLoopPull(ref, out)
	if err != nil {
		return false, err
	}
	if !p.Green || p.State != "OPEN" || p.Head == "" {
		return false, nil
	}
	err = w.store.Update(func(st *state.State) ([]state.Event, error) {
		for i := range st.Agents {
			cur := &st.Agents[i]
			if !cur.Is(ag.Party) {
				continue
			}
			if cur.Park == nil || !cur.Park.At.Equal(ag.Park.At) || cur.Park.On != ref {
				return nil, fmt.Errorf("CI park changed before review handoff")
			}
			cur.Park.Checks = false
			cur.Park.RefKind = github.RefPullRequest
			cur.Park.ReviewHead = p.Head
			cur.Park.Waits = "desk review"
			cur.Park.Resumable = state.Park{}.Resumable
			cur.Park.Answer = ""
			recordAgentReviewPark(st, *cur)
			return []state.Event{event(watchParty, "agents.ci-review", "%s: %s at %s; no worker turn", cur.Name, ref, p.Head)}, nil
		}
		return nil, fmt.Errorf("CI owner left roster before review handoff")
	})
	return err == nil, err
}
