package cmd

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

const deliveryTestIssue = "example/project#1"
const deliveryTestPull = "example/project#2"
const deliveryTestTask = "delivery"

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
			d := state.Delivery{Ref: deliveryTestIssue, Step: tc.step, Since: now.Add(-tc.age), Agent: workerRole, OutsideDesk: tc.outside}
			st := &state.State{DeliveriesAt: now.Add(-5 * time.Minute), Deliveries: []state.Delivery{d}}
			if tc.live {
				st.Agents = []state.Agent{{Party: state.Party{Name: workerRole}, Task: deliveryTestTask, Done: tc.done}}
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
	st.StartDelivery(deliveryTestIssue, workerRole, "", now.Add(-time.Hour))
	if v := readDeliveryWIP(st, 2, time.Second, now); v.Count != 1 || v.Reason != "" {
		t.Fatal(v)
	}
	// A local start during a partial sweep must consume the remaining place.
	st.StartDelivery(deliveryTestPull, "second", "", now)
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
		st.StartDelivery(deliveryTestIssue, "gone", "", now.Add(-48*time.Hour))
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

func TestDeliveryWIPLiveAgentServingLinkedPull(t *testing.T) {
	now := time.Now().UTC()
	for _, step := range []string{state.DeliveryReviewing, state.DeliveryFixing} {
		t.Run(step, func(t *testing.T) {
			ag := state.Agent{Party: state.Party{Name: "linked-pull-agent", Session: "linked-pull-session"}, Task: deliveryTestTask}
			st := &state.State{
				DeliveriesAt: now,
				Agents:       []state.Agent{ag},
				Records:      []state.Record{{Session: ag.Party, Issue: deliveryTestPull}},
				Deliveries: []state.Delivery{{
					Ref: deliveryTestIssue, Pulls: []string{deliveryTestPull},
					Agent: "original-worker", Step: step, Since: now.Add(-48 * time.Hour),
				}},
			}
			if v := readDeliveryWIP(st, 2, time.Second, now); v.Count != 1 {
				t.Fatalf("live agent serving linked pull lost its delivery: %+v", v)
			}
			st.Agents[0].Done = true
			if v := readDeliveryWIP(st, 2, time.Second, now); v.Count != 0 {
				t.Fatalf("ended agent retained abandoned delivery: %+v", v)
			}
		})
	}
}

func TestPassivePullsDoNotFillWIPOrRaiseStalls(t *testing.T) {
	a, _ := supervisorApp(t)
	now := a.timeNow()
	seed(t, a, func(st *state.State) {
		st.DeliveriesAt = now
		for i := range 30 {
			ref := fmt.Sprintf("example/project#%d", i+1)
			st.Deliveries = append(st.Deliveries, state.Delivery{Ref: ref, Pulls: []string{ref}, Origin: state.DeliveryOriginObserved, Step: state.DeliveryImplementing, Since: now.Add(-48 * time.Hour)})
		}
	})
	st, _ := a.store.Read()
	if v := readDeliveryWIP(st, 30, time.Second, now); v.Count != 0 || v.Reason != "" {
		t.Fatal(v)
	}
	if line, err := a.checkDeliveries(now); err != nil || line != "" {
		t.Fatalf("passive PRs stalled: %s %v", line, err)
	}
}

func TestDeliveryStallsAreBatchedWithWaitReasons(t *testing.T) {
	a, _ := supervisorApp(t)
	now := a.timeNow()
	seed(t, a, func(st *state.State) {
		for i := range 30 {
			st.StartDelivery(fmt.Sprintf("example/project#%d", i+1), "gone", "", now.Add(-48*time.Hour))
		}
	})
	line, err := a.checkDeliveries(now)
	if err != nil || strings.Contains(line, "\n") || !strings.Contains(line, "30 deliveries") || strings.Contains(line, "waits: ;") || strings.HasSuffix(line, "waits: ") {
		t.Fatalf("unbatched or empty stall: %s %v", line, err)
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == loopFileVerb })
	if err != nil || len(evs) != 1 {
		t.Fatalf("got %d reports: %v", len(evs), err)
	}
	if line, err := a.checkDeliveries(now.Add(time.Minute)); err != nil || line != "" {
		t.Fatalf("repeated batch: %s %v", line, err)
	}
	seed(t, a, func(st *state.State) {
		for i := range st.Deliveries {
			st.Deliveries[i].Step = state.DeliveryDone
		}
	})
	if line, err := a.checkDeliveries(now.Add(2 * time.Minute)); err != nil || strings.Contains(line, "\n") || !strings.HasPrefix(line, "RECOVERED:") {
		t.Fatalf("unbatched recovery: %s %v", line, err)
	}
	evs, err = a.store.Events(0, func(e state.Event) bool { return e.Verb == loopFileVerb })
	st := &state.State{Guide: &state.Role{}}
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		key, text, ok := keyed(ev.Detail)
		if ok {
			retainGuideReport(st, key, text, ev.At, state.Party{})
		}
	}
	if len(st.GuideWork) != 0 {
		t.Fatalf("recovery did not settle batched guide work: %+v", st.GuideWork)
	}
}
