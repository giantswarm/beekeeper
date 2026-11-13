package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const outcomeWorkerName = "delivery worker"

func TestWorkerCompletionOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, reason, by, want string
		done, blocked, merged  bool
	}{
		{name: "unexplained", done: true},
		{name: state.OutcomeNoChange, done: true, reason: "already fixed by commit abc123, verified issue response", want: state.OutcomeNoChange},
		{name: state.OutcomeMerged, done: true, merged: true, want: state.OutcomeMerged},
		{name: "blocked note", blocked: true, by: "#7", want: state.OutcomeBlocked},
		{name: "missing note", blocked: true, by: "#8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := stubApp(t)
			ag := state.Agent{Party: state.Party{Name: outcomeWorkerName}, Task: heldRef, AssignedAt: a.now}
			st := &state.State{Notes: []state.Note{{ID: 7}}}
			st.StartDelivery(heldRef, ag.Name, "", a.now)
			f := useFakeBoard(t, a, map[int]string{7: holdWorking})
			if tc.merged {
				st.Deliveries[0].Pulls = []string{alsoHeldRef}
				prev := refStates
				t.Cleanup(func() { refStates = prev })
				refStates = func(_ context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
					return map[github.PR]github.RefState{refs[0]: {Kind: github.RefPullRequest, State: github.Merged}}, nil
				}
			}
			events, err := a.completionOutcome(t.Context(), st, ag, tc.done, tc.reason, tc.blocked, tc.by)
			if tc.want == "" {
				if err == nil {
					t.Fatal("accepted an unexplained end")
				}
				if tc.done && !strings.Contains(err.Error(), taskEndings) {
					t.Fatal(err)
				}
				if st.Deliveries[0].Outcome != "" {
					t.Fatal("refusal mutated outcome")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 1 || st.Deliveries[0].Outcome != tc.want || st.Deliveries[0].Evidence == "" {
				t.Fatalf("outcome: %+v, events: %+v", st.Deliveries[0], events)
			}
			if tc.blocked && (f.statusOf(7) != holdBlocked || st.Notes[0].HoldOf(heldRef) == nil) {
				t.Fatal("blocker not recorded on board and note")
			}
			if !tc.merged {
				next := superloop.Deliveries(superloop.Input{Now: a.now}, st.Deliveries)
				if next[0].Outcome != tc.want || next[0].Step != st.Deliveries[0].Step {
					t.Fatalf("observation lost outcome: %+v", next)
				}
			}
		})
	}
}

func TestParkRecordsDeliveryOutcome(t *testing.T) {
	a, _ := stubApp(t)
	ag := state.Agent{Party: state.Party{Name: outcomeWorkerName}, AssignedAt: a.now}
	st := &state.State{}
	st.StartDelivery(heldRef, ag.Name, "", a.now)
	events := a.recordTaskOutcome(st, ag, state.OutcomeParked, "#7: decision")
	if len(events) != 1 || st.Deliveries[0].Outcome != state.OutcomeParked || !st.Deliveries[0].Active() {
		t.Fatalf("park: %+v", st.Deliveries)
	}
	st.Deliveries[0].Step = state.DeliveryDropped
	st.Deliveries[0].Outcome = state.DeliveryDropped
	st.Deliveries[0].Evidence = "issue closed without a pull request"
	if events := a.recordTaskOutcome(st, ag, state.OutcomeParked, "#7: decision"); len(events) != 0 || st.Deliveries[0].Outcome != state.DeliveryDropped {
		t.Fatalf("park overwrote terminal delivery outcome: %+v %+v", st.Deliveries, events)
	}
}

func TestCompletionRejectsUnmergedAndUnreadPull(t *testing.T) {
	for _, read := range []github.RefState{{Kind: github.RefPullRequest, State: github.Open}, {Kind: github.RefPullRequest, State: github.Closed}, {}} {
		a, _ := stubApp(t)
		ag := state.Agent{Party: state.Party{Name: outcomeWorkerName}, AssignedAt: a.now}
		st := &state.State{}
		st.StartDelivery(heldRef, ag.Name, "", a.now)
		st.Deliveries[0].Pulls = []string{refOne}
		prev := refStates
		refStates = func(_ context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
			return map[github.PR]github.RefState{refs[0]: read}, nil
		}
		_, err := a.completionOutcome(t.Context(), st, ag, true, "", false, "")
		refStates = prev
		if err == nil || st.Deliveries[0].Outcome != "" {
			t.Fatalf("accepted %+v", read)
		}
	}
}

func TestNoChangeIsNotAReleaseStatistic(t *testing.T) {
	a, _ := stubApp(t)
	ag := state.Agent{Party: state.Party{Name: outcomeWorkerName}, AssignedAt: a.now}
	st := &state.State{}
	st.StartDelivery(heldRef, ag.Name, "", a.now)
	events, err := a.completionOutcome(t.Context(), st, ag, true, "operation verified on the issue", false, "")
	if err != nil {
		t.Fatal(err)
	}
	stats := deliveryStatistics(events, a.now, a.now)
	if stats.Released != 0 || stats.Outcomes[state.OutcomeNoChange] != 1 {
		t.Fatalf("statistics: %+v", stats)
	}
}

// TestReviewerOwnsNoDelivery: a review started by hand, without the loop's
// assignment, serves the pull request it reviews, whose delivery no issue
// holds. Its --done needs no merge and reads nothing; its --no-change and
// --blocked end nothing.
func TestReviewerOwnsNoDelivery(t *testing.T) {
	a, _ := stubApp(t)
	reviewer := state.Party{Session: "outcome-reviewer", HostSession: "local_outcome-reviewer", Name: "Review r 1 r1"}
	ag := state.Agent{Party: reviewer, Task: "Review " + refOne, AssignedAt: a.now}
	st := &state.State{
		Starts:  []state.Start{{Kind: capacity.TaskKindReview, Party: reviewer, At: a.now}},
		Records: []state.Record{{Session: reviewer, Issue: refOne, At: a.now}},
	}
	st.StartDelivery(refOne, reviewer.Name, "", a.now)
	st.Deliveries[0].Pulls = []string{refOne}
	reads := 0
	lookup := func(_ context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
		reads++
		return map[github.PR]github.RefState{refs[0]: {Kind: github.RefPullRequest, State: github.Open}}, nil
	}
	if _, err := a.completionOutcomeWith(t.Context(), st, ag, true, "", false, "", lookup); err != nil {
		t.Fatalf("the reviewer's --done: %v", err)
	}
	if _, err := a.completionOutcomeWith(t.Context(), st, ag, true, "no findings", false, "", lookup); err != nil {
		t.Fatalf("the reviewer's --no-change: %v", err)
	}
	if _, err := a.completionOutcomeWith(t.Context(), st, ag, false, "", true, "#7", lookup); err == nil {
		t.Fatal("the reviewer blocked a delivery it does not own")
	}
	if d := st.Deliveries[0]; d.Outcome != "" || !d.Active() || reads != 0 {
		t.Fatalf("the review ended the delivery: %+v after %d reads", d, reads)
	}
	if got := workerDeliveries(st, ag); len(got) != 0 {
		t.Fatalf("the reviewer owns deliveries %v", got)
	}
}
