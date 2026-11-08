package cmd

import (
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

func TestDeliveryWIPInFlightBounds(t *testing.T) {
	now := time.Now().UTC()
	policy := deliveryPolicy{stallAfter: 2 * time.Hour, reserveFor: 10 * time.Minute}
	for _, tc := range []struct {
		name, step                string
		age                       time.Duration
		live, done, outside, want bool
	}{
		{name: "recent implement", step: state.DeliveryImplementing, age: time.Hour, want: true},
		{name: "abandoned implement", step: state.DeliveryImplementing, age: 2 * time.Hour},
		{name: "recent review", step: state.DeliveryReviewQueued, age: 9 * time.Minute, want: true},
		{name: "abandoned review", step: state.DeliveryReviewQueued, age: 10 * time.Minute},
		{name: "abandoned fixer", step: state.DeliveryFixing, age: time.Hour},
		{name: "live reviewer", step: state.DeliveryReviewing, age: 24 * time.Hour, live: true, want: true},
		{name: endedName, step: state.DeliveryCI, age: 24 * time.Hour, live: true, done: true},
		{name: "outside ownership", step: state.DeliveryCI, age: time.Minute, live: true, outside: true},
		{name: "released", step: state.DeliveryDone, age: time.Minute, live: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := state.Delivery{Ref: "example/project#1", Step: tc.step, Since: now.Add(-tc.age), Agent: workerRole, OutsideDesk: tc.outside}
			st := &state.State{DeliveriesAt: now.Add(-5 * time.Minute), Deliveries: []state.Delivery{d}}
			if tc.live {
				st.Agents = []state.Agent{{Party: state.Party{Name: workerRole}, Task: task, Done: tc.done}}
			}
			v := readDeliveryWIP(st, 2, time.Second, now, policy)
			if (v.Count == 1) != tc.want || v.Reason != "" {
				t.Fatalf("unexpected WIP: %+v", v)
			}
			if got := readDelivery(st, 2, now, policy).InDelivery; got != v.Count {
				t.Fatalf("display %d admission %d", got, v.Count)
			}
		})
	}
}

func TestDeliveryPartialObservationRetainsCountAndStarts(t *testing.T) {
	now := time.Now().UTC()
	st := &state.State{DeliveriesAt: now.Add(-5 * time.Minute)}
	st.StartDelivery("example/project#1", workerRole, "", now.Add(-time.Hour))
	if v := readDeliveryWIP(st, 2, time.Second, now); v.Count != 1 || v.Reason != "" {
		t.Fatal(v)
	}
	// A local start during a partial sweep must consume the remaining place.
	st.StartDelivery("example/project#2", "second", "", now)
	if v := readDeliveryWIP(st, 2, time.Second, now); v.Count != 2 || v.Reason == "" {
		t.Fatal(v)
	}
	if v := readDeliveryWIP(st, 3, time.Second, now.Add(6*time.Minute)); v.Reason == "" {
		t.Fatal("old complete observation admitted work")
	}
}

func TestAbandonedDeliveryEscalatesWithoutHoldingWIP(t *testing.T) {
	a, _ := supervisorApp(t)
	now := a.timeNow()
	a.cfg.Delivery.WIPLimit = 1
	seed(t, a, func(st *state.State) {
		st.DeliveriesAt = now
		st.StartDelivery("example/project#1", "gone", "", now.Add(-48*time.Hour))
	})
	if line, err := a.checkDeliveries(now); err != nil || line == "" {
		t.Fatalf("%s %v", line, err)
	}
	if line, err := a.checkDeliveries(now.Add(time.Minute)); err != nil || line != "" {
		t.Fatalf("duplicate: %s %v", line, err)
	}
	st, _ := a.store.Read()
	if v := readDeliveryWIP(st, 1, time.Second, now, a.deliveryPolicy()); v.Count != 0 || v.Reason != "" {
		t.Fatal(v)
	}
	if !st.Deliveries[0].Active() {
		t.Fatal("stalled delivery lost its record")
	}
}
