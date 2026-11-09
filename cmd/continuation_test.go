package cmd

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const continuationOldSession = "continuation-old-session"
const continuationNewSession = "continuation-new-session"
const continuationTaskRef = "o/r#12"

func TestParkWakeQueuesFreshContinuationWithoutHarness(t *testing.T) {
	for _, harness := range []string{claudeComm, config.CodexHarness, harnessAgy} {
		t.Run(harness, func(t *testing.T) {
			w := parkingWatch(t, nil)
			if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents[0].Harness = harness
				st.Agents[0].Task = recoveryTask
				st.Agents[0].Park = &state.Park{At: w.now, On: "#3", Waits: "answer"}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := w.wakeAgent(context.Background(), state.Party{Name: resumer}, agentOne, parkAnswer, ""); err != nil {
					t.Fatal(err)
				}
			}
			ag := agentOf(t, w)
			if ag.Park == nil || len(ag.Park.Continuation) != 1 || !strings.Contains(ag.Park.Continuation[0], parkAnswer) {
				t.Fatalf("event not retained once: %+v", ag.Park)
			}
		})
	}
}

func TestContinuationStartupRetainsEventsOnCapacityRefusal(t *testing.T) {
	r, _ := fillRig(t)
	r.a.cfg.LeaseDir = t.TempDir()
	ag := state.Agent{Party: state.Party{Name: holderParked, Session: continuationOldSession}, Task: recoveryTask,
		Park: &state.Park{At: r.a.now, On: "#3", Waits: "answer", Continuation: []string{parkAnswer}}}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) { st.Agents = []state.Agent{ag}; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	r.answer = func(args []string) (string, int) {
		if args[0] == fillCapacityCommand {
			return fillNoRoomJSON, 0
		}
		if args[0] == agentsName && args[1] == agentStartName {
			return "capacity full", ExitRefused
		}
		t.Fatalf("unexpected command %v", args)
		return "", 1
	}
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := r.a.store.Read()
	if err := l.continueParks(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	st, _ = r.a.store.Read()
	if len(st.Agents) != 1 || len(st.Agents[0].Park.Continuation) != 1 || st.Agents[0].Park.Continuing {
		t.Fatalf("task lost: %+v", st.Agents)
	}
}

func TestContinuationTransfersOwnershipOnlyAfterAcceptedStart(t *testing.T) {
	r, _ := fillRig(t)
	r.a.cfg.LeaseDir = t.TempDir()
	old := state.Agent{Party: state.Party{Name: holderParked, Session: continuationOldSession}, Task: recoveryTask, AssignedAt: r.a.now.Add(-time.Hour),
		Park: &state.Park{At: r.a.now, On: "#3", Continuation: []string{parkAnswer}}}
	next := state.Agent{Party: state.Party{Name: "replacement", Session: continuationNewSession}, Undelivered: "retry delivery"}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{old, next}
		st.Records = []state.Record{{Session: old.Party, Issue: continuationTaskRef}}
		st.Grants = []state.Grant{{Resource: labOne, To: old.Party}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	l := &supervisorLoop{a: r.a}
	if err := l.finishContinuation(old, next.Name); err != nil {
		t.Fatal(err)
	}
	st, _ := r.a.store.Read()
	if len(st.Agents) != 2 {
		t.Fatal("retired before task acceptance")
	}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) { st.Agents[1].Undelivered = ""; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if err := l.finishContinuation(old, next.Name); err != nil {
		t.Fatal(err)
	}
	st, _ = r.a.store.Read()
	if len(st.Agents) != 1 || st.Agents[0].Task != old.Task || !st.Records[0].Session.Is(next.Party) || !st.Grants[0].To.Is(next.Party) {
		t.Fatalf("ownership not handed over: %+v", st.Agents)
	}
}

func TestContinuationRefusesNewEventsDuringStartup(t *testing.T) {
	w := parkingWatch(t, nil)
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Park = &state.Park{At: w.now, Continuing: true}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	err := w.queueContinuation(state.Party{Name: resumer}, agentOf(t, w), parkAnswer)
	if err == nil || !strings.Contains(err.Error(), "retry event delivery") {
		t.Fatalf("event acknowledged without delivery: %v", err)
	}
}

func TestReviewWaitIgnoresLegacyMechanicalTimers(t *testing.T) {
	w := parkingWatch(t, nil)
	woke := stubWakes(t)
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Park = &state.Park{At: w.now, On: loopPR, ReviewHead: loopHead, Waits: deskReviewWait}
		recordAgentReviewPark(st, st.Agents[0])
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	w.wakeTimer(context.Background(), timerFire{t: state.Timer{ID: 1, Wake: agentOne, When: "pr-checks " + loopPR}, reason: "fixture condition settled"})
	if len(*woke) != 0 || len(agentOf(t, w).Park.Continuation) != 0 {
		t.Fatal("mechanical review event used a worker turn")
	}
}

func TestStoppedExecutionRetainsItsPendingContinuation(t *testing.T) {
	a, r := executionTestApp(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Park = &state.Park{At: a.now, Continuation: []string{parkAnswer}}
		st.Starts[0].Execution.Observation.StopAcknowledged = true
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	st, _ := a.store.Read()
	if why := stoppedExecutionWhy(st, st.Agents[0]); why != "" {
		t.Fatalf("%s lost its queued task: %s", r.Name, why)
	}
}

func TestContinuationStartsFreshOnceWithTaskAndEvent(t *testing.T) {
	r, _ := fillRig(t)
	r.a.cfg.LeaseDir = t.TempDir()
	old := state.Agent{Party: state.Party{Name: holderParked, Session: continuationOldSession}, Task: recoveryTask,
		Park: &state.Park{At: r.a.now, On: "#3", Waits: noteDecision, Continuation: []string{parkAnswer}}}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) { st.Agents = []state.Agent{old}; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	starts := 0
	r.answer = func(args []string) (string, int) {
		if args[0] == agentsName && args[1] == agentStartName {
			starts++
			brief, err := readStateFixture(r.a.store.Dir(), args[3])
			if err != nil || !strings.Contains(string(brief), old.Task) || !strings.Contains(string(brief), parkAnswer) || !slices.Contains(args, harnessAuto) {
				t.Fatalf("continuation brief: %s; %v", brief, err)
			}
			r.started(t, args[2], continuationNewSession)
		}
		return "", 0
	}
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		st, _ := r.a.store.Read()
		if err := l.continueParks(context.Background(), st); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := r.a.store.Read()
	if starts != 1 || len(st.Agents) != 1 || st.Agents[0].Session != continuationNewSession || st.Agents[0].Task != old.Task {
		t.Fatalf("takeover: starts=%d agents=%+v", starts, st.Agents)
	}
}

func TestLateWakeFollowsFreshContinuationInsteadOfOldTranscript(t *testing.T) {
	old := state.Party{Name: "previous worker", Session: continuationOldSession}
	next := state.Party{Name: "current worker", Session: continuationNewSession}
	st := &state.State{Starts: []state.Start{{Party: old, Successor: &next}}, Agents: []state.Agent{{Party: next, Task: recoveryTask}}}
	a := &app{}
	for _, q := range []string{old.Name, old.Session} {
		ag, rostered, err := a.wakeParty(st, q)
		if err != nil || !rostered || !ag.Is(next) {
			t.Fatalf("late wake reached an old transcript: %+v %v", ag, err)
		}
	}
	st.Agents = nil
	st.Starts = append(st.Starts, state.Start{Party: next, Successor: &old})
	if _, _, err := a.wakeParty(st, old.Session); err == nil {
		t.Fatal("successor cycle resumed a transcript")
	}
}

func TestLoopReconcilesQueuedContinuationAfterRestart(t *testing.T) {
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.a.store.Record(event(loopParty, continuationQueuedEvent, "%s: findings retained", loopWorker)); err != nil {
		t.Fatal(err)
	}
	st, _ := r.a.store.Read()
	_, done, err := l.reconcile(context.Background(), st, superloop.Action{Kind: superloop.KindRed}, loopClaim{target: loopWorker, at: r.a.now.Add(-time.Minute)})
	if err != nil || !done {
		t.Fatalf("queued delivery replayed after restart: %v, %v", done, err)
	}
}

func TestContinuationWithoutChoicesUsesGatedDefaultStart(t *testing.T) {
	r, _ := fillRig(t)
	r.a.cfg.Capacity.Choices = nil
	r.a.cfg.LeaseDir = t.TempDir()
	old := state.Agent{Party: state.Party{Name: holderParked, Session: continuationOldSession}, Task: recoveryTask,
		Park: &state.Park{At: r.a.now, Continuation: []string{parkAnswer}}}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) { st.Agents = []state.Agent{old}; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	started := false
	r.answer = func(args []string) (string, int) {
		if args[0] == agentsName && args[1] == agentStartName {
			if slices.Contains(args, harnessAuto) {
				t.Fatal("forced an unavailable choice pool")
			}
			started = true
			r.started(t, args[2], continuationNewSession)
		}
		return "", 0
	}
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := r.a.store.Read()
	if err := l.continueParks(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if !started {
		t.Fatal("legacy desk lost its fresh continuation")
	}
}
