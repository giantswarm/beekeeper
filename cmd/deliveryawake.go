package cmd

import (
	"context"
	"time"

	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/state"
)

// Read power history before either watch evaluates health. Waiting for the
// standby's resume notification races the supervisor's first post-resume pass.
// Reconstructing from the journal also survives a watch restart during sleep.
func deliveryAwakeClock(st *state.State, now time.Time) func(time.Time) time.Duration {
	since := now
	for _, d := range st.Deliveries {
		if d.Active() && !d.Since.IsZero() && d.Since.Before(since) {
			since = d.Since
		}
	}
	if at := st.DeliveryHealth.FullSince; !at.IsZero() && at.Before(since) {
		since = at
	}
	var records []machine.SuspendRecord
	if reader, ok := plat.Machine.(suspendReader); ok && since.Before(now) {
		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()
		// Unsupported platforms and inaccessible journals retain the existing
		// wall clock behavior, like the optional watch resume observer.
		records, _ = reader.SuspendRecords(ctx, since.Add(-time.Second))
	}
	return func(start time.Time) time.Duration {
		return deliveryAwakeElapsed(start, now, records)
	}
}

func deliveryAwakeElapsed(start, now time.Time, records []machine.SuspendRecord) time.Duration {
	elapsed := max(time.Duration(0), now.Sub(start))
	// An exit without an entry means sleep began before the query boundary.
	entry := start
	for _, record := range records {
		if record.At.After(now) {
			break
		}
		if record.Entry {
			entry = record.At
			continue
		}
		if !entry.IsZero() && record.At.After(entry) {
			from := entry
			if from.Before(start) {
				from = start
			}
			elapsed -= max(time.Duration(0), record.At.Sub(from))
		}
		entry = time.Time{}
	}
	return max(time.Duration(0), elapsed)
}
