package cmd

import (
	"context"
	"fmt"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
	"github.com/giantswarm/beekeeper/internal/watch"
)

func watchHandCITimerToReviewRead(w *watch.Watcher, ctx context.Context, f timerFire, ag state.Agent, ref string) (bool, error) {
	c := checkOf(f.T)
	out, err := readProbe(ctx, c.cmd)
	if err != nil {
		return false, err
	}
	p, err := loop.ParseLoopPull(ref, out)
	if err != nil {
		return false, err
	}
	if !p.Green || p.State != superloop.Open || p.Head == "" {
		return false, nil
	}
	if ag.Execution != nil {
		st, err := watchApp(w.Host).store.Read()
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
	err = watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
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
			return []state.Event{event(watch.WatchParty, "agents.ci-review", "%s: %s at %s; no worker turn", cur.Name, ref, p.Head)}, nil
		}
		return nil, fmt.Errorf("CI owner left roster before review handoff")
	})
	return err == nil, err
}
