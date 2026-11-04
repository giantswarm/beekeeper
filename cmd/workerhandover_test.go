package cmd

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/state"
)

const registerVerb = "register"

func TestKilledRunDoesNotFinishWorker(t *testing.T) {
	a, _ := stubApp(t)
	p := state.Party{Session: "s3", Name: countName}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: p, Task: countTask, AssignedAt: a.now}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	record := a.runRecorder()
	record(guard.VerbStart, "test step")
	record(guard.VerbEnd, "exit 137: test step")
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	ag := st.Agents[0]
	if ag.Task != countTask || ag.Done || !ag.IdleSince.IsZero() {
		t.Fatalf("killed step finished worker: %+v", ag)
	}
	sessions := []*claude.Session{{ID: p.Session, Name: p.Name}}
	if got := handoversDue(st, sessions, config.Agents{RelayAt: 20000}, a.now, func(state.Party) bool { return false }, func(state.Agent, *claude.Session) int64 { return 100 }, func(int) bool { return false }); len(got) != 0 {
		t.Fatalf("killed step caused handover: %+v", got)
	}
	if got := planChores(st, sessions, unarchived, func(state.Party) bool { return false }, 24*time.Hour, a.now, nil); len(got) != 0 {
		t.Fatalf("killed step caused doctor chores: %+v", got)
	}
}

func TestHandoverStopsFreshlyDiscoveredCLI(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: reviewedStoppedCase, true: "stop fails"}[fail], func(t *testing.T) {
			a, out := stubApp(t)
			titles := map[int]string{3: countName}
			runLive(t, a, titles)
			saved := endHandoverSession
			t.Cleanup(func() { endHandoverSession = saved })
			called := false
			endHandoverSession = func(_ context.Context, s *claude.Session) (int, error) {
				called = true
				if s.ID != "s3" {
					t.Fatalf("stopping wrong CLI: %+v", s)
				}
				if fail {
					return 0, errors.New("cannot stop")
				}
				delete(titles, 3)
				return 1, nil
			}
			err := a.stopHandoverPredecessor(t.Context(), state.Party{Session: "s3", Name: countName})
			if !called {
				t.Fatal("freshly discovered CLI was not stopped")
			}
			if fail {
				if Code(err) != ExitRefused || !strings.Contains(err.Error(), "no successor started") {
					t.Fatalf("stop failure: %v", err)
				}
				return
			}
			if err != nil || !strings.Contains(out.String(), "before starting the successor") {
				t.Fatalf("stop: %v, %s", err, out)
			}
			sessions, _, err := a.sessions()
			if err != nil {
				t.Fatal(err)
			}
			if err := titleTaken(sessions, countName, state.Party{}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestArchiveNeverWarmsHandedOverCLI(t *testing.T) {
	a, _ := stubApp(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = []state.Start{{Party: handedFrom}, {Party: handedTo}}
		st.Agents = []state.Agent{{Party: handedTo, Task: countTask}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err := a.warmSteward(t.Context(), handedFrom.HostSession)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "will not be resumed") {
		t.Fatalf("archive revived predecessor: %v", err)
	}
}

// The handover mark hides the outgoing session from superseded, so the
// handover's own archive step needs its own refusal.
func TestArchiveNeverWarmsCLIDuringItsHandover(t *testing.T) {
	a, _ := stubApp(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = []state.Start{{Party: handedFrom}, {Party: handedTo}}
		st.Agents = []state.Agent{{Party: handedTo, Task: countTask}}
		st.Handovers = []state.Handover{{Party: handedFrom, PID: os.Getpid()}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err := a.warmSteward(t.Context(), handedFrom.HostSession)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "will not be resumed") {
		t.Fatalf("archive revived the session under handover: %v", err)
	}
}

func TestHandoverRefusesLiveGateWait(t *testing.T) {
	for _, phase := range []string{state.Waiting, state.Running, state.Settling} {
		st := &state.State{Merges: []state.Merge{{By: handedFrom, PID: os.Getpid(), Phase: phase}}}
		if err := refuseHandoverGate(st, handedFrom); Code(err) != ExitRefused {
			t.Fatalf("phase %s: %v", phase, err)
		}
	}
}

func TestRegisterRefusesUnregisteredLiveCLIName(t *testing.T) {
	a, _ := stubApp(t)
	a.as = ""
	for _, key := range []string{"BEEKEEPER_CODEX_AGENT", "BEEKEEPER_OMP_AGENT", litBeekeeperAgyAgent, "CLAUDE_CODE_HOST_SESSION_ID"} {
		t.Setenv(key, "")
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s4")
	t.Setenv("CLAUDE_CODE_SESSION_NAME", "new worker")
	runLive(t, a, map[int]string{3: countName})
	c, _, err := a.agentsCmd().Find([]string{registerVerb})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Flags().Set("name", countName); err != nil {
		t.Fatal(err)
	}
	err = c.RunE(c, nil)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "live session s3") {
		t.Fatalf("register duplicate: %v", err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Agents) != 0 {
		t.Fatalf("refused registration changed roster: %+v", st.Agents)
	}
}

// A turn whose unit could not be stopped must not get a successor.
type stuckHandoverTurn struct{ platform.Launcher }

func (stuckHandoverTurn) Running(context.Context, bool, ...string) []string {
	return []string{"test-turn.service"}
}
func (stuckHandoverTurn) Stop(context.Context, string) error { return errors.New("turn still runs") }

func TestHandoverRefusesTurnThatCannotStop(t *testing.T) {
	a, _ := stubApp(t)
	runLive(t, a, map[int]string{})
	plat.Launcher = stuckHandoverTurn{plat.Launcher}
	err := a.stopHandoverPredecessor(t.Context(), state.Party{Session: "session-12345678", Name: countName})
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "still runs a turn") {
		t.Fatalf("unstopped turn: %v", err)
	}
}
