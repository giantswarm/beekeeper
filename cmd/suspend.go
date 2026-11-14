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

func watchSuspends(w *Watcher, ctx context.Context, since time.Time, sessions []*claude.Session) {
	reader, ok := plat.Machine.(suspendReader)
	if !ok {
		return
	}
	defer w.RecoverSuspend(ctx)
	if !w.SuspendChecked.IsZero() && w.SuspendChecked.Before(since) {
		since = w.SuspendChecked
	}
	query, cancel := context.WithTimeout(ctx, commandTimeout)
	records, err := reader.SuspendRecords(query, since.Add(-2*time.Second))
	cancel()
	if err != nil {
		return
	} // journal access is optional, as on unsupported platforms
	for _, record := range records {
		if !record.At.After(w.SuspendThrough) {
			continue
		}
		if record.Entry {
			w.SuspendEntry = record.At
			continue
		}
		if w.SuspendEntry.IsZero() || record.At.Before(w.SuspendEntry) {
			continue
		}
		st, err := watchApp(w.Host).store.Peek()
		if err != nil {
			return
		} // retry the pair on the next poll
		w.Resumed(st, sessions, w.SuspendEntry, record.At)
		// The supervisor is now a service loop. Apply the same service recovery
		// as after a reboot; a healthy loop continues without restarting it.
		if w.Standby {
			w.CheckLoop(ctx, st)
		}
		w.SuspendThrough = record.At
		w.SuspendEntry = time.Time{}
	}
	w.SuspendChecked = watchApp(w.Host).now
}
