package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	incidentRepairIssue = "o/r#3"
	incidentReason      = "merge engine unavailable"
	incidentExceptFlag  = "--except"
	incidentModeFlag    = "--incident"
)

func TestIncidentBoardSkipsOtherWorkButOffersRepair(t *testing.T) {
	st := &state.State{Holds: []state.Hold{{Target: loopMergeRepo, Except: incidentRepairIssue, Incident: true}}}
	sc := pickScope{listed: time.Now(), alive: func(state.Party) bool { return false }}
	cands := boardCandidates(3)
	res := nextFree(st, cands, sc)
	if res.Pick == nil || res.Pick.Ref != incidentRepairIssue {
		t.Fatalf("repair not offered: %+v", res)
	}
	cands[2] = board.Candidate{Item: board.Item{Ref: "other/repo#1"}, Step: stepUpNext}
	res = nextFree(st, cands, sc)
	if res.Pick == nil || res.Pick.Ref != "other/repo#1" {
		t.Fatalf("other repository blocked: %+v", res)
	}
}

func TestIncidentHoldRequiresNamedRepairAndNoExpiry(t *testing.T) {
	run := holdRunner(t, "")
	for _, args := range [][]string{
		{setOp, holdMerges, incidentModeFlag, incidentExceptFlag, loopPR, "-r", incidentReason},
		{setOp, loopMergeRepo, incidentModeFlag, "-r", incidentReason},
		{setOp, loopMergeRepo, incidentModeFlag, incidentExceptFlag, loopPR, untilFlag, "1h", "-r", incidentReason},
		{setOp, loopMergeRepo, incidentModeFlag, incidentExceptFlag, "o/s#7", "-r", incidentReason},
	} {
		if out, code := run(args...); code != ExitUsage {
			t.Fatalf("%v: %d %s", args, code, out)
		}
	}
	if out, code := run(setOp, loopMergeRepo, incidentModeFlag, incidentExceptFlag, loopPR, "-r", incidentReason); code != 0 || !strings.Contains(out, "starts paused") {
		t.Fatalf("%d %s", code, out)
	}
	if out, code := run("list"); code != 0 || !strings.Contains(out, "starts paused") {
		t.Fatalf("%d %s", code, out)
	}
}

func TestIncidentStartsRestrictOnlyHeldRepositoryUntilLift(t *testing.T) {
	r := newLoopRig(t)
	h := state.Hold{Target: loopMergeRepo, Except: loopPR, Incident: true, Reason: "merge broken", At: time.Now()}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) { st.Holds = append(st.Holds, h); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 7, 999} {
		_, blocked := merge.Blocking(st, time.Now(), loopMergeRepo, n, r.a.cfg.LaneOf(loopMergeRepo))
		if blocked != (n == 999) {
			t.Fatalf("incident merge gate for %d: blocked=%t", n, blocked)
		}
	}
	other := loopMergeRepo + "#999"
	if why := incidentStartReason(st, time.Now(), other); why == "" {
		t.Fatal("unrelated work admitted")
	}
	for _, ref := range []string{loopPR, "another/repo#999"} {
		if why := incidentStartReason(st, time.Now(), ref); why != "" {
			t.Fatalf("%s blocked: %s", ref, why)
		}
	}
	l := &supervisorLoop{a: r.a, said: map[string]bool{}}
	act := superloop.Action{Ref: other}
	if _, err := l.startReviewer(context.Background(), act); !errors.Is(err, errLoopLater) {
		t.Fatalf("review: %v", err)
	}
	if err := l.startFixer(context.Background(), st, act); !errors.Is(err, errLoopLater) {
		t.Fatalf("fixer: %v", err)
	}
	if _, err := l.startItem(context.Background(), act, loopBoardItem{Ref: other}); !errors.Is(err, errLoopLater) {
		t.Fatalf("worker: %v", err)
	}
	for _, c := range r.calls {
		if len(c) > 1 && c[0] == agentsName && c[1] == agentStartName {
			t.Fatalf("started: %v", c)
		}
	}
	// A repair merging does not remove the hold. Only the release/proof's
	// explicit lift (or successful configured probe) restores dispatch.
	r.pull.State = superloop.Merged
	if err := l.incidentStart(other); !errors.Is(err, errLoopLater) {
		t.Fatalf("merged prematurely resumed starts: %v", err)
	}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) { st.Holds = nil; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if err := l.incidentStart(other); err != nil {
		t.Fatal(err)
	}
	h.Incident = false
	st.Holds = []state.Hold{h}
	if why := incidentStartReason(st, time.Now(), other); why != "" {
		t.Fatalf("ordinary hold blocked starts: %s", why)
	}
}
