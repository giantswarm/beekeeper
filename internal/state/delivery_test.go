package state

import (
	"reflect"
	"testing"
	"time"
)

func TestDeliveryStartKeepsIssueIdentityAndClock(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st := &State{}
	st.StartDelivery("O/R#7", workerWord, "", now)
	initial := st.Deliveries[0]
	st.StartDelivery("o/r#7", "fixer", "", now.Add(time.Hour))
	if len(st.Deliveries) != 1 || !reflect.DeepEqual(st.Deliveries[0], initial) {
		t.Fatal(st.Deliveries)
	}
	if events := DeliveryEvents([]Delivery{initial}, st.Deliveries, Party{}); len(events) != 0 {
		t.Fatal(events)
	}
	st.Deliveries[0].Step = DeliveryDropped
	st.StartDelivery("o/r#7", "new worker", "", now.Add(2*time.Hour))
	if len(st.Deliveries) != 1 || st.Deliveries[0].Step != DeliveryImplementing || !st.Deliveries[0].Started.Equal(now.Add(2*time.Hour)) {
		t.Fatal(st.Deliveries)
	}
}

func TestDeliveryAndEventCommitTogetherAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := store.Update(func(st *State) ([]Event, error) {
		st.StartDelivery("o/r#1", workerWord, "", now)
		return DeliveryEvents(nil, st.Deliveries, Party{Name: workerWord}), nil
	}); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Read()
	if err != nil || len(st.Deliveries) != 1 {
		t.Fatalf("%+v %v", st, err)
	}
	events, err := store.Events(0, func(e Event) bool { return e.Verb == DeliveryStepVerb })
	if err != nil || len(events) != 1 || events[0].Subject != "o/r#1" {
		t.Fatalf("%+v %v", events, err)
	}
	if got := DeliveryEvents(st.Deliveries, st.Deliveries, Party{}); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestStartDeliveryForLinkedPullKeepsIssueDeliveries(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	st := &State{}
	for _, ref := range []string{"o/r#2", "o/r#3"} {
		st.StartDelivery(ref, "implementer", "", now)
	}
	for i := range st.Deliveries {
		st.Deliveries[i].Pulls = []string{"o/r#1"}
		st.Deliveries[i].Step = DeliveryReviewQueued
	}
	st.StartDelivery("O/R#1", "reviewer", "", now.Add(time.Hour))
	st.StartDelivery("o/r#1", "fixer", "", now.Add(2*time.Hour))
	if len(st.Deliveries) != 2 {
		t.Fatalf("linked pull duplicated issue deliveries: %+v", st.Deliveries)
	}
	for _, d := range st.Deliveries {
		if d.Agent != "implementer" || !d.Started.Equal(now) || d.Step != DeliveryReviewQueued {
			t.Fatalf("review/fix start changed original delivery: %+v", d)
		}
	}
}
