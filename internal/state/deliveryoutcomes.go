package state

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// DeliveryOutcomes counts one ending per delivery attempt in the window.
// Parked is a worker handoff, not a released issue. Legacy unexplained ends
// remain visible as dropped rather than being given invented evidence.
func DeliveryOutcomes(events []Event, since, until time.Time) map[string]int {
	type ending struct {
		outcome string
		at      time.Time
	}
	attempts := map[string]ending{}
	events = slices.Clone(events)
	slices.SortStableFunc(events, func(a, b Event) int { return a.At.Compare(b.At) })
	for _, e := range events {
		var d Delivery
		if e.Verb != DeliveryStepVerb || e.At.After(until) || json.Unmarshal([]byte(e.Detail), &d) != nil || d.Ref == "" {
			continue
		}
		key := fmt.Sprintf("%s/%s", d.Ref, d.Started.Format(time.RFC3339Nano))
		outcome := d.Outcome
		if outcome == "" {
			switch d.Step {
			case DeliveryDone:
				outcome = OutcomeMerged
			case DeliveryDropped:
				outcome = DeliveryDropped
			}
		}
		if outcome != "" && attempts[key].outcome != outcome {
			attempts[key] = ending{outcome, e.At}
		}
	}
	out := map[string]int{}
	for _, e := range attempts {
		if !e.at.Before(since) {
			out[e.outcome]++
		}
	}
	return out
}
