package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

const fillCapacityCommand = "capacity"

// Only external commands are replaced; capacity changes after each start.
func fillRig(t *testing.T) (*loopRig, *int) {
	t.Helper()
	r := newLoopRig(t)
	brief := filepath.Join(t.TempDir(), "worker.md")
	if err := os.WriteFile(brief, []byte("Work {{.URL}}."), 0o600); err != nil {
		t.Fatal(err)
	}
	r.a.cfg.Supervisor.WorkerBrief = brief
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
						return `{"floor":3,"room":0}`, 0
					case "at floor":
						return `{"floor":0,"room":5}`, 0
					case "bad capacity":
						return "broken JSON", 0
					}
				}
				if args[0] == loopBoard {
					switch name {
					case "empty board":
						return `{"pick":null}`, 0
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
			return `{"floor":3,"room":0}`, 0
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
