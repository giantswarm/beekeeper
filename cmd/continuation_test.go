package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

func TestParkWakeQueuesFreshContinuationWithoutHarness(t *testing.T) {
	for _, harness := range []string{"claude", "codex", "agy"} {
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
			if ag.Park == nil || len(ag.Park.Continuation) != 1 || ag.Park.Continuation[0] != parkAnswer {
				t.Fatalf("event not retained once: %+v", ag.Park)
			}
		})
	}
}

func TestContinuationStartupRetainsEventsOnCapacityRefusal(t *testing.T) {
	r, _ := fillRig(t)
	r.a.cfg.LeaseDir = t.TempDir()
	ag := state.Agent{Party: state.Party{Name: "parked", Session: "old"}, Task: recoveryTask,
		Park: &state.Park{At: r.a.now, On: "#3", Waits: "answer", Continuation: []string{parkAnswer}}}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) { st.Agents = []state.Agent{ag}; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	r.answer = func(args []string) (string, int) {
		if args[0] == fillCapacityCommand {
			return fillNoRoomJSON, 0
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
	if len(st.Agents) != 1 || len(st.Agents[0].Park.Continuation) != 1 {
		t.Fatalf("task lost: %+v", st.Agents)
	}
}

func TestContinuationTransfersOwnershipOnlyAfterAcceptedStart(t *testing.T) {
	r, _ := fillRig(t)
	r.a.cfg.LeaseDir = t.TempDir()
	old := state.Agent{Party: state.Party{Name: "parked", Session: "old"}, Task: recoveryTask, AssignedAt: r.a.now.Add(-time.Hour),
		Park: &state.Park{At: r.a.now, On: "#3", Continuation: []string{parkAnswer}}}
	next := state.Agent{Party: state.Party{Name: "replacement", Session: "new"}, Undelivered: "retry delivery"}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{old, next}
		st.Records = []state.Record{{Session: old.Party, Issue: "o/r#12"}}
		st.Grants = []state.Grant{{Resource: "lab", To: old.Party}}
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
