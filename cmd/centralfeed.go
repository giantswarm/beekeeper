package cmd

import (
	"cmp"
	"context"
	"errors"
	"time"

	"github.com/giantswarm/beekeeper/internal/central"
	"github.com/giantswarm/beekeeper/internal/feed"
)

// feedKey is the watch's condition of a feed subscription the central
// instance refused or could not read.
const feedKey = centralKey + " feed"

// feedRetry is how long the feed watch waits before it subscribes again
// after the subscription ended.
const feedRetry = time.Minute

// watchFeed prints the central instance's feed while ctx lasts: it
// subscribes to beekeeper://feed through muster as the person and reads
// the feed once the subscription stands and on each update, printing each
// event above the last one it printed. A subscription that ends on an
// unreachable central instance is the CENTRAL UNREACHABLE line, one the
// instance refused CENTRAL FEED REFUSED; either is opened again after
// feedRetry, and the events meanwhile are printed once it stands.
func (w *watcher) watchFeed(ctx context.Context) {
	kick := make(chan struct{}, 1)
	signal := func() {
		select {
		case kick <- struct{}{}:
		default:
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-kick:
			}
			w.readFeed(ctx)
		}
	}()
	defer func() { <-done }()
	for ctx.Err() == nil {
		err := w.app.hub().Listen(ctx, []string{resourceFeed}, func() {
			w.clear(centralKey)
			w.clear(feedKey)
			signal()
		}, func(uri string) {
			if uri == resourceFeed {
				signal()
			}
		})
		if ctx.Err() != nil {
			return
		}
		if u := (*central.Unreachable)(nil); errors.As(err, &u) {
			w.emit(centralKey, "CENTRAL UNREACHABLE %s: %s", w.cfg.Central.Context, u.Reason)
		} else {
			w.emit(feedKey, "CENTRAL FEED REFUSED %s: %v; subscribed again every %s", w.cfg.Central.Context, err, feedRetry)
		}
		t := time.NewTimer(feedRetry)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// readFeed reads the central feed and prints each event above the last
// one the watch printed, with its actor's person and host, and keeps the
// last one's id in the watch's mark: a restarted watch prints none twice.
// A watch with no last id starts at the feed's newest event, printing
// none of the past.
func (w *watcher) readFeed(ctx context.Context) {
	var f feed.Feed
	if err := w.app.hub().Read(ctx, resourceFeed, &f); err != nil {
		if u := (*central.Unreachable)(nil); errors.As(err, &u) {
			w.emit(centralKey, "CENTRAL UNREACHABLE %s: %s", w.cfg.Central.Context, u.Reason)
			return
		}
		w.emit(feedKey, "CENTRAL FEED UNREADABLE %s: %v", w.cfg.Central.Context, err)
		return
	}
	w.clear(feedKey)
	w.mu.Lock()
	last := w.feedID
	w.mu.Unlock()
	if last == "" && len(f.Events) > 0 {
		w.markFeed(f.Events[len(f.Events)-1].ID)
		return
	}
	for _, ev := range f.Events {
		if ev.ID <= last {
			continue
		}
		w.emitNow(feedKey, "CENTRAL %s [%s on %s]", ev.Line, cmp.Or(ev.Actor.Person, "?"), cmp.Or(ev.Actor.Host, "?"))
		last = ev.ID
	}
	w.markFeed(last)
}

// markFeed keeps id as the last event the watch printed, in its mark at
// once.
func (w *watcher) markFeed(id string) {
	w.mu.Lock()
	changed := id != w.feedID
	w.feedID = id
	w.dirty = w.dirty || changed
	w.mu.Unlock()
	if changed {
		w.saveMark()
	}
}
