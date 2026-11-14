package cmd

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/giantswarm/beekeeper/internal/mailbox"
)

// mailboxRetry is how long the mailbox watch waits before it subscribes
// again after the subscription ended.
const mailboxRetry = time.Minute

// watchMailbox delivers the person's mailbox to the machine's agents while
// ctx lasts: it subscribes to beekeeper://mailbox/<person> through muster
// and drains the mailbox once the subscription stands and on each update.
// A subscription that ends is one MAILBOX UNSUBSCRIBED line and is opened
// again after mailboxRetry; meanwhile, and while a message waits for an
// agent that did not take it or an acknowledgement that failed, the mailbox
// is drained every watch interval.
func (w *watcher) watchMailbox(ctx context.Context) {
	person := mailbox.Key(w.a.cfg.Identity.Person)
	if person == "" {
		return
	}
	uri := resourceMailbox + person
	kick := make(chan struct{}, 1)
	signal := func() {
		select {
		case kick <- struct{}{}:
		default:
		}
	}
	var down, waiting atomic.Bool
	down.Store(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(w.a.cfg.Watch.Interval.Duration)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-kick:
			case <-tick.C:
				if !down.Load() && !waiting.Load() {
					continue
				}
			}
			waiting.Store(w.drainMailbox(ctx))
		}
	}()
	defer func() { <-done }()
	for ctx.Err() == nil {
		err := w.a.hub().Listen(ctx, []string{uri}, func() {
			down.Store(false)
			w.clear(mailboxKey)
			signal()
		}, func(u string) {
			if u == uri {
				signal()
			}
		})
		if ctx.Err() != nil {
			return
		}
		down.Store(true)
		w.emit(mailboxKey, "MAILBOX UNSUBSCRIBED %s: %s; drained every %s until it stands again", uri, unreadableReason(err), w.a.cfg.Watch.Interval.Duration)
		t := time.NewTimer(mailboxRetry)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// drainMailbox runs in the command, which holds what it needs (watchDrainMailbox).
func (w *watcher) drainMailbox(ctx context.Context) bool {
	return watchDrainMailbox(w, ctx)
}
