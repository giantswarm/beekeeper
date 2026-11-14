package cmd

import (
	"context"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/machine"
)

// This optional capability leaves platforms without a kernel journal unchanged.
type suspendReader interface {
	SuspendRecords(context.Context, time.Time) ([]machine.SuspendRecord, error)
}

func watchSuspends(w *watcher, ctx context.Context, since time.Time, sessions []*claude.Session) {
	reader, ok := plat.Machine.(suspendReader)
	if !ok {
		return
	}
	defer w.recoverSuspend(ctx)
	if !w.suspendChecked.IsZero() && w.suspendChecked.Before(since) {
		since = w.suspendChecked
	}
	query, cancel := context.WithTimeout(ctx, commandTimeout)
	records, err := reader.SuspendRecords(query, since.Add(-2*time.Second))
	cancel()
	if err != nil {
		return
	} // journal access is optional, as on unsupported platforms
	for _, record := range records {
		if !record.At.After(w.suspendThrough) {
			continue
		}
		if record.Entry {
			w.suspendEntry = record.At
			continue
		}
		if w.suspendEntry.IsZero() || record.At.Before(w.suspendEntry) {
			continue
		}
		st, err := w.a.store.Peek()
		if err != nil {
			return
		} // retry the pair on the next poll
		w.resumed(st, sessions, w.suspendEntry, record.At)
		// The supervisor is now a service loop. Apply the same service recovery
		// as after a reboot; a healthy loop continues without restarting it.
		if w.standby {
			w.checkLoop(ctx, st)
		}
		w.suspendThrough = record.At
		w.suspendEntry = time.Time{}
	}
	w.suspendChecked = w.a.now
}
