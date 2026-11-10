package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/state"
)

type suspendWake struct {
	party   state.Party
	message string
	guide   bool
}

// This optional capability leaves platforms without a kernel journal unchanged.
type suspendReader interface {
	SuspendRecords(context.Context, time.Time) ([]machine.SuspendRecord, error)
}

func (w *watcher) suspends(ctx context.Context, since time.Time, sessions []*claude.Session) {
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
		st, err := w.store.Read()
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
	w.suspendChecked = w.now
}

func (w *watcher) resumed(st *state.State, sessions []*claude.Session, entry, exit time.Time) {
	var stopped, timers []string
	if w.suspendRecovery == nil {
		w.suspendRecovery = map[string]suspendWake{}
	}
	for _, ag := range st.Agents {
		if ag.Execution != nil || ag.Task == "" || ag.Done || ag.Park != nil {
			continue
		}
		if _, live := claude.Live(sessions, ag.Party); live {
			continue
		}
		stopped = append(stopped, ag.Name)
		if w.standby && st.Stop == nil {
			w.suspendRecovery[ag.Name] = suspendWake{party: ag.Party, message: "beekeeper: the machine resumed from suspend and your CLI stopped. Continue your assigned task: " + ag.Task}
		}
	}
	for _, timer := range st.Timers {
		if timer.Due.After(entry) && !timer.Due.After(exit) {
			timers = append(timers, fmt.Sprintf("#%d %s", timer.ID, timer.What))
		}
	}
	if w.standby && st.Stop == nil {
		if holder := guideRole.get(st).Holder; holder != nil {
			for name, wake := range w.suspendRecovery {
				if wake.guide {
					delete(w.suspendRecovery, name)
				}
			}
			w.suspendRecovery[holder.Name] = suspendWake{party: holder.Party, message: guideRole.resumeMessage("the machine resumed from suspend; check the workers and timers that passed during the gap"), guide: true}
		}
	}
	slices.Sort(timers)
	w.emitNow("suspend", "SUSPENDED / RESUMED: %s to %s (gap %s); stopped agents: %s; timers passed: %s", entry.Format(time.RFC3339), exit.Format(time.RFC3339), dur(exit.Sub(entry)), suspendNames(stopped), suspendItems(timers))
}

func suspendNames(names []string) string {
	if len(names) == 0 {
		return ghNone
	}
	return quoted(names)
}
func suspendItems(items []string) string {
	if len(items) == 0 {
		return ghNone
	}
	return strings.Join(items, ", ")
}

// Use the standby's serialized delivery slot so recovery does not block polling
// or race its ordinary role recovery. Failed deliveries remain queued to retry.
func (w *watcher) recoverSuspend(ctx context.Context) {
	if w.suspendResults != nil {
		select {
		case failed := <-w.suspendResults:
			if w.suspendRecovery == nil {
				w.suspendRecovery = map[string]suspendWake{}
			}
			hasGuide := false
			for _, wake := range w.suspendRecovery {
				if wake.guide {
					hasGuide = true
					break
				}
			}
			for name, wake := range failed {
				if wake.guide && hasGuide {
					continue
				}
				if _, exists := w.suspendRecovery[name]; !exists {
					w.suspendRecovery[name] = wake
				}
			}
		default:
		}
	}
	if !w.standby || len(w.suspendRecovery) == 0 || !w.stand.busy.CompareAndSwap(false, true) {
		return
	}
	if w.suspendResults == nil {
		w.suspendResults = make(chan map[string]suspendWake, 1)
	}
	pending := w.suspendRecovery
	w.suspendRecovery = nil
	w.stand.inflight.Add(1)
	go func() {
		defer w.stand.inflight.Done()
		defer w.stand.busy.Store(false)
		failed := map[string]suspendWake{}
		st, stateErr := w.store.Read()
		if stateErr == nil && st.Stop != nil {
			w.suspendResults <- failed
			return
		}
		for _, wake := range pending {
			if wake.guide && stateErr == nil {
				holder := guideRole.get(st).Holder
				if holder == nil {
					continue
				}
				wake.party = holder.Party
			}
			var err error
			if w.suspendDeliver != nil {
				err = w.suspendDeliver(ctx, wake)
			} else if wake.guide && w.stand.revive != nil {
				err = w.stand.revive(ctx, guideRole, wake.party, wake.message)
			} else {
				err = w.wakeAgent(ctx, watchParty, wake.party.Name, wake.message, "")
			}
			if err != nil {
				if !errors.Is(err, errResumeStartedCopy) {
					failed[wake.party.Name] = wake
				}
				w.emitNow("suspend-recovery", "RESUME FAILED: %q: %v", wake.party.Name, err)
			}
		}
		w.suspendResults <- failed
	}()
}
