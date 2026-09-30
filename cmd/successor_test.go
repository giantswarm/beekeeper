package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	agentTwentyEight = "Agent twenty-eight"
	guideSeven       = "Guide run 7"
	hostG7           = "local_G7"
	sessionG7        = "sG7"
	supThirteen      = "Supervisor run 13"
)

func TestRunOf(t *testing.T) {
	for name, want := range map[string]int{
		"Supervisor run 35": 35, " Supervisor run 7 ": 7, guideSeven: 0, agentTwentyEight: 0,
		"Supervisor run": 0, "Supervisor run 3b": 0, "My Supervisor run 3": 0,
	} {
		if got := supervisorRole.runOf(name); got != want {
			t.Errorf("runOf(%q) = %d, want %d", name, got, want)
		}
	}
	if got := guideRole.runName(8); got != "Guide run 8" {
		t.Errorf("runName = %q", got)
	}
}

func TestLastRunReadsTheRecordedNames(t *testing.T) {
	r := state.Role{
		Run:      3,
		Holder:   &state.Supervisor{Party: state.Party{Name: supA.Name}},
		Relieved: []state.Relief{{Party: state.Party{Name: "Supervisor run 12"}, By: state.Party{Name: "Agent nine"}}},
	}
	if got := supervisorRole.lastRun(r); got != 12 {
		t.Errorf("lastRun = %d, want 12", got)
	}
	r.Relay = &state.Relay{To: state.Party{Name: supThirteen}}
	if got := supervisorRole.lastRun(r); got != 13 {
		t.Errorf("lastRun with a relay = %d, want 13", got)
	}
	if got := guideRole.lastRun(r); got != 3 {
		t.Errorf("the guide's lastRun reads the supervisor's names: %d", got)
	}
}

// A start numbers the run: a relayed or titled run keeps its number, any
// other start is the next run, a restart keeps its run.
func TestStartNumbersTheRun(t *testing.T) {
	st := handOverState() // "Supervisor run 11" supervises
	msg, _, err := supervisorRole.start(st, agentC, false, false, relayNow)
	if err != nil || st.SupervisorRun != 12 || !strings.Contains(msg, `"Agent three" supervises now as Supervisor run 12`) {
		t.Fatalf("an untitled start: %q, run %d, %v", msg, st.SupervisorRun, err)
	}
	if _, _, err := supervisorRole.start(st, agentC, true, false, relayNow.Add(time.Minute)); err != nil || st.SupervisorRun != 12 {
		t.Fatalf("a restart: run %d, %v", st.SupervisorRun, err)
	}
	next := state.Party{Session: "sN", HostSession: "local_N", Name: supThirteen}
	if _, _, err := supervisorRole.relay(st, agentC, next, relayNow.Add(2*time.Minute), 15*time.Minute); err != nil || st.SupervisorRun != 13 {
		t.Fatalf("the relay: run %d, %v", st.SupervisorRun, err)
	}
	msg, _, err = supervisorRole.start(st, next, true, false, relayNow.Add(3*time.Minute))
	if err != nil || st.SupervisorRun != 13 || strings.Contains(msg, " as ") {
		t.Fatalf("the successor's start: %q, run %d, %v", msg, st.SupervisorRun, err)
	}
}

func TestSuccessorBriefTakesTheRoleAndEndsTheTurn(t *testing.T) {
	b := guideRole.successorBrief("Guide run 8", guideSeven)
	for _, want := range []string{`You are "Guide run 8"`, "`beekeeper guide start`", "end the turn", "Arm no Monitor", "`beekeeper guide handover --prompt`", `from "Guide run 7"`} {
		if !strings.Contains(b, want) {
			t.Errorf("the brief lacks %q:\n%s", want, b)
		}
	}
}

// A successor that cannot start withdraws its relay: the holder keeps the
// role, and the run number stays taken.
func TestAFailedSuccessorWithdrawsItsRelay(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	cfg.Supervisor.RelayTTL.Duration = 15 * time.Minute
	cfg.Claude.DesktopDir = t.TempDir()
	a := &app{cfg: &cfg, store: store, now: relayNow, out: os.Stdout}
	if err := store.Update(func(st *state.State) ([]state.Event, error) { *st = *handOverState(); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	_, _, err = a.startSuccessor(context.Background(), supervisorRole, supA, supA, filepath.Join(t.TempDir(), "missing"))
	if err == nil || !strings.Contains(err.Error(), `starting "Supervisor run 12"`) || !strings.Contains(err.Error(), "withdrawn") {
		t.Fatalf("startSuccessor: %v", err)
	}
	st, _ := store.Read()
	if st.Relay != nil || !st.Supervisor.Is(supA) || st.SupervisorRun != 12 {
		t.Fatalf("after the failed start: relay %+v, supervisor %+v, run %d", st.Relay, st.Supervisor, st.SupervisorRun)
	}
	evs, _ := store.Events(0, func(e state.Event) bool { return strings.HasPrefix(e.Verb, "supervisor.relay") })
	if len(evs) != 2 || evs[1].Verb != "supervisor.relay-cancel" {
		t.Fatalf("relay events: %+v", evs)
	}
}

// A gone guide gets a successor from the standby watch once: its relay
// keeps the next polls from starting another.
func TestStandbyStartsAGoneGuidesSuccessor(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), true)
	var started atomic.Int32
	w.stand = standbyWatch{
		send: func(context.Context, string, string) error { return nil },
		succeed: func(_ context.Context, rl role, from state.Party) (state.Party, error) {
			started.Add(1)
			to := state.Party{Session: "sG8", Name: "Guide run 8"}
			return to, w.store.Update(func(st *state.State) ([]state.Event, error) {
				_, evs, err := rl.relay(st, from, to, w.now, time.Hour)
				return evs, err
			})
		},
	}
	guide := state.Party{Session: sessionG7, HostSession: hostG7, Name: guideSeven}
	err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide = &state.Role{Holder: &state.Supervisor{Party: guide, Since: relayNow.Add(-time.Hour)}, Run: 7,
			CLI: &state.CLI{Supervisor: guide, Since: relayNow.Add(-time.Hour), PID: 77, Gone: relayNow.Add(-time.Minute)}}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Duration{0, 10 * time.Second, time.Minute} {
		w.now = relayNow.Add(at)
		w.pending(context.Background(), nil)
		eventually(2*time.Second, func() bool { return !w.stand.starting.Load() })
	}
	if n := started.Load(); n != 1 {
		t.Fatalf("%d guide successors, want 1:\n%s", n, out)
	}
	if l := out.String(); !strings.Contains(l, `GUIDE GONE: "Guide run 7"`) || !strings.Contains(l, `GUIDE SUCCESSOR: started "Guide run 8"`) {
		t.Errorf("watch lines:\n%s", l)
	}
}

// A holder between beekeeper's start and its desktop CLI is not gone.
func TestAFirstTurnIsNotGone(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), true)
	w.stand = standbyWatch{
		turning: func(_ context.Context, id string) bool { return id == four.Session },
		succeed: func(context.Context, role, state.Party) (state.Party, error) {
			t.Error("a successor for a session in its first turn")
			return state.Party{}, nil
		},
	}
	err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Supervisor = &state.Supervisor{Party: four, Since: relayNow.Add(-time.Hour)}
		st.SupervisorCLI = &state.CLI{Supervisor: four, Since: relayNow.Add(-time.Hour), PID: 44, Gone: relayNow.Add(-time.Hour)}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	w.now = relayNow
	w.pending(context.Background(), nil)
	if strings.Contains(out.String(), "GONE") {
		t.Errorf("a first turn said gone:\n%s", out)
	}
}

// A new CLI of the guide is told to take its role up again.
func TestStandbyResumesARestartedGuide(t *testing.T) {
	w, _, _ := notifyingWatch(t, t.TempDir(), true)
	sent := make(chan string, 2)
	w.stand = standbyWatch{send: func(_ context.Context, to, msg string) error { sent <- to + ": " + msg; return nil }}
	guide := state.Party{Session: sessionG7, HostSession: hostG7, Name: guideSeven}
	err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide = &state.Role{Holder: &state.Supervisor{Party: guide, Since: relayNow.Add(-time.Hour)},
			CLI: &state.CLI{Supervisor: guide, Since: relayNow.Add(-time.Hour), PID: 77}}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	w.now = relayNow
	w.pending(context.Background(), []*claude.Session{{ID: sessionG7, HostID: hostG7, Name: guideSeven, PID: 78}})
	select {
	case m := <-sent:
		if !strings.HasPrefix(m, "Guide run 7: beekeeper: your CLI restarted (CLI 77 back as 78") || !strings.Contains(m, "`beekeeper guide handover --prompt`") {
			t.Errorf("resume message: %q", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no resume message")
	}
}

// A roster entry known by its CLI id alone, whose session the desktop
// imported, gets the reopen after its wake.
func TestAWakeReopensAnImportedSessionWithoutADesktopID(t *testing.T) {
	cfg := &config.Config{}
	cfg.Claude.DesktopDir = t.TempDir()
	cfg.Claude.ProjectsDir = t.TempDir()
	dir := filepath.Join(cfg.Claude.DesktopDir, "u", "o")
	for _, f := range []struct{ path, body string }{
		{filepath.Join(dir, "local_s9.json"), `{"sessionId":"local_s9","cliSessionId":"s9","cwd":"/tmp","title":"Supervisor run 35"}`},
		{filepath.Join(cfg.Claude.ProjectsDir, "p", "s9.jsonl"), "{}\n"},
	} {
		if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.path, []byte(f.body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ag := state.Agent{Party: state.Party{Session: "s9", Name: agentTwentyEight}}
	st := &state.State{Agents: []state.Agent{ag}}
	w, err := resolveWake(cfg, st, ag)
	if err != nil || w.host != "local_s9" || w.name != "Supervisor run 35" {
		t.Fatalf("resolveWake = %+v, %v", w, err)
	}
	if name, ok := reopens(st, "local_s9"); !ok || name != agentTwentyEight {
		t.Fatalf("reopens = %q, %v", name, ok)
	}
}

// A relay whose successor beekeeper never started (the watch that opened it
// stopped first) stands for none once startGrace has passed.
func TestADanglingRelayStandsForNoSuccessor(t *testing.T) {
	to := state.Party{Session: "sN", Name: supThirteen}
	r := state.Role{Relay: &state.Relay{From: supA, To: to, At: relayNow, Expires: relayNow.Add(15 * time.Minute)}}
	st := &state.State{}
	if !relayPending(st, r, relayNow.Add(time.Minute)) {
		t.Error("a relay a minute old stands for none")
	}
	if relayPending(st, r, relayNow.Add(startGrace)) {
		t.Error("a relay without a start stands past startGrace")
	}
	st.Starts = []state.Start{{Party: to}}
	if !relayPending(st, r, relayNow.Add(startGrace)) {
		t.Error("a relay to a started successor stands for none")
	}
	if relayPending(st, r, relayNow.Add(15*time.Minute)) {
		t.Error("an expired relay stands")
	}
}
