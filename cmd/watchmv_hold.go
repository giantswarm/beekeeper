package cmd

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// probeHoldLifts runs the lift probe of every active hold and returns the
// probes that passed by target.
func probeHoldLifts(ctx context.Context, holds []state.Hold, now time.Time) map[string]string {
	passed := map[string]string{}
	for _, h := range holds {
		if h.LiftWhen != "" && h.Active(now) && runProbe(ctx, h.LiftWhen) {
			passed[h.Target] = h.LiftWhen
		}
	}
	return passed
}

// liftProbed lifts the holds whose probe passed, as long as they still carry
// that probe, and returns their watch lines and events.
func liftProbed(st *state.State, passed map[string]string) ([]string, []state.Event) {
	var lines []string
	var evs []state.Event
	st.Holds = slices.DeleteFunc(st.Holds, func(h state.Hold) bool {
		if probe, ok := passed[h.Target]; !ok || probe != h.LiftWhen {
			return false
		}
		lines = append(lines, fmt.Sprintf("HOLD LIFTED: %s, its probe passed: %s", holdTarget(h), truncate(h.Reason, 200)))
		evs = append(evs, event(watchParty, "hold.lift", "%s: its probe passed (%s)", h.Target, h.LiftWhen))
		return true
	})
	return lines, evs
}
