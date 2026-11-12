package cmd

import (
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const loopBoardMove = "move"

func TestLoopSettlesReportWithoutRefOnce(t *testing.T) {
	r := newLoopRig(t)
	if err := r.a.store.Record(event(state.Party{Name: loopWorker}, superloop.VerbReport, "%s: finished maintenance", loopWorker)); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		r.pass(t)
	}
	evs := r.logged(t, "supervisor.classify")
	if len(evs) != 1 || !strings.Contains(evs[0].Detail, "names no issue or pull request") {
		t.Fatalf("classification = %+v; output %s", evs, r.out)
	}
	if evs := r.logged(t, LoopFailureVerb); len(evs) != 0 {
		t.Fatalf("failures = %+v", evs)
	}
}

func TestLoopSettlesNonIssueIntakeOnce(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Board = config.Board{Owner: crossOwner, Project: 1}
	r.a.cfg.GitHub.AllowedOrgs = []string{"org"}
	if err := r.a.store.Record(event(state.Party{Name: loopWorker}, superloop.VerbProblem, "%s: filed https://github.com/org/engine/issues/89", loopWorker)); err != nil {
		t.Fatal(err)
	}
	moves := 0
	r.answer = func(args []string) (string, int) {
		if slices.Equal(args, []string{LoopBoard, loopBoardMove, "org/engine#89", "backlog", "--intake"}) {
			moves++
			return "beekeeper: org/engine#89 is no issue: Could not resolve to an Issue with the number of 89.", ExitRefused
		}
		return "", 0
	}
	for range 3 {
		r.pass(t)
	}
	evs := r.logged(t, "supervisor.intake")
	if moves != 1 || len(evs) != 1 || !strings.Contains(evs[0].Detail, "not an issue") {
		t.Fatalf("moves=%d intake=%+v output=%s", moves, evs, r.out)
	}
	if evs := r.logged(t, LoopFailureVerb); len(evs) != 0 {
		t.Fatalf("failures = %+v", evs)
	}
}
