package cmd

import (
	"context"
	"slices"
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

// A worker whose brief names its task by a pull request URL works that pull
// request: its delivery holds it from the start, so its idle --done after the
// merge is accepted although no sweep of the desk's pull requests read it. A
// brief that names a pull request only beside its issue, and a reviewer of a
// pull request, work no pull request.
//
// control: none: briefServedPulls and recordStart's pulls are new, so the
// base cannot build this test; the scenario
// delivery/pull-request-task-finishes-with-its-merge fails on the base
// instead.
func TestPullRequestTaskFinishesWithItsMerge(t *testing.T) {
	const pull = "example/app#7"
	for _, tc := range []struct {
		name, brief, kind string
		served, pulls     []string
	}{
		{name: "pull request task", brief: "Prove https://github.com/example/app/pull/7 on main.", served: []string{pull}, pulls: []string{pull}},
		{name: "issue task", brief: "Fix https://github.com/example/app/issues/3.\nSee https://github.com/example/app/pull/7 for context.", served: []string{"example/app#3"}},
		{name: "review", brief: "Review https://github.com/example/app/pull/7.", kind: capacity.TaskKindReview, served: []string{pull}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := stubApp(t)
			served := briefIssues(tc.brief)
			if !slices.Equal(served, tc.served) || !slices.Equal(briefServedPulls(tc.brief, served), tc.pulls) {
				t.Fatalf("serves %v and pulls %v, want %v and %v", served, briefServedPulls(tc.brief, served), tc.served, tc.pulls)
			}
			who := state.Party{Session: "sess-pull-task", Name: "app prover"}
			st := &state.State{}
			s := state.Start{Party: who, By: who, At: a.now, Kind: tc.kind}
			if _, err := recordStart(st, s, tc.brief, strings.Join(served, ","), func(p state.Party) bool { return p.Is(who) }, briefServedPulls(tc.brief, served)...); err != nil {
				t.Fatal(err)
			}
			holds := slices.ContainsFunc(st.Deliveries, func(d state.Delivery) bool { return slices.Contains(d.Pulls, pull) })
			if holds != (len(tc.pulls) > 0) {
				t.Fatalf("a delivery holds %s: %v, want %v (%+v)", pull, holds, len(tc.pulls) > 0, st.Deliveries)
			}
			if len(tc.pulls) == 0 {
				return
			}
			merged := func(context.Context, []github.PR) (map[github.PR]github.RefState, error) {
				return map[github.PR]github.RefState{{Repo: "example/app", N: 7}: {Kind: github.RefPullRequest, State: github.Merged}}, nil
			}
			if _, err := a.completionOutcomeWith(t.Context(), st, st.Agents[len(st.Agents)-1], true, "", false, "", merged); err != nil {
				t.Fatalf("idle --done after the merge refused: %v (%+v)", err, st.Deliveries)
			}
			if d := st.Deliveries[0]; d.Outcome != state.OutcomeMerged || d.Evidence != "https://github.com/example/app/pull/7" {
				t.Fatalf("outcome %q with evidence %q, want merged with the pull request", d.Outcome, d.Evidence)
			}
		})
	}
}
