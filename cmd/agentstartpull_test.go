package cmd

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A worker whose brief names its task by a pull request URL works that pull
// request: its delivery holds it from the start, so its idle --done after the
// merge is accepted although no sweep of the desk's pull requests read it. A
// brief that names a pull request only beside its issue, and a reviewer of a
// pull request, work no pull request.
//
// control: none: briefServedPulls and startPullDeliveries are new, so the
// base cannot build this test; the scenario
// delivery/pull-request-task-finishes-with-its-merge fails on the base
// instead.
func TestPullRequestTaskFinishesWithItsMerge(t *testing.T) {
	const pull = "example/app#7"
	for _, tc := range []struct {
		name, brief, kind string
		served, pulls     []string
		wantDelivery      bool
	}{
		{name: "pull request task", brief: "Prove https://github.com/example/app/pull/7 on main.", served: []string{pull}, pulls: []string{pull}, wantDelivery: true},
		{name: "issue task", brief: "Fix https://github.com/example/app/issues/3.\nSee https://github.com/example/app/pull/7 for context.", served: []string{"example/app#3"}},
		{name: "reviewer", brief: "Review https://github.com/example/app/pull/7.", kind: capacity.TaskKindReview, served: []string{pull}, pulls: []string{pull}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := stubApp(t)
			served := briefIssues(tc.brief)
			pulls := briefServedPulls(tc.brief, served)
			if !slices.Equal(served, tc.served) || !slices.Equal(pulls, tc.pulls) {
				t.Fatalf("serves %v and pulls %v, want %v and %v", served, pulls, tc.served, tc.pulls)
			}
			who := state.Party{Session: "sess-pull-task", Name: "app prover"}
			st := &state.State{}
			s := state.Start{Party: who, By: who, At: a.now, Kind: tc.kind}
			// What app.recordStart does for every start path.
			if _, err := recordStart(st, s, tc.brief, strings.Join(served, ","), func(p state.Party) bool { return p.Is(who) }); err != nil {
				t.Fatal(err)
			}
			startPullDeliveries(st, s, pulls)
			holds := slices.ContainsFunc(st.Deliveries, func(d state.Delivery) bool { return slices.Contains(d.Pulls, pull) })
			if holds != tc.wantDelivery {
				t.Fatalf("a delivery holds %s: %v, want %v (%+v)", pull, holds, tc.wantDelivery, st.Deliveries)
			}
			if tc.kind == capacity.TaskKindReview && len(st.Deliveries) != 0 {
				t.Fatalf("a reviewer owns no delivery, got %+v", st.Deliveries)
			}
			if !tc.wantDelivery {
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
