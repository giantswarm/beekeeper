package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

type SuspendWake struct {
	Party   state.Party
	message string
	Guide   bool
}

// Suspends runs in the command, which holds what it needs (watchSuspends).
func (w *Watcher) Suspends(ctx context.Context, since time.Time, sessions []*claude.Session) {
	watchSuspends(w, ctx, since, sessions)
}

func (w *Watcher) Resumed(st *state.State, sessions []*claude.Session, entry, exit time.Time) {
	var stopped, timers []string
	if w.SuspendRecovery == nil {
		w.SuspendRecovery = map[string]SuspendWake{}
	}
	for _, ag := range st.Agents {
		if ag.Execution != nil || ag.Task == "" || ag.Done || ag.Park != nil || pastRun(st, ag.Party, exit) || holdsRole(st, ag.Party) {
			continue
		}
		if _, live := claude.Live(sessions, ag.Party); live {
			continue
		}
		stopped = append(stopped, ag.Name)
		if w.Standby && st.Stop == nil {
			w.SuspendRecovery[ag.Name] = SuspendWake{Party: ag.Party, message: "beekeeper: the machine resumed from suspend and your CLI stopped. Continue your assigned task: " + ag.Task}
		}
	}
	for _, timer := range st.Timers {
		if timer.Due.After(entry) && !timer.Due.After(exit) {
			timers = append(timers, fmt.Sprintf("#%d %s", timer.ID, timer.What))
		}
	}
	if w.Standby && st.Stop == nil {
		if holder := guideRole.Get(st).Holder; holder != nil {
			for name, wake := range w.SuspendRecovery {
				if wake.Guide {
					delete(w.SuspendRecovery, name)
				}
			}
			w.SuspendRecovery[holder.Name] = SuspendWake{Party: holder.Party, message: guideRole.ResumeMessage("the machine resumed from suspend; check the workers and timers that passed during the gap"), Guide: true}
		}
	}
	slices.Sort(timers)
	w.EmitNow("suspend", "SUSPENDED / RESUMED: %s to %s (gap %s); stopped agents: %s; timers passed: %s", entry.Format(time.RFC3339), exit.Format(time.RFC3339), dur(exit.Sub(entry)), suspendNames(stopped), suspendItems(timers))
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
// or race its ordinary role recovery. Consume each wake before delivery: even an
// error may have started a turn, so retrying could start another copy.
func (w *Watcher) RecoverSuspend(ctx context.Context) {
	if !w.Standby || len(w.SuspendRecovery) == 0 || !w.Stand.Busy.CompareAndSwap(false, true) {
		return
	}
	now := w.Host.now
	pending := w.SuspendRecovery
	w.SuspendRecovery = nil
	w.Stand.Inflight.Add(1)
	go func() {
		defer w.Stand.Inflight.Done()
		defer w.Stand.Busy.Store(false)
		var attempted []state.Party
		for _, wake := range pending {
			// Recheck authority immediately before each delivery: a relay may
			// have completed while the batch waited for the delivery slot.
			st, err := w.Host.store.Read()
			if err != nil {
				w.EmitNow("suspend-recovery", "RESUME FAILED: %q: read current state: %v", wake.Party.Name, err)
				continue
			}
			if st.Stop != nil {
				return
			}
			if wake.Guide {
				holder := guideRole.Get(st).Holder
				if holder == nil {
					continue
				}
				wake.Party = holder.Party
			} else if pastRun(st, wake.Party, now) || holdsRole(st, wake.Party) {
				continue
			}
			if slices.ContainsFunc(attempted, func(p state.Party) bool { return p.Is(wake.Party) }) {
				continue
			}
			attempted = append(attempted, wake.Party)
			if w.SuspendDeliver != nil {
				err = w.SuspendDeliver(ctx, wake)
			} else if wake.Guide && w.Stand.Revive != nil {
				err = w.Stand.Revive(ctx, guideRole, wake.Party, wake.message)
			} else {
				err = w.Host.wakeAgent(ctx, WatchParty, wake.Party.Name, wake.message, "")
			}
			if err != nil {
				w.EmitNow("suspend-recovery", "RESUME FAILED: %q: %v", wake.Party.Name, err)
			}
		}
	}()
}
