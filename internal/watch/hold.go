package watch

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// ProbeHoldLifts runs the lift probe of every active hold and returns the
// probes that passed by target.
func ProbeHoldLifts(ctx context.Context, holds []state.Hold, now time.Time) map[string]string {
	passed := map[string]string{}
	for _, h := range holds {
		if h.LiftWhen != "" && h.Active(now) && RunProbe(ctx, h.LiftWhen) {
			passed[h.Target] = h.LiftWhen
		}
	}
	return passed
}

// LiftProbed lifts the holds whose probe passed, as long as they still carry
// that probe, and returns their watch lines and events.
func LiftProbed(st *state.State, passed map[string]string) ([]string, []state.Event) {
	var lines []string
	var evs []state.Event
	st.Holds = slices.DeleteFunc(st.Holds, func(h state.Hold) bool {
		if probe, ok := passed[h.Target]; !ok || probe != h.LiftWhen {
			return false
		}
		lines = append(lines, fmt.Sprintf("HOLD LIFTED: %s, its probe passed: %s", Cmd.HoldTarget(h), Cmd.Truncate(h.Reason, 200)))
		evs = append(evs, Cmd.Event(WatchParty, "hold.lift", "%s: its probe passed (%s)", h.Target, h.LiftWhen))
		return true
	})
	return lines, evs
}
