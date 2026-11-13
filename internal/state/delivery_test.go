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
	for _, ref := range []string{"review/repo#2", "review/repo#3"} {
		st.StartDelivery(ref, "implementer", "", now)
	}
	for i := range st.Deliveries {
		st.Deliveries[i].Pulls = []string{"review/repo#1"}
		st.Deliveries[i].Step = DeliveryReviewQueued
	}
	st.StartDelivery("REVIEW/REPO#1", "reviewer", "", now.Add(time.Hour))
	st.StartDelivery("review/repo#1", "fixer", "", now.Add(2*time.Hour))
	if len(st.Deliveries) != 2 {
		t.Fatalf("linked pull duplicated issue deliveries: %+v", st.Deliveries)
	}
	for _, d := range st.Deliveries {
		if d.Agent != "implementer" || !d.Started.Equal(now) || d.Step != DeliveryReviewQueued {
			t.Fatalf("review/fix start changed original delivery: %+v", d)
		}
	}
}

func TestContinueDeliveryKeepsTheTasksDelivery(t *testing.T) {
	started := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	at := started.Add(3 * time.Hour)
	const issue, pull = "o/r#1", "o/r#2"
	for _, tc := range []struct {
		name  string
		prev  []Delivery
		pull  string
		want  Delivery
		fresh bool
	}{
		{name: "merged and released", pull: pull,
			prev: []Delivery{{Ref: issue, Pulls: []string{pull}, Step: DeliveryDone, Outcome: OutcomeMerged, Evidence: "merged", Agent: "old", Started: started}},
			want: Delivery{Ref: issue, Pulls: []string{pull}, Step: DeliveryDone, Outcome: OutcomeMerged, Evidence: "merged", Agent: "next", Started: started}},
		{name: "merged without the parked pull", pull: pull,
			prev: []Delivery{{Ref: issue, Pulls: []string{"o/r#3"}, Step: DeliveryDone, Outcome: OutcomeMerged, Agent: "old", Started: started}},
			want: Delivery{Ref: issue, Pulls: []string{pull, "o/r#3"}, Step: DeliveryDone, Outcome: OutcomeMerged, Agent: "next", Started: started}},
		{name: "still in review", pull: "O/R#2",
			prev: []Delivery{{Ref: issue, Pulls: []string{pull}, Step: DeliveryReviewing, Agent: "old", Started: started}},
			want: Delivery{Ref: issue, Pulls: []string{pull}, Step: DeliveryReviewing, Agent: "next", Started: started}},
		{name: "ended without a change", pull: pull, fresh: true,
			prev: []Delivery{{Ref: issue, Pulls: []string{"o/r#3"}, Step: DeliveryDone, Outcome: OutcomeNoChange, Agent: "old", Started: started}},
			want: Delivery{Ref: issue, Pulls: []string{pull}, Step: DeliveryImplementing, Agent: "next", Started: at, Since: at}},
		{name: "none yet", fresh: true,
			want: Delivery{Ref: issue, Pulls: []string{}, Step: DeliveryImplementing, Agent: "next", Started: at, Since: at}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &State{Deliveries: tc.prev}
			st.ContinueDelivery("O/R#1", "next", tc.pull, at)
			if len(st.Deliveries) != 1 || !reflect.DeepEqual(st.Deliveries[0], tc.want) {
				t.Fatalf("got %+v, want %+v", st.Deliveries, tc.want)
			}
		})
	}
}
