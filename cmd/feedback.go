package cmd

import (
	"context"
	"errors"
	"io"

	"github.com/giantswarm/beekeeper/internal/central"
)

// watchTendFeedback reads the report threads once per feedback.every, in the
// standby watch only, beside the poll: a slow muster never holds it up.
func watchTendFeedback(w *watcher, ctx context.Context) {
	fc := w.a.cfg.Feedback
	if !w.standby || !fc.Enabled() || w.a.now.Before(w.feedbackNext) || !w.feedbackBusy.CompareAndSwap(false, true) {
		return
	}
	w.feedbackNext = w.a.now.Add(fc.Every.Duration)
	quiet := *w.a
	quiet.out = io.Discard
	go func() {
		defer w.feedbackBusy.Store(false)
		w.readFeedback(ctx, &quiet)
	}()
}

// unreadableReason is a muster call's error without the central instance's
// wording: the reason alone.
func unreadableReason(err error) string {
	var u *central.Unreachable
	switch {
	case err == nil:
		return ""
	case errors.As(err, &u):
		return u.Reason
	}
	return err.Error()
}
