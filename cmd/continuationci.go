package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// handCITimerToReview handles a successful typed CI wait without an LLM
// turn. Re-read the rollup: a timer may have queued against an older head.
// Red, closed and expired waits need reasoning and use a fresh continuation.
func (w *watcher) handCITimerToReview(ctx context.Context, f timerFire, ag state.Agent) (bool, error) {
	kind, ref, _ := strings.Cut(f.t.When, " ")
	if kind != timerPRChecks || ref != ag.Park.On || f.timedOut {
		return false, nil
	}
	if ag.Park.ReviewHead == "" && ag.Execution == nil {
		return false, nil
	}
	if w.loopOwned {
		var handed bool
		err := w.loopGitHub(ctx, "ci-review", func() error {
			var err error
			handed, err = w.handCITimerToReviewRead(ctx, f, ag, ref)
			return err
		})
		return handed, err
	}
	return w.handCITimerToReviewRead(ctx, f, ag, ref)
}

func (w *watcher) handCITimerToReviewRead(ctx context.Context, f timerFire, ag state.Agent, ref string) (bool, error) {
	c := checkOf(f.t)
	out, err := readProbe(ctx, c.cmd)
	if err != nil {
		return false, err
	}
	p, err := ParseLoopPull(ref, out)
	if err != nil {
		return false, err
	}
	if !p.Green || p.State != superloop.Open || p.Head == "" {
		return false, nil
	}
	if ag.Execution != nil {
		st, err := w.store.Read()
		if err != nil {
			return false, err
		}
		r, _ := executionRecord(st, *ag.Execution)
		if r == nil || executionReviewHead(ctx, r, ref, p.Head) != p.Head {
			return false, nil
		}
	} else if ag.Park.ReviewHead != p.Head {
		return false, nil
	}
	err = w.store.Update(func(st *state.State) ([]state.Event, error) {
		for i := range st.Agents {
			cur := &st.Agents[i]
			if !cur.Is(ag.Party) {
				continue
			}
			if cur.Park == nil || !cur.Park.At.Equal(ag.Park.At) || cur.Park.On != ref || cur.Park.Continuing || len(cur.Park.Continuation) > 0 {
				return nil, fmt.Errorf("CI park changed before review handoff")
			}
			if cur.Execution != nil {
				if r, _ := executionRecord(st, *cur.Execution); r != nil {
					r.Completed = true
				}
			}
			cur.Park.Checks = false
			cur.Park.RefKind = github.RefPullRequest
			cur.Park.ReviewHead = p.Head
			cur.Park.Waits = deskReviewWait
			cur.Park.Resumable = state.Park{}.Resumable
			cur.Park.Answer = ""
			recordAgentReviewPark(st, *cur)
			return []state.Event{event(watchParty, "agents.ci-review", "%s: %s at %s; no worker turn", cur.Name, ref, p.Head)}, nil
		}
		return nil, fmt.Errorf("CI owner left roster before review handoff")
	})
	return err == nil, err
}
