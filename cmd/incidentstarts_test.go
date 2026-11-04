package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	incidentRepairIssue = "o/r#3"
	incidentReason      = "merge engine unavailable"
	incidentExceptFlag  = "--except"
	incidentModeFlag    = "--incident"
	incidentFixerKind   = "fixer"
	incidentRoomJSON    = `{"room":1}`
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

// Save the hold after the initial incident check, during an external read.
// No real capacity command, GitHub request, or agent start is performed.
func TestIncidentHoldSetDuringStartPreparation(t *testing.T) {
	for _, kind := range []string{workerRole, litReviewer, incidentFixerKind} {
		for _, repair := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/repair=%t", kind, repair), func(t *testing.T) {
				r, _ := fillRig(t)
				r.a.cfg.Agents.Models = []config.AgentModelRule{{Match: ".*", Model: litTestModel}}
				inserted := false
				setHold := func() {
					inserted = true
					except := incidentRepairIssue
					if repair {
						except = loopPR
					}
					if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
						st.Holds = append(st.Holds, state.Hold{Target: loopMergeRepo, Except: except, Incident: true, Reason: incidentReason})
						return nil, nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				r.answer = func(args []string) (string, int) {
					if args[0] == loopCapacity {
						if kind != incidentFixerKind {
							setHold()
						}
						return incidentRoomJSON, 0
					}
					return "", 0
				}
				if kind == incidentFixerKind {
					readLoopPull = func(context.Context, string) (superloop.Pull, error) {
						setHold()
						return r.pull, nil
					}
				}
				l := &supervisorLoop{a: r.a, said: map[string]bool{}}
				act := superloop.Action{Ref: loopPR, URL: loopURL, Key: "incident preparation", Head: loopHead, Round: 1}
				var err error
				switch kind {
				case "worker":
					_, err = l.startItem(context.Background(), act, loopBoardItem{Ref: loopPR, URL: loopURL})
				case litReviewer:
					_, err = l.startReviewer(context.Background(), act)
				case incidentFixerKind:
					err = l.startFixer(context.Background(), &state.State{}, act)
				}
				if !inserted {
					t.Fatal("test did not reach the preparation read")
				}
				if repair {
					if err != nil || len(r.logged(t, loopClaimVerb)) != 1 || r.called(agentsName, agentStartName) == nil {
						t.Fatalf("repair should start: %v; calls %v", err, r.calls)
					}
					return
				}
				if !errors.Is(err, errLoopLater) {
					t.Fatalf("held start returned %v, want errLoopLater", err)
				}
				if len(r.logged(t, loopClaimVerb)) != 0 {
					t.Fatal("held start claimed the action")
				}
				for _, args := range r.calls {
					if len(args) > 1 && args[0] == agentsName && args[1] == agentStartName {
						t.Fatalf("held start dispatched: %v", args)
					}
				}
			})
		}
	}
}
