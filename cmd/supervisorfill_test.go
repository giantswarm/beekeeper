package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	fillCapacityCommand = "capacity"
	fillEmptyBoardJSON  = `{"pick":null}`
	fillNoRoomJSON      = `{"floor":3,"room":0}`
	fillAgentDone       = "an agent is done"
)

// Only external commands are replaced; capacity changes after each start.
func fillRig(t *testing.T) (*loopRig, *int) {
	t.Helper()
	r := newLoopRig(t)
	brief := filepath.Join(t.TempDir(), "worker.md")
	if err := os.WriteFile(brief, []byte("Work {{.URL}}."), 0o600); err != nil {
		t.Fatal(err)
	}
	r.a.cfg.Supervisor.WorkerBrief = brief
	r.a.cfg.Supervisor.FillRetryEvery.Duration = 5 * time.Minute
	r.a.cfg.Capacity.Choices = []config.Choice{{Harness: config.CodexHarness, Model: "test-model"}}
	busy := 0
	r.answer = func(args []string) (string, int) {
		switch args[0] {
		case fillCapacityCommand:
			out, err := json.Marshal(capacityView{Floor: 3, Ceiling: 5, Room: 5 - busy, agentCount: agentCount{Busy: make([]string, busy)}})
			if err != nil {
				t.Fatal(err)
			}
			return string(out), 0
		case loopBoard:
			return fmt.Sprintf(`{"pick":{"ref":"o/r#%d","url":"https://github.com/o/r/issues/%d"}}`, busy+10, busy+10), 0
		case agentsName:
			if args[1] == agentStartName {
				busy++
				r.started(t, args[2], "fill-"+args[2])
			}
		}
		return "", 0
	}
	return r, &busy
}

func TestLoopFillOnStartupAndAfterRosterLoss(t *testing.T) {
	r, busy := fillRig(t)
	r.pass(t)
	if *busy != 3 || len(r.logged(t, "supervisor.next")) != 3 {
		t.Fatalf("startup: busy %d, %s", *busy, r.out)
	}
	for _, c := range r.calls {
		if c[0] == agentsName && c[1] == agentStartName && !slices.Contains(c, harnessAuto) {
			t.Fatalf("lost capacity pick: %v", c)
		}
	}
	r.pass(t)
	if r.called(agentsName, agentStartName) != nil {
		t.Fatal("started above floor")
	}
	original := r.answer
	r.answer = func(args []string) (string, int) {
		if args[0] == loopBoard {
			return `{"pick":{"ref":"o/r#10","url":"https://github.com/o/r/issues/10"}}`, 0
		}
		return original(args)
	}
	*busy = 2 // a crash or removal without agents.done
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = slices.DeleteFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == "r 10" })
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	if *busy != 3 {
		t.Fatalf("did not refill roster loss: %s", r.out)
	}
	evs := r.logged(t, "supervisor.next")
	if !strings.Contains(evs[len(evs)-1].Detail, "o/r#10 attempt 2") {
		t.Fatalf("did not retry the freed item: %+v", evs)
	}
}

func TestLoopFillStopsAtGatesAndEmptyBoard(t *testing.T) {
	for _, name := range []string{"capacity refused", "no room", "at floor", "empty board", "bad capacity", "bad board", "start refused", "no brief", "dry run"} {
		t.Run(name, func(t *testing.T) {
			r, busy := fillRig(t)
			original := r.answer
			r.answer = func(args []string) (string, int) {
				if args[0] == fillCapacityCommand {
					switch name {
					case "capacity refused":
						return "pace or host pressure", ExitRefused
					case "no room":
						return fillNoRoomJSON, 0
					case "at floor":
						return `{"floor":0,"room":5}`, 0
					case "bad capacity":
						return "broken JSON", 0
					}
				}
				if args[0] == loopBoard {
					switch name {
					case "empty board":
						return fillEmptyBoardJSON, 0
					case "bad board":
						return `{"pick":{"ref":"invalid"}}`, 0
					}
				}
				if args[0] == agentsName && name == "start refused" {
					return "pace changed", ExitRefused
				}
				return original(args)
			}
			if name == "no brief" {
				r.a.cfg.Supervisor.WorkerBrief = ""
			}
			if name == "dry run" {
				if err := r.a.dryRunSupervisorLoop(t.Context()); err != nil {
					t.Fatal(err)
				}
			} else {
				r.pass(t)
			}
			if *busy != 0 || len(r.logged(t, "supervisor.next")) != 0 {
				t.Fatalf("started despite %s: %s", name, r.out)
			}
		})
	}
}

func TestLoopFillRechecksCapacityBetweenStarts(t *testing.T) {
	r, busy := fillRig(t)
	original := r.answer
	r.answer = func(args []string) (string, int) {
		if args[0] == fillCapacityCommand && *busy > 0 {
			return fillNoRoomJSON, 0
		}
		return original(args)
	}
	r.pass(t)
	if *busy != 1 {
		t.Fatalf("ignored changed gate: %d", *busy)
	}
}

func TestLoopFillRecoversInterruptedStart(t *testing.T) {
	r, busy := fillRig(t)
	r.failRecord = func(e state.Event) bool { return e.Verb == "supervisor.next" }
	if err := r.passErr(); err == nil {
		t.Fatal("expected failed completion record")
	}
	if *busy != 1 {
		t.Fatalf("starts before interruption: %d", *busy)
	}
	r.pass(t) // a fresh loop, with the board offering a different item
	if *busy != 3 {
		t.Fatalf("recovery: busy %d, %s", *busy, r.out)
	}
	evs := r.logged(t, "supervisor.next")
	if len(evs) != 3 || !strings.Contains(evs[0].Detail, "taken before an interruption") {
		t.Fatalf("lost claim: %+v", evs)
	}
}

func TestLoopFillBoundsStaleCapacity(t *testing.T) {
	r, busy := fillRig(t)
	original := r.answer
	r.answer = func(args []string) (string, int) {
		if args[0] == fillCapacityCommand {
			return `{"floor":3,"room":2}`, 0
		}
		return original(args)
	}
	r.pass(t)
	if *busy != 2 {
		t.Fatalf("ignored initial room bound: %d", *busy)
	}
}

func TestLoopFillNeverRepeatsStaleBoardItem(t *testing.T) {
	r, busy := fillRig(t)
	original := r.answer
	r.answer = func(args []string) (string, int) {
		if args[0] == loopBoard {
			return `{"pick":{"ref":"o/r#30","url":"https://github.com/o/r/issues/30"}}`, 0
		}
		return original(args)
	}
	r.pass(t)
	r.pass(t)
	if *busy != 1 {
		t.Fatalf("repeated a stale board item: %d", *busy)
	}
}

func TestLoopFillBacksOffEmptyAndFailedBoardReads(t *testing.T) {
	for _, answer := range []struct {
		name, body string
		code       int
	}{
		{"board has no free item", fillEmptyBoardJSON, 0},
		{"board read failure", "board unavailable", 1},
		{"invalid ref", `{"pick":{"ref":"invalid"}}`, 0},
		{"malformed board response", "not JSON", 0},
	} {
		t.Run(answer.name, func(t *testing.T) {
			r, _ := fillRig(t)
			original := r.answer
			reads := 0
			r.answer = func(args []string) (string, int) {
				if args[0] == loopBoard {
					reads++
					return answer.body, answer.code
				}
				return original(args)
			}
			l, err := r.a.newSupervisorLoop(false)
			if err != nil {
				t.Fatal(err)
			}
			pass := func() {
				t.Helper()
				if err := l.pass(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			pass()
			r.a.now = r.a.now.Add(30 * time.Second)
			pass()
			if reads != 1 {
				t.Fatalf("unchanged pass read the board %d times", reads)
			}
			r.a.now = r.a.now.Add(r.a.cfg.Supervisor.FillRetryEvery.Duration)
			pass()
			if reads != 2 {
				t.Fatalf("retry interval did not retry: %d reads", reads)
			}
		})
	}
}

func TestLoopFillStateChangesInvalidateBoardBackoff(t *testing.T) {
	for _, change := range []string{"busy count changes", "available room changes", "an agent is removed", fillAgentDone, "capacity cycles through the floor"} {
		t.Run(change, func(t *testing.T) {
			r, busy := fillRig(t)
			original := r.answer
			reads := 0
			changedRoom := false
			r.answer = func(args []string) (string, int) {
				if args[0] == loopBoard {
					reads++
					return fillEmptyBoardJSON, 0
				}
				if args[0] == fillCapacityCommand && changedRoom {
					return `{"floor":3,"room":1}`, 0
				}
				return original(args)
			}
			r.started(t, "a worker", "a-session")
			l, err := r.a.newSupervisorLoop(false)
			if err != nil {
				t.Fatal(err)
			}
			if err := l.pass(t.Context()); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "busy count changes":
				*busy = 1
			case "available room changes":
				changedRoom = true
			case "an agent is removed", fillAgentDone:
				if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
					i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == "a worker" })
					if change == fillAgentDone {
						st.Agents[i].Done = true
					} else {
						st.Agents = slices.Delete(st.Agents, i, i+1)
					}
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
			case "capacity cycles through the floor":
				*busy = 3
				if err := l.pass(t.Context()); err != nil {
					t.Fatal(err)
				}
				*busy = 0
			}
			r.a.now = r.a.now.Add(time.Second)
			if err := l.pass(t.Context()); err != nil {
				t.Fatal(err)
			}
			if reads != 2 {
				t.Fatalf("%s did not invalidate backoff: %d reads", change, reads)
			}
		})
	}
}

// emptyBoardFill is a refill rig whose board offers nothing, counting reads.
func emptyBoardFill(t *testing.T) (*loopRig, *supervisorLoop, *int) {
	t.Helper()
	r, _ := fillRig(t)
	original := r.answer
	reads := 0
	r.answer = func(args []string) (string, int) {
		if args[0] == loopBoard {
			reads++
			return fillEmptyBoardJSON, 0
		}
		return original(args)
	}
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	return r, l, &reads
}

func TestLoopFillPauseHoldsThroughRosterChurn(t *testing.T) {
	r, l, reads := emptyBoardFill(t)
	r.started(t, "a worker", "a-session")
	retry := r.a.cfg.Supervisor.FillRetryEvery.Duration
	start := r.a.now
	for i := 0; r.a.now.Before(start.Add(2 * retry)); i++ {
		// Every pass a worker parks, resumes, re-parks, changes session or
		// task, and another joins: none of it makes room.
		if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
			for j := range st.Agents {
				ag := &st.Agents[j]
				ag.Session = fmt.Sprintf("session-%d", i)
				ag.Task = fmt.Sprintf("task %d", i)
				ag.Park = nil
				if i%2 == 0 {
					ag.Park = &state.Park{On: fmt.Sprintf("o/r#%d", i)}
				}
			}
			st.Agents = append(st.Agents, state.Agent{Party: state.Party{Name: fmt.Sprintf("joiner %d", i)}})
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := l.pass(t.Context()); err != nil {
			t.Fatal(err)
		}
		r.a.now = r.a.now.Add(30 * time.Second)
	}
	if *reads != 2 {
		t.Fatalf("roster churn over two retry intervals read the board %d times, want 2", *reads)
	}
}

func TestLoopPassSharesAnEmptyBoardRead(t *testing.T) {
	r, l, reads := emptyBoardFill(t)
	l.boardNone = nil // a pass's start
	act := superloop.Action{Kind: superloop.KindNext, Key: "next after r 1"}
	what, err := l.startNext(t.Context(), act)
	if err != nil || !strings.Contains(what, "the board offers nothing") {
		t.Fatalf("done-triggered start: %q, %v", what, err)
	}
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if err := l.fill(t.Context(), st, nil, nil); err != nil {
		t.Fatal(err)
	}
	if *reads != 1 || l.fillPause.until.IsZero() {
		t.Fatalf("one pass: %d board reads, refill paused %v", *reads, !l.fillPause.until.IsZero())
	}
	r.a.now = r.a.now.Add(time.Second)
	if err := l.pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if *reads != 1 {
		t.Fatalf("the refill's pause did not hold the next pass: %d reads", *reads)
	}
}

func TestLoopFillFailedStartClosesClaim(t *testing.T) {
	for _, code := range []int{ExitRefused, 1} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			r, _ := fillRig(t)
			original := r.answer
			r.answer = func(args []string) (string, int) {
				if args[0] == agentsName && args[1] == agentStartName {
					return "start unavailable", code
				}
				return original(args)
			}
			r.pass(t)
			if evs := r.logged(t, loopClaimEndVerb); len(evs) != 1 {
				t.Fatalf("failed claim not closed: %+v", evs)
			}
			// A later unrelated start with the same name must not settle the
			// failed attempt as though it had started before an interruption.
			if err := r.a.store.Record(event(loopParty, "agents.start", "r 10: later start")); err != nil {
				t.Fatal(err)
			}
			r.answer = func(args []string) (string, int) {
				if args[0] == fillCapacityCommand {
					return fillNoRoomJSON, 0
				}
				return original(args)
			}
			r.pass(t)
			if evs := r.logged(t, "supervisor.next"); len(evs) != 0 {
				t.Fatalf("false reconciliation: %+v", evs)
			}
		})
	}
}

func TestLoopFillDryRunPreviewsWithoutStartingOrWriting(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprint(empty), func(t *testing.T) {
			r, busy := fillRig(t)
			original := r.answer
			boardReads, capacityReads := 0, 0
			r.answer = func(args []string) (string, int) {
				if args[0] == fillCapacityCommand {
					capacityReads++
				}
				if args[0] == loopBoard {
					boardReads++
					if empty {
						return fillEmptyBoardJSON, 0
					}
				}
				return original(args)
			}
			before, err := r.a.store.Events(0, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.a.dryRunSupervisorLoop(t.Context()); err != nil {
				t.Fatal(err)
			}
			after, err := r.a.store.Events(0, nil)
			if err != nil {
				t.Fatal(err)
			}
			count := 3
			if empty {
				count = 0
			}
			if !strings.Contains(r.out.String(), fmt.Sprintf("SUPERVISOR WOULD NEXT fill %d to capacity.floor", count)) {
				t.Fatalf("no refill preview: %s", r.out)
			}
			if *busy != 0 || len(after) != len(before) || capacityReads != 1 || boardReads != 1 || r.called(agentsName) != nil {
				t.Fatalf("dry run: busy %d, events %d -> %d, reads %d/%d, calls %v", *busy, len(before), len(after), capacityReads, boardReads, r.calls)
			}
		})
	}
}
