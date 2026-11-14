package cmd

import (
	"context"
	"errors"
	"io"

	"github.com/giantswarm/beekeeper/internal/central"
)

// watchTendFeedback reads the report threads once per feedback.every, in the
// standby watch only, beside the poll: a slow muster never holds it up.
func watchTendFeedback(w *Watcher, ctx context.Context) {
	fc := w.Host.cfg.Feedback
	if !w.Standby || !fc.Enabled() || w.Host.now.Before(w.FeedbackNext) || !w.FeedbackBusy.CompareAndSwap(false, true) {
		return
	}
	w.FeedbackNext = w.Host.now.Add(fc.Every.Duration)
	quiet := *w.Host
	quiet.out = io.Discard
	go func() {
		defer w.FeedbackBusy.Store(false)
		w.ReadFeedback(ctx, &quiet)
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
