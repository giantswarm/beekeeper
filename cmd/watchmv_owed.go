package cmd

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

const OwedFor = 24 * time.Hour

const owedBusy = 2 * ownerWait

// OwedOutcomes hands over each owed outcome whose gate and merge-child no longer
// hand it over themselves (both gone, or owedBusy passed), one claim per
// owedRetry under the state's lock, so one watch tries it: a caller that
// heard it drops it, and one owed past owedFor is given up.
func (w *Watcher) OwedOutcomes(ctx context.Context) {
	st, err := w.Host.store.Peek()
	if err != nil || len(st.Owed) == 0 {
		return
	}
	now := w.Host.now.UTC()
	due := func(o state.Owed) bool {
		busy := (proc.Alive(o.Gate) || proc.Alive(o.Child)) && now.Sub(o.Since) < owedBusy
		return !busy && now.Sub(o.Tried) >= owedRetry
	}
	if !slices.ContainsFunc(st.Owed, due) {
		return
	}
	var claimed []state.Owed
	_ = w.Host.store.Update(func(st *state.State) ([]state.Event, error) {
		for i, o := range st.Owed {
			if due(o) {
				st.Owed[i].Tried = now
				claimed = append(claimed, st.Owed[i])
			}
		}
		return nil, nil
	})
	for _, o := range claimed {
		switch {
		case heard(o):
			w.Host.paid(o)
		case now.Sub(o.Since) > OwedFor:
			w.Host.paid(o, fmt.Sprintf("owed to %q since %s and never handed over (%s): given up", o.Owner.Name, clock(now, o.Since), o.Why))
		default:
			w.Host.payOwed(ctx, o)
		}
	}
}
