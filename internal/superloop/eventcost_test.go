package superloop

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

// Interleaved workers must retain exactly the same report/problem pairing
// as isolated histories, including a completed worker waiting for intake.
func TestPlanInterleavedHistoriesKeepAgentPairing(t *testing.T) {
	var combined Input
	var want []Action
	for i := range 40 {
		name := fmt.Sprintf("worker-%d", i)
		in := Input{Events: []Event{
			{At: t0, Agent: name, Verb: VerbReport, Detail: "finished"},
			{At: t0.Add(time.Millisecond), Agent: name, Verb: VerbDone},
		}}
		want = append(want, Plan(in)...)
		combined.Events = append(combined.Events, in.Events...)
	}
	got := Plan(combined)
	byKey := func(actions []Action) map[string]Action {
		out := map[string]Action{}
		for _, a := range actions {
			out[a.Key] = a
		}
		return out
	}
	if !reflect.DeepEqual(byKey(got), byKey(want)) {
		t.Fatalf("interleaved histories changed actions: got %v want %v", got, want)
	}
}

func BenchmarkPlanCompletedWorkers(b *testing.B) {
	for _, count := range []int{100, 1000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			in := Input{}
			for i := range count {
				in.Events = append(in.Events, Event{At: t0, Agent: fmt.Sprintf("worker-%d", i), Verb: VerbDone})
			}
			b.ResetTimer()
			for b.Loop() {
				Plan(in)
			}
		})
	}
}
