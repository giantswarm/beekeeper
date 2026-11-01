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
)

const (
	fillCapacityCommand = "capacity"
	fillEmptyBoardJSON  = `{"pick":null}`
	fillNoRoomJSON      = `{"floor":3,"room":0}`
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
	for _, change := range []string{"busy count changes", "available room changes", "roster membership changes", "capacity cycles through the floor"} {
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
			case "roster membership changes":
				r.started(t, "another worker", "another-session")
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
			out, err := json.Marshal(capacityView{Floor: 3, Ceiling: 5, Room: 0, agentCount: agentCount{Busy: make([]string, *busy)}})
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
// placed start refused after it reserved its execution: the loop recovers it, and the claim stays open until
// the taken line, so an interruption in between still reconciles it.
func TestLoopFillRecoversAReservedPlacedStartBeforeClosingItsClaim(t *testing.T) {
	r, busy := hostFillRig(t)
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
