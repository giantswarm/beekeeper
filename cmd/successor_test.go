package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/proc"
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
	t.Cleanup(w.stand.inflight.Wait)
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

// A successor starts in the configured folder, else where its predecessor's
// desktop session started, never in the desktop's worktree of it.
func TestSuccessorDir(t *testing.T) {
	wt := &claude.Record{Cwd: "/repo/.claude/worktrees/w1", OriginCwd: "/repo"}
	for _, c := range []struct {
		name  string
		cfg   config.Role
		rec   *claude.Record
		start string
		want  string
	}{
		{"configured", config.Role{Dir: "/desk"}, wt, "/started", "/desk"},
		{"a desktop worktree: its origin", config.Role{}, wt, "/started", "/repo"},
		{"no worktree", config.Role{}, &claude.Record{Cwd: "/repo"}, "/started", "/repo"},
		{"no desktop record: beekeeper's start", config.Role{}, nil, "/started", "/started"},
		{"nothing recorded: the caller's", config.Role{}, nil, "", "."},
	} {
		if got := successorDir(c.cfg, c.rec, c.start); got != c.want {
			t.Errorf("%s: successorDir = %q, want %q", c.name, got, c.want)
		}
	}
}

// supervisedBy makes p, one of beekeeper's starts, the supervisor since
// since, its CLI gone two minutes before w.now.
func supervisedBy(t *testing.T, w *watcher, p state.Party, since time.Time) {
	t.Helper()
	err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Relay = nil
		st.Supervisor = &state.Supervisor{Party: p, Since: since}
		st.SupervisorCLI = &state.CLI{Supervisor: p, Since: since, PID: 50, Gone: w.now.Add(-2 * time.Minute)}
		st.Starts = append(st.Starts, state.Start{Party: p, Mode: state.ModeBypass, At: since})
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A successor whose first turn ended without a desktop CLI is resumed
// headless, not replaced; one that does not come up is one note, and the
// next successors start a bounded number of times, further and further
// apart.
func TestStandbyResumesAndBoundsSuccessorsThatDoNotComeUp(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), true)
	w.cfg.Guide.Person = "Pat"
	var revived []string
	var succeeded atomic.Int32
	w.stand = standbyWatch{
		send: func(context.Context, string, string) error { t.Error("a resume message by name"); return nil },
		revive: func(_ context.Context, rl role, holder state.Party, msg string) error {
			if rl.name != supervisorRole.name || !strings.Contains(msg, "`beekeeper handover --prompt`") {
				t.Errorf("revive %s %q: %q", rl.name, holder.Name, msg)
			}
			revived = append(revived, holder.Name)
			return nil
		},
		succeed: func(context.Context, role, state.Party) (state.Party, error) {
			succeeded.Add(1)
			return state.Party{Name: "next"}, nil
		},
	}
	t.Cleanup(w.stand.inflight.Wait)
	poll := func(at time.Time) {
		w.now = at
		w.pending(context.Background(), nil)
		eventually(2*time.Second, func() bool { return !w.stand.starting.Load() })
	}
	at := relayNow
	for i, wait := range []time.Duration{successorBackoff, 2 * successorBackoff, time.Hour} {
		run := state.Party{Session: fmt.Sprintf("s%d", 12+i), Name: supervisorRole.runName(12 + i)}
		w.now = at
		supervisedBy(t, w, run, at.Add(-time.Minute))
		poll(at)
		if len(revived) != i+1 || revived[i] != run.Name || succeeded.Load() != int32(i) {
			t.Fatalf("%s: revived %v, %d successors", run.Name, revived, succeeded.Load())
		}
		poll(at.Add(time.Minute)) // its headless resume ended too
		poll(at.Add(wait - time.Second))
		if n := succeeded.Load(); n != int32(i) {
			t.Fatalf("%s: %d successors before the backoff", run.Name, n)
		}
		at = at.Add(time.Minute + wait)
		poll(at)
	}
	if n := succeeded.Load(); n != 2 {
		t.Errorf("%d successors, want 2 (the third failed one ends the chain)", n)
	}
	st, _ := w.store.Read()
	if len(st.Notes) != 1 || !strings.Contains(st.Notes[0].Text, `"Supervisor run 12"`) || st.Notes[0].For != "Pat" {
		t.Errorf("notes: %+v", st.Notes)
	}
	if l := out.String(); !strings.Contains(l, "3 successors did not come up, none further starts (note #1)") || strings.Count(l, "SUCCESSOR DOWN") != 3 {
		t.Errorf("watch lines:\n%s", l)
	}
}

// A holder that is no start of beekeeper's gets its successor at once, and
// a holder up again ends a chain of failed successors.
func TestStandbySucceedsAGoneDesktopSupervisorAtOnce(t *testing.T) {
	w, _, _ := notifyingWatch(t, t.TempDir(), true)
	var succeeded atomic.Int32
	w.stand = standbyWatch{
		revive: func(context.Context, role, state.Party, string) error { t.Error("revived a desktop session"); return nil },
		succeed: func(context.Context, role, state.Party) (state.Party, error) {
			succeeded.Add(1)
			return state.Party{}, nil
		},
		chains: map[string]*successorChain{supervisorRole.name: {failures: 1, next: relayNow.Add(time.Hour)}},
	}
	t.Cleanup(w.stand.inflight.Wait)
	err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Supervisor = &state.Supervisor{Party: four, Since: relayNow.Add(-time.Hour)}
		st.SupervisorCLI = &state.CLI{Supervisor: four, Since: relayNow.Add(-time.Hour), PID: 44}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	w.now = relayNow
	w.pending(context.Background(), []*claude.Session{{ID: four.Session, HostID: hostFour, Name: agentFour, PID: 44}})
	if w.stand.chains[supervisorRole.name] != nil {
		t.Fatalf("a supervisor up again kept the chain: %+v", w.stand.chains[supervisorRole.name])
	}
	_ = w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.SupervisorCLI.Gone = relayNow
		return nil, nil
	})
	w.now = relayNow.Add(2 * time.Minute)
	w.pending(context.Background(), nil)
	eventually(2*time.Second, func() bool { return succeeded.Load() == 1 })
	if n := succeeded.Load(); n != 1 {
		t.Errorf("%d successors, want 1", n)
	}
}

// The resume goes to the holder's desktop CLI, never to the headless first
// turn that took the role.
func TestStandbyResumesTheDesktopCLINotTheFirstTurn(t *testing.T) {
	w, _, _ := notifyingWatch(t, t.TempDir(), true)
	sent := make(chan string, 2)
	w.stand = standbyWatch{send: func(_ context.Context, to, msg string) error { sent <- to; return nil }}
	guide := state.Party{Session: sessionG7, HostSession: hostG7, Name: guideSeven}
	err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide = &state.Role{Holder: &state.Supervisor{Party: guide, Since: relayNow.Add(-time.Minute)},
			CLI: &state.CLI{Supervisor: guide, Since: relayNow.Add(-time.Minute), PID: 77}}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	w.table = &proc.Table{ByPID: map[int]*proc.Process{
		78: {PID: 78, Comm: claudeComm, Args: []string{claudeComm, "-p", sessionIDFlag, sessionG7, "--", "brief"}},
		79: {PID: 79, Comm: claudeComm, Args: []string{claudeComm, "--output-format", "stream-json", resumeFlag, sessionG7}},
	}}
	w.now = relayNow
	w.pending(context.Background(), []*claude.Session{{ID: sessionG7, HostID: hostG7, Name: guideSeven, PID: 78}})
	select {
	case to := <-sent:
		t.Fatalf("resumed the first turn: %s", to)
	case <-time.After(200 * time.Millisecond):
	}
	w.pending(context.Background(), []*claude.Session{{ID: sessionG7, HostID: hostG7, Name: guideSeven, PID: 79}})
	select {
	case to := <-sent:
		if to != guideSeven {
			t.Errorf("resumed %q", to)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the desktop CLI got no resume")
	}
}
