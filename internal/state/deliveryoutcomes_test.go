package state

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDeliveryOutcomesKeepReasonsAndAttemptsApart(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	var events []Event
	add := func(ref, outcome, step string, started, at time.Time) {
		t.Helper()
		d := Delivery{Ref: ref, Outcome: outcome, Step: step, Started: started, Since: at}
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, Event{At: at, Verb: DeliveryStepVerb, Detail: string(raw)})
	}
	for i, outcome := range []string{OutcomeNoChange, OutcomeBlocked, OutcomeParked, OutcomeStalled, OutcomeMerged} {
		at := now.Add(time.Duration(i) * time.Minute)
		add(outcome, outcome, DeliveryDone, now, at)
		add(outcome, outcome, DeliveryDone, now, at.Add(time.Second))
	}
	add("legacy", "", DeliveryDropped, now, now)
	// An unreported done event is never counted as a merged release, and a
	// subsequent park on an ended attempt does not conceal its drop.
	add("unreported-done", "", DeliveryDone, now, now.Add(2*time.Minute))
	add("legacy", OutcomeParked, DeliveryDropped, now, now.Add(3*time.Minute))
	// A later attempt on the same issue has its own ending.
	add(OutcomeBlocked, OutcomeNoChange, DeliveryDone, now.Add(time.Minute), now.Add(10*time.Minute))
	got := DeliveryOutcomes(events, now, now.Add(time.Hour))
	if got[OutcomeNoChange] != 2 || got[OutcomeBlocked] != 1 || got[OutcomeParked] != 1 || got[OutcomeStalled] != 1 || got[OutcomeMerged] != 1 || got[DeliveryDropped] != 2 {
		t.Fatalf("outcomes: %v", got)
	}
	if got := DeliveryOutcomes(events, now.Add(20*time.Minute), now.Add(time.Hour)); len(got) != 0 {
		t.Fatalf("old outcomes counted: %v", got)
	}
}
