package cmd

import (
	"cmp"
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/giantswarm/beekeeper/internal/central"
	"github.com/giantswarm/beekeeper/internal/feed"
	"github.com/giantswarm/beekeeper/internal/state"
)

// feedKey is the watch's condition of a feed subscription the central
// instance refused or could not read.
const feedKey = CentralKey + " feed"

// feedRetry is how long the feed watch waits before it subscribes again
// after the subscription ended.
const feedRetry = time.Minute

// WatchFeed prints the central instance's feed while ctx lasts: it
// subscribes to beekeeper://feed through muster as the person and reads
// the feed (read_feed) once the subscription stands and on each update,
// printing each event above the last one it printed. The first read of a
// subscription says CENTRAL FEED GAP when events after the last one may
// have aged out of the feed meanwhile; a standing one read them all as
// they came. A subscription that ends on an
// unreachable central instance is the CENTRAL UNREACHABLE line, one the
// instance refused CENTRAL FEED REFUSED; either is opened again after
// feedRetry, and the events meanwhile are printed once it stands.
func (w *Watcher) WatchFeed(ctx context.Context) {
	kick := make(chan struct{}, 1)
	var resumed atomic.Bool
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
			if resume := resumed.Swap(false); !w.readFeed(ctx, resume) && resume {
				resumed.Store(true)
			}
		}
	}()
	defer func() { <-done }()
	for ctx.Err() == nil {
		err := w.Host.Hub().Listen(ctx, []string{resourceFeed}, func() {
			w.Clear(CentralKey)
			w.Clear(feedKey)
			resumed.Store(true)
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
			w.Emit(CentralKey, "CENTRAL UNREACHABLE %s: %s", w.Host.Cfg().Central.Context, u.Reason)
		} else {
			w.Emit(feedKey, "CENTRAL FEED REFUSED %s: %v; subscribed again every %s", w.Host.Cfg().Central.Context, err, feedRetry)
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
// none of the past. With resume, a feed that no longer holds the last
// event is the CENTRAL FEED GAP line before the events. It reports
// whether the feed was read.
func (w *Watcher) readFeed(ctx context.Context, resume bool) bool {
	w.Mu.Lock()
	last := w.FeedID
	w.Mu.Unlock()
	var f feed.Feed
	me := state.Party{Name: w.Host.Cfg().Identity.Person, Host: w.Host.Cfg().Identity.Host}
	if _, err := w.Host.CallCentralRaw(ctx, me, nameReadFeed, map[string]any{paramAfter: last}, &f); err != nil {
		if u := (*central.Unreachable)(nil); errors.As(err, &u) {
			w.Emit(CentralKey, "CENTRAL UNREACHABLE %s: %s", w.Host.Cfg().Central.Context, u.Reason)
			return false
		}
		w.Emit(feedKey, "CENTRAL FEED UNREADABLE %s: %v", w.Host.Cfg().Central.Context, err)
		return false
	}
	w.Clear(feedKey)
	if last == "" {
		if len(f.Events) > 0 {
			w.markFeed(f.Events[len(f.Events)-1].ID)
		}
		return true
	}
	if resume && f.Gap {
		w.EmitNow(feedKey, "CENTRAL FEED GAP %s: events after %s may be missing: the central instance no longer holds them", w.Host.Cfg().Central.Context, feedTime(last))
	}
	for _, ev := range f.Events {
		if ev.ID <= last {
			continue
		}
		w.EmitNow(feedKey, "CENTRAL %s [%s on %s]", Printable(ev.Line), Printable(cmp.Or(ev.Actor.Person, "?")), Printable(cmp.Or(ev.Actor.Host, "?")))
		last = ev.ID
	}
	w.markFeed(last)
	return true
}

// feedTime is when the event of a feed id happened, the id itself if it
// names no time.
func feedTime(id string) string {
	nanos, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return id
	}
	return time.Unix(0, nanos).UTC().Format(time.RFC3339)
}

// Printable is another machine's text as one line of the watch: a control
// character or escape is a space, so it forges no line and moves no cursor.
func Printable(s string) string {
	return Cmd.OneLine(strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) {
			return ' '
		}
		return r
	}, s))
}

// markFeed keeps id as the last event the watch printed, in its mark at
// once.
func (w *Watcher) markFeed(id string) {
	w.Mu.Lock()
	changed := id != w.FeedID
	w.FeedID = id
	w.Dirty = w.Dirty || changed
	w.Mu.Unlock()
	if changed {
		w.SaveMark()
	}
}
