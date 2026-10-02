package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// rotationLogin is the session of a worker whose next turn needs the
// desktop's browser, rotationName its name and rotationTask its task.
const (
	rotationLogin = "26c5814d-1c8e-42dc-9103-51893d720905"
	rotationName  = "Rotation login"
	rotationTask  = "log in"
)

// rotationParty is the worker's party.
var rotationParty = state.Party{Session: rotationLogin, HostSession: "local_" + rotationLogin, Name: rotationName}

// lockMachine's process table holds a screen locker, or none.
type lockMachine struct {
	platform.Machine
	locker string
}

func (m lockMachine) Processes() (*proc.Table, error) {
	t := &proc.Table{ByPID: map[int]*proc.Process{1: {PID: 1, Comm: "systemd"}}}
	if m.locker != "" {
		t.ByPID[2] = &proc.Process{PID: 2, Comm: m.locker}
	}
	return t, nil
}

// A locked screen holds no link: the compositor still names the desktop's
// window, but nobody reads or types there, and keystrokes go to the locker.
func TestALockedScreenHoldsNoLink(t *testing.T) {
	_, _ = stubApp(t)
	saved := desktopWindowActive
	t.Cleanup(func() { desktopWindowActive = saved })
	desktopWindowActive = func(context.Context) (bool, error) { return true, nil }
	d := desk{quiet: time.Hour, last: time.Now, locked: screenLocked}
	plat.Machine = lockMachine{Machine: plat.Machine}
	if err := d.await(t.Context(), 2*awayPoll, nil); !errors.Is(err, errDesktopInUse) {
		t.Fatalf("unlocked, the desktop's focused window: %v, want errDesktopInUse", err)
	}
	plat.Machine = lockMachine{Machine: plat.Machine, locker: "hyprlock"}
	if err := d.await(t.Context(), 2*awayPoll, nil); err != nil {
		t.Errorf("a link under a locked screen waited: %v", err)
	}
}

// An agent that asked for a desktop turn is shown past the window's focus;
// the person's typing still holds the link, within desktopTurnWait.
func TestADesktopTurnGoesPastTheFocus(t *testing.T) {
	saved := desktopWindowActive
	t.Cleanup(func() { desktopWindowActive = saved })
	desktopWindowActive = func(context.Context) (bool, error) { return true, nil }
	asked := func() bool { return true }
	if err := (desk{quiet: -1}).await(t.Context(), 2*awayPoll, nil); !errors.Is(err, errDesktopInUse) {
		t.Fatalf("no desktop turn asked: %v, want the focus to hold the link", err)
	}
	if err := (desk{quiet: -1, urgent: asked}).await(t.Context(), 2*awayPoll, nil); err != nil {
		t.Fatalf("a desktop turn waited on the focus: %v", err)
	}
	typing := desk{quiet: time.Hour, last: time.Now, urgent: asked}
	if err := typing.await(t.Context(), 2*awayPoll, nil); !errors.Is(err, errTyping) {
		t.Errorf("a desktop turn under the person's typing: %v, want errTyping", err)
	}
}

// rosterAgent puts ag on the roster as one of beekeeper's starts.
func rosterAgent(t *testing.T, a *app, ag state.Agent) {
	t.Helper()
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = append(st.Starts, state.Start{Party: ag.Party, Mode: state.ModeBypass, At: time.Now()})
		st.Agents = append(st.Agents, ag)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// warmMachine's process table holds the desktop's CLI of session id once
// warm is set.
type warmMachine struct {
	platform.Machine
	id   string
	warm *atomic.Bool
}

func (m warmMachine) Processes() (*proc.Table, error) {
	t := &proc.Table{ByPID: map[int]*proc.Process{}}
	if m.warm.Load() {
		t.ByPID[7] = &proc.Process{PID: 7, Comm: claudeComm, Args: []string{claudeComm, resumeFlag, m.id}}
	}
	return t, nil
}

// warmingOpener is a desktop that warms the CLI of every session it shows.
type warmingOpener struct {
	timedOpener
	warm *atomic.Bool
}

func (o *warmingOpener) Open(ctx context.Context, url string, running bool) error {
	o.warm.Store(true)
	return o.timedOpener.Open(ctx, url, running)
}

// The reopen of an agent that asked for a desktop turn shows it while the
// person works in the desktop's window, and the ask ends once the desktop
// runs its CLI.
func TestReopenShowsADesktopTurnAtOnce(t *testing.T) {
	a, out := stubApp(t)
	a.cfg.Desktop.TypingQuiet.Duration = -1
	a.cfg.Claude.DesktopLog = filepath.Join(t.TempDir(), "main.log")
	p := rotationParty
	rosterAgent(t, a, state.Agent{Party: p, Task: rotationTask, DesktopTurn: time.Now()})
	saved := desktopWindowActive
	t.Cleanup(func() { desktopWindowActive = saved })
	desktopWindowActive = func(context.Context) (bool, error) { return true, nil }
	warm := &atomic.Bool{}
	plat.Machine = warmMachine{Machine: plat.Machine, id: rotationLogin, warm: warm}
	o := &warmingOpener{warm: warm}
	plat.Opener = o
	plat.Launcher = &unitLauncher{}
	// The retitle after the show waits for a steward none runs here.
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	start := time.Now()
	if err := a.reopenSession(ctx, rotationLogin); err != nil {
		t.Fatal(err)
	}
	if len(o.opened) == 0 || o.opened[0] != resumeURL(rotationLogin) {
		t.Fatalf("opened %v, want the import\n%s", o.opened, out)
	}
	if waited := o.at.Sub(start); waited > time.Second {
		t.Errorf("the import waited %s", waited)
	}
	if !strings.Contains(out.String(), "reopen: the desktop runs its CLI (PID 7)") {
		t.Errorf("output:\n%s", out)
	}
	st, _ := a.store.Read()
	if ag := st.Agents[agentOfSession(st, rotationLogin)]; !ag.DesktopTurn.IsZero() || ag.Import != nil {
		t.Errorf("after the show: desktop turn %v, import %+v", ag.DesktopTurn, ag.Import)
	}
}

// timedOpener records the links it opens and when it opened the first.
type timedOpener struct {
	recordingOpener
	at time.Time
}

func (o *timedOpener) Open(ctx context.Context, url string, running bool) error {
	if o.at.IsZero() {
		o.at = time.Now()
	}
	return o.recordingOpener.Open(ctx, url, running)
}

// A desktop at its cap of CLIs warms none for a show: the reopen says so
// instead of a warmed CLI, and the ask for a desktop turn stays.
func TestReopenSaysTheDesktopIsAtItsCap(t *testing.T) {
	a, out := stubApp(t)
	a.cfg.Desktop.TypingQuiet.Duration = -1
	a.cfg.Claude.DesktopLog = filepath.Join(t.TempDir(), "main.log")
	p := rotationParty
	rosterAgent(t, a, state.Agent{Party: p, Task: rotationTask, DesktopTurn: time.Now()})
	plat.Machine = tableMachine{plat.Machine}
	plat.Launcher = &unitLauncher{}
	plat.Opener = capOpener{log: a.cfg.Claude.DesktopLog}
	ctx, cancel := context.WithTimeout(t.Context(), twinWait+5*time.Second)
	defer cancel()
	if err := a.reopenSession(ctx, rotationLogin); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "it runs its cap of 28 CLIs and starts none for a show") {
		t.Errorf("output:\n%s", out)
	}
	st, _ := a.store.Read()
	if ag := st.Agents[agentOfSession(st, rotationLogin)]; ag.DesktopTurn.IsZero() {
		t.Error("the ask for a desktop turn ended without a desktop CLI")
	}
}

// capOpener is a desktop at its cap of CLIs: a show logs the declined warm
// spawn.
type capOpener struct{ log string }

func (capOpener) Running(*proc.Table) time.Time { return time.Now() }

func (o capOpener) Open(context.Context, string, bool) error {
	at := time.Now().Format(time.DateTime)
	line := at + " [info] [CliGovernor] at cap=28; would evict local_x (idle 83238s) for warm spawn\n" +
		at + " [info] [CliGovernor] at cap; yielding warm spawn\n"
	return os.WriteFile(o.log, []byte(line), 0o600)
}

// A reopen held by the window's focus records what it waits for and until
// when while it waits, and clears it when it ends.
func TestReopenRecordsItsWait(t *testing.T) {
	a, _ := stubApp(t)
	a.cfg.Desktop.TypingQuiet.Duration = -1
	p := rotationParty
	rosterAgent(t, a, state.Agent{Party: p, Task: rotationTask})
	saved := desktopWindowActive
	t.Cleanup(func() { desktopWindowActive = saved })
	desktopWindowActive = func(context.Context) (bool, error) { return true, nil }
	plat.Machine = tableMachine{plat.Machine}
	plat.Opener = &recordingOpener{}
	plat.Launcher = &unitLauncher{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.reopenSession(ctx, rotationLogin) }()
	var seen *state.ImportWait
	eventually(5*time.Second, func() bool {
		st, _ := a.store.Read()
		seen = st.Agents[agentOfSession(st, rotationLogin)].Import
		return seen != nil
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if seen == nil || seen.On != "the desktop's window to lose the focus" || seen.Until.Sub(seen.Since) != reopenAwayWait {
		t.Fatalf("recorded wait %+v", seen)
	}
	st, _ := a.store.Read()
	if w := st.Agents[agentOfSession(st, rotationLogin)].Import; w != nil {
		t.Errorf("the ended reopen left its wait: %+v", w)
	}
}

// waitingAgent is a worker whose reopen waits on the window's focus.
func waitingAgent(now time.Time) state.Agent {
	return state.Agent{
		Party: rotationParty,
		Task:  rotationTask,
		Import: &state.ImportWait{On: "the desktop's window to lose the focus",
			Since: now.Add(-time.Minute), Until: now.Add(24 * time.Minute)},
	}
}

// A grant to an agent whose CLI does not run is recorded, saying so and
// that its import is pending.
func TestGrantToAnAgentWithoutACLI(t *testing.T) {
	a, out := stubApp(t)
	plat.Machine = tableMachine{plat.Machine}
	rosterAgent(t, a, waitingAgent(a.now))
	c := a.leaseGrantCmd()
	c.SetArgs([]string{"browser", rotationName})
	if err := c.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`granted browser to "Rotation login"`, `no CLI of "Rotation login" runs`, "its import is pending", "lose the focus", "it claims once a CLI of it runs"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("grant lacks %q:\n%s", want, out)
		}
	}
	st, _ := a.store.Read()
	if len(st.Grants) != 1 || st.Grants[0].To.Session != rotationLogin {
		t.Errorf("grants %+v", st.Grants)
	}
}

// A message by name to an agent whose CLI does not run is refused with
// whether its import is pending; one with no pending import names the
// commands that bring it back.
func TestMessageByNameToAnAgentWithoutACLI(t *testing.T) {
	a, _ := stubApp(t)
	plat.Machine = tableMachine{plat.Machine}
	rosterAgent(t, a, waitingAgent(time.Now()))
	if r := a.absentPeer("rotation login"); !strings.Contains(r, `no CLI of "Rotation login" runs; its import is pending`) {
		t.Errorf("pending import: %q", r)
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Import = nil
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if r := a.absentPeer("Rotation login"); !strings.Contains(r, "no import is pending") || !strings.Contains(r, `beekeeper agents desktop "Rotation login"`) {
		t.Errorf("no pending import: %q", r)
	}
	if r := a.absentPeer("Nobody"); r != "" {
		t.Errorf("a name no agent carries: %q", r)
	}
}

// The watch says once per wait which import waits, on what and until when.
func TestWatchSaysAnImportWaits(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	w.now = relayNow
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = append(st.Agents, waitingAgent(relayNow))
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		st, _ := w.store.Read()
		w.importWaits(st)
	}
	if n := strings.Count(out.String(), "IMPORT WAITS"); n != 1 {
		t.Fatalf("%d IMPORT WAITS lines, want 1:\n%s", n, out)
	}
	if l := out.String(); !strings.Contains(l, `"Rotation login" (its import is pending: the reopen waits for the desktop's window to lose the focus`) ||
		!strings.Contains(l, "`beekeeper agents desktop \"Rotation login\"` imports it now") {
		t.Errorf("watch line:\n%s", l)
	}
}
