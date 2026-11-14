package cmd

import (
	"context"
	"strings"

	"github.com/giantswarm/beekeeper/internal/state"
)

// HandCITimerToReview handles a successful typed CI wait without an LLM
// turn. Re-read the rollup: a timer may have queued against an older head.
// Red, closed and expired waits need reasoning and use a fresh continuation.
func (w *Watcher) HandCITimerToReview(ctx context.Context, f timerFire, ag state.Agent) (bool, error) {
	kind, ref, _ := strings.Cut(f.T.When, " ")
	if kind != timerPRChecks || ref != ag.Park.On || f.TimedOut {
		return false, nil
	}
	if ag.Park.ReviewHead == "" && ag.Execution == nil {
		return false, nil
	}
	if w.LoopOwned {
		var handed bool
		err := w.Host.loopGitHub(ctx, "ci-review", func() error {
			var err error
			handed, err = w.handCITimerToReviewRead(ctx, f, ag, ref)
			return err
		})
		return handed, err
	}
	return w.handCITimerToReviewRead(ctx, f, ag, ref)
}

// handCITimerToReviewRead runs in the command, which holds what it needs (watchHandCITimerToReviewRead).
func (w *Watcher) handCITimerToReviewRead(ctx context.Context, f timerFire, ag state.Agent, ref string) (bool, error) {
	return watchHandCITimerToReviewRead(w, ctx, f, ag, ref)
}
