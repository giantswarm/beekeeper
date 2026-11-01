package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
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

func TestLoopFillStateChangesAndBoardBackoff(t *testing.T) {
	for _, tc := range []struct {
		change string
		reads  int
	}{
		{"the floor changes", 2},
		{"the ceiling changes", 2},
		{"an agent is removed", 2},
		{fillAgentDone, 2},
		// A park or resume moves an agent in or out of busy and room with
		// it; neither can make an empty board offer an item.
		{"busy count changes", 1},
		{"available room changes", 1},
		{"capacity cycles through the floor", 1},
	} {
		change := tc.change
		t.Run(change, func(t *testing.T) {
			r, busy := fillRig(t)
			original := r.answer
			reads := 0
			changedRoom, floor, ceiling := false, 3, 5
			r.answer = func(args []string) (string, int) {
				if args[0] == loopBoard {
					reads++
					return fillEmptyBoardJSON, 0
				}
				if args[0] == fillCapacityCommand {
					room := ceiling - *busy
					if changedRoom {
						room = 1
					}
					return fmt.Sprintf(`{"floor":%d,"ceiling":%d,"room":%d,"busy":%s}`, floor, ceiling, room, busyJSON(*busy)), 0
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
			case "the floor changes":
				floor = 4
			case "the ceiling changes":
				ceiling = 6
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
			if reads != tc.reads {
				t.Fatalf("%s: %d board reads, want %d", change, reads, tc.reads)
			}
		})
	}
}

// busyJSON is a capacity read's busy list of n agents.
func busyJSON(n int) string {
	b, _ := json.Marshal(make([]string, n))
	return string(b)
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
	// Capacity counts the stored roster as the capacity command does: a
	// park moves its agent out of busy and gives room back.
	board := r.answer
	r.answer = func(args []string) (string, int) {
		if args[0] != fillCapacityCommand {
			return board(args)
		}
		st, err := r.a.store.Read()
		if err != nil {
			t.Fatal(err)
		}
		c := countAgents(st, nil, r.a.now)
		out, err := json.Marshal(capacityView{Floor: 3, Ceiling: 5, Room: 5 - len(c.Busy), agentCount: c})
		if err != nil {
			t.Fatal(err)
		}
		return string(out), 0
	}
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

// hostFillRig is fillRig with an execution host configured: the
// coordinator's machine has no room (local capacity --json answers room
// 0), the host has room, and the board offers items of the mapped
// repository.
func hostFillRig(t *testing.T) (*loopRig, *int) {
	t.Helper()
	return hostFillRigRoom(t, 0)
}

// hostFillRigRoom is hostFillRig with localRoom the coordinator's room.
func hostFillRigRoom(t *testing.T, localRoom int) (*loopRig, *int) {
	t.Helper()
	r, busy := fillRig(t)
	r.sshLoopHost(t, true)
	branch := ghDefaultBranch
	t.Cleanup(func() { ghDefaultBranch = branch })
	ghDefaultBranch = func(context.Context, string) (string, error) { return mainBranch, nil }
	original := r.answer
	r.answer = func(args []string) (string, int) {
		switch args[0] {
		case fillCapacityCommand:
			if slices.Contains(args, loopHostFlag) {
				return loopRoomJSON, 0
			}
			out, err := json.Marshal(capacityView{Floor: 3, Ceiling: 5, Room: localRoom, agentCount: agentCount{Busy: make([]string, *busy)}})
			if err != nil {
				t.Fatal(err)
			}
			return string(out), 0
		case loopBoard:
			n := *busy + 10
			return fmt.Sprintf(`{"pick":{"ref":"%s#%d","url":"https://github.com/%s/issues/%d"}}`, sshTestRepository, n, sshTestRepository, n), 0
		}
		return original(args)
	}
	return r, busy
}

// TestLoopFillPlacesOnAHostWhenTheCoordinatorIsFull is the issue's case for
// the capacity fill: the coordinator's machine is at its floor of room, a
// host has room, and the shortfall is filled there rather than not at all.
func TestLoopFillPlacesOnAHostWhenTheCoordinatorIsFull(t *testing.T) {
	r, busy := hostFillRig(t)
	if err := r.a.dryRunSupervisorLoop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.out.String(), "SUPERVISOR WOULD NEXT fill 3 to capacity.floor") {
		t.Fatalf("no refill preview onto the host: %s", r.out)
	}
	r.pass(t)
	if *busy != 3 || len(r.logged(t, "supervisor.next")) != 3 {
		t.Fatalf("the fill did not reach the floor on the host: busy %d\n%s", *busy, r.out)
	}
	for _, c := range r.calls {
		if c[0] == agentsName && c[1] == agentStartName {
			for _, want := range []string{loopHostFlag, sshTestSelector, loopRepoFlag, sshTestRepository, loopRefFlag, mainBranch} {
				if !slices.Contains(c, want) {
					t.Errorf("the fill's start names no %q: %v", want, c)
				}
			}
		}
	}
}

// TestLoopFillStaysBoundByLocalRoomWithoutHosts keeps the fill's local
// bound when nothing can be placed remotely: no capacity.choices means no
// remote start, so the full coordinator starts nothing.
func TestLoopFillStaysBoundByLocalRoomWithoutHosts(t *testing.T) {
	r, busy := hostFillRig(t)
	r.a.cfg.Capacity.Choices = nil
	r.pass(t)
	if *busy != 0 || r.called(agentsName, agentStartName) != nil {
		t.Fatalf("a full coordinator started without a placeable host: %v", r.calls)
	}
}

// TestLoopFillRecoversAReservedPlacedStartBeforeClosingItsClaim covers a
// placed start refused after it reserved its execution: the loop recovers
// it, and the claim stays open until the taken line, so an interruption in
// between still reconciles it. supervisor.hostsFirst places the start on
// the host although the coordinator has room.
func TestLoopFillRecoversAReservedPlacedStartBeforeClosingItsClaim(t *testing.T) {
	r, busy := hostFillRigRoom(t, 5)
	r.a.cfg.Supervisor.HostsFirst = true
	name := repoName(sshTestRepository) + " 10"
	original := r.answer
	recovered := 0
	r.answer = func(args []string) (string, int) {
		if args[0] == agentsName && args[1] == agentStartName && args[2] == name {
			// The start reserved its execution, then was refused before
			// the host accepted the first task.
			if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents = append(st.Agents, state.Agent{Party: state.Party{Name: name}, Execution: &execution.Ref{Host: execution.HostID(sshTestSelector), Execution: "x1", Generation: 1}})
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			return "refused: execution reserved", ExitRefused
		}
		if args[0] == agentsName && args[1] == loopRecover {
			recovered++
			*busy++
			return "recovered", 0
		}
		return original(args)
	}
	r.pass(t)
	if recovered != 1 {
		t.Fatalf("the reserved execution was not recovered: %v\n%s", r.calls, r.out)
	}
	if evs := r.logged(t, loopClaimEndVerb); len(evs) != 0 {
		t.Fatalf("the recovered start's claim was closed as failed: %+v", evs)
	}
	evs := r.logged(t, "supervisor.next")
	if len(evs) == 0 || !strings.Contains(evs[0].Detail, "recovered") {
		t.Fatalf("the recovery is not the start taken: %+v", evs)
	}
}

// boardNothingFree is what board next --json prints with nothing free: its
// document on stdout, the refusal on stderr, and exit 3.
const boardNothingFree = `{"pick":null,"skipped":[{"ref":"o/r#4","skip":"held"}]}` + "\n" +
	"beekeeper: no free board item: the order offered 1, all skipped\n"

// nothingFreeRig is the refill rig whose board answers as board next does
// with nothing free, counting the reads, with doneN agents done.
func nothingFreeRig(t *testing.T, doneN int) (*loopRig, *supervisorLoop, *int) {
	t.Helper()
	r, _ := fillRig(t)
	original := r.answer
	reads := 0
	r.answer = func(args []string) (string, int) {
		if args[0] == loopBoard {
			reads++
			return boardNothingFree, ExitRefused
		}
		return original(args)
	}
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	var evs []state.Event
	for i := range doneN {
		name := fmt.Sprintf("worker %d", i+1)
		e := event(state.Party{Name: name}, "agents.done", "%s done: fix it", name)
		e.At = e.At.Add(time.Duration(i) * time.Microsecond)
		evs = append(evs, e)
	}
	if err := r.a.store.Record(evs...); err != nil {
		t.Fatal(err)
	}
	return r, l, &reads
}

func (r *loopRig) loopPass(t *testing.T, l *supervisorLoop) {
	t.Helper()
	r.a.now = r.a.now.Add(30 * time.Second)
	// The watch keeps publishing: its budget reading never goes stale.
	w := event(watchParty, "watch.budget-state", "false")
	w.At = r.a.now
	if err := r.a.store.Record(w); err != nil {
		t.Fatal(err)
	}
	if err := l.pass(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestLoopSettlesDoneEventsWhenNothingIsFree(t *testing.T) {
	const n = 5
	r, l, reads := nothingFreeRig(t, n)
	r.loopPass(t, l) // the done events take their agents off the roster
	if got := len(r.logged(t, "supervisor.done")); got != n {
		t.Fatalf("%d of %d done events settled:\n%s", got, n, r.out)
	}
	before := *reads
	r.loopPass(t, l) // each done event's next start asks the board
	if got := *reads - before; got != 1 {
		t.Fatalf("%d done events read the board %d times in one pass, want 1", n, got)
	}
	if got := len(r.logged(t, "supervisor.next")); got != n {
		t.Fatalf("%d of %d next starts settled:\n%s", got, n, r.out)
	}
	for _, e := range r.logged(t, "supervisor.next") {
		if !strings.Contains(e.Detail, "the board offers nothing") {
			t.Errorf("next start settled as %q", e.Detail)
		}
	}
	// Within the refill's pause (supervisor.fillRetryEvery) nothing reads.
	for range 8 {
		r.loopPass(t, l)
	}
	if fs := r.logged(t, loopFailureVerb); len(fs) != 0 {
		t.Fatalf("nothing free recorded as failures: %+v", fs)
	}
	if got := *reads - before; got != 1 {
		t.Fatalf("the settled done events or the paused refill read the board again: %d reads", got)
	}
}

func TestLoopReadsNoBoardUnderTheBudgetFloor(t *testing.T) {
	for _, name := range []string{"graphql under floor", "graphql refused", "rest under floor"} {
		t.Run(name, func(t *testing.T) {
			r, l, reads := nothingFreeRig(t, 2)
			r.a.cfg.GitHub.Floor = 2500
			r.loopPass(t, l)
			setBudget := func(low bool) {
				t.Helper()
				err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
					now := r.a.now
					b := &state.Budget{Remaining: 4000, Limit: 5000, Reset: now.Add(time.Hour), At: now,
						GraphQL: &state.GraphQL{Remaining: 4000, Limit: 5000, At: now}}
					switch {
					case !low:
					case name == "graphql under floor":
						b.GraphQL.Remaining = 100
					case name == "graphql refused":
						b.GraphQL.Remaining, b.GraphQL.Refused = 0, "API rate limit exceeded"
					case name == "rest under floor":
						b.Remaining = 100
					}
					st.Budget = b
					return nil, nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			setBudget(true)
			before := *reads
			for range 3 {
				r.loopPass(t, l)
			}
			if *reads != before {
				t.Fatalf("read the board %d times under the floor", *reads-before)
			}
			if fs := r.logged(t, loopFailureVerb); len(fs) != 0 {
				t.Fatalf("a held read recorded as failures: %+v", fs)
			}
			if got := len(r.logged(t, "supervisor.next")); got != 0 {
				t.Fatalf("next starts settled without a board read: %d", got)
			}
			if !strings.Contains(r.out.String(), "SUPERVISOR NEXT HELD") {
				t.Errorf("the hold is not said:\n%s", r.out)
			}
			setBudget(false)
			r.loopPass(t, l)
			if *reads != before+1 || len(r.logged(t, "supervisor.next")) != 2 {
				t.Fatalf("after recovery: %d reads, %d settled", *reads-before, len(r.logged(t, "supervisor.next")))
			}
		})
	}
}

func TestLoopBoardNextRefusalWithoutNothingFreeFails(t *testing.T) {
	for _, body := range []string{
		"beekeeper: configuration refused\n",
		`{"pick":{"ref":"o/r#9","url":"https://github.com/o/r/issues/9"}}` + "\nbeekeeper: no claim\n",
		`{"skipped":[]}` + "\n",
	} {
		r, _ := fillRig(t)
		r.answer = func(args []string) (string, int) {
			if args[0] == loopBoard {
				return body, ExitRefused
			}
			return "", 0
		}
		l, err := r.a.newSupervisorLoop(false)
		if err != nil {
			t.Fatal(err)
		}
		if next, err := l.readNextItem(t.Context()); next != nil || err == nil {
			t.Errorf("refusal %q read as %v, %v", body, next, err)
		}
	}
}
