package cmd

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// reopenLauncher runs no unit: it lists the running ones it was given by
// pattern and records the units it was asked to start.
type reopenLauncher struct {
	platform.Launcher
	running []string
	started []platform.Unit
}

func (l *reopenLauncher) Running(_ context.Context, _ bool, patterns ...string) []string {
	var out []string
	for _, u := range l.running {
		for _, p := range patterns {
			if ok, _ := path.Match(p, u); ok {
				out = append(out, u)
			}
		}
	}
	return out
}

func (l *reopenLauncher) Start(u platform.Unit) error {
	l.started = append(l.started, u)
	return nil
}

// The rowless workers of beekeeper's starts: busy on a task, no row in the
// desktop, no CLI running, past the start's grace, and no reopen of theirs
// running; the task they are on; and the units a start of one (stopng01)
// and a doctor's reopen of another (reopen01) run.
const (
	rowlessOne   = "rowless1"
	rowlessTwo   = "rowless2"
	rowlessTask  = "a task"
	stoppingUnit = "beekeeper-agent-stopng01.service"
	reopenGoing  = "beekeeper-reopen-reopen01-1a2b3c4d"
	notRunning   = "not running"
	holderID     = "holder01"
)

// rowlessState is a roster of beekeeper's starts: two rowless workers and
// one of every kind the doctor leaves alone.
func rowlessState(now time.Time) *state.State {
	st := &state.State{}
	worker := func(id string, started time.Duration, task string) {
		p := state.Party{Session: id, Name: "worker " + id}
		st.Starts = append(st.Starts, state.Start{Party: p, Dir: "/work/" + id, At: now.Add(-started)})
		st.Agents = append(st.Agents, state.Agent{Party: p, Task: task})
	}
	worker(rowlessOne, time.Hour, rowlessTask)
	worker(rowlessTwo, 2*time.Hour, "another task")
	for _, id := range []string{"fresh001", "rowed001", "turning1", "twin0001", "done0001", "idle0001", "reopen01", "stopng01", holderID} {
		worker(id, time.Hour, rowlessTask)
	}
	st.Starts[2].At = now.Add(-time.Minute)
	st.Agents[6].Done = true
	st.Agents[7].Task = ""
	st.Supervisor = &state.Supervisor{Party: state.Party{Session: holderID}, Since: now.Add(-time.Hour)}
	// A registered session of the person's own, no start of beekeeper's.
	st.Agents = append(st.Agents, state.Agent{Party: state.Party{Session: "own00001", Name: "the person's own"}, Task: rowlessTask})
	return st
}

// rowlessTable is a process table with the desktop's CLI of twin0001, one
// more desktop CLI, and the headless first turn of turning1.
func rowlessTable() *proc.Table {
	twin := append([]string{claudeComm, resumeFlag, "twin0001"}, desktopArgs[1:]...)
	return &proc.Table{ByPID: map[int]*proc.Process{
		1: {PID: 1, Comm: claudeComm, Args: twin},
		2: {PID: 2, Comm: claudeComm, Args: desktopArgs},
		3: {PID: 3, Comm: claudeComm, Args: []string{claudeComm, "-p", sessionIDFlag, "turning1"}},
	}}
}

func rowlessRecord(host string) (*claude.Record, bool) {
	if host == "local_rowed001" {
		return &claude.Record{SessionID: host}, true
	}
	return nil, false
}

// capLog writes a desktop log whose governor named a cap of n CLIs and
// returns its path.
func capLog(t *testing.T, n int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "main.log")
	line := "2026-10-06 20:00:00 [info] [CliGovernor] at cap=" + strconv.Itoa(n) + "; would evict local_x\n"
	if err := os.WriteFile(p, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The doctor finds the workers the desktop never imported whose CLI does
// not run: not a fresh start, one with a row, one in a headless turn, one
// whose desktop CLI runs, a finished one, an idle one, a role holder, one
// whose reopen or start unit still runs, nor a session a person started.
func TestRowlessWorkers(t *testing.T) {
	a, _ := stubApp(t)
	now := a.now
	l := &reopenLauncher{running: []string{reopenGoing, stoppingUnit}}
	useLauncher(t, l)
	st := rowlessState(now)
	var got []string
	for _, w := range a.rowlessWorkers(t.Context(), st, rowlessTable(), rowlessRecord) {
		got = append(got, w.agent.Session)
	}
	if want := rowlessOne + " " + rowlessTwo; strings.Join(got, " ") != want {
		t.Errorf("rowless workers %v, want %s", got, want)
	}
	if w := a.rowlessWorkers(t.Context(), st, nil, rowlessRecord); w != nil {
		t.Errorf("with no process table: %v", w)
	}
}

// Under its cap the doctor reopens a rowless worker in a transient unit
// that runs `agents reopen` of its session, logs it, and leaves the next
// one once the reopen fills the cap; a dry run says both; a running reopen
// unit of the worker holds the next pass off, and a desktop that does not
// run gets no reopen.
func TestDoctorReopensRowlessWorkersUnderTheCap(t *testing.T) {
	a, _ := stubApp(t)
	a.cfg.Claude.DesktopLog = capLog(t, 4)
	// reopen01's reopen is under way: the CLI it warms counts against the
	// cap beside the two desktop CLIs the table shows.
	l := &reopenLauncher{running: []string{reopenGoing, stoppingUnit}}
	useLauncher(t, l)
	plat.Opener = &recordingOpener{}
	st := rowlessState(a.now)
	tbl := rowlessTable()
	by := state.Party{Name: "beekeeper doctor"}

	lines := a.reopenRowless(t.Context(), st, tbl, rowlessRecord, doctorRun{by: by, dryRun: true})
	if len(lines) != 2 || !strings.HasPrefix(lines[0], `would reopen "worker rowless1" in the desktop (no row in the desktop since its start at`) ||
		!strings.Contains(lines[0], "the desktop runs 2 of its cap of 4 CLIs, 1 reopen under way") ||
		lines[1] != `would leave "worker rowless2" without its desktop row: the desktop runs its cap of 4 CLIs` {
		t.Fatalf("dry run:\n%s", strings.Join(lines, "\n"))
	}
	if len(l.started) != 0 {
		t.Fatalf("a dry run started %+v", l.started)
	}

	lines = a.reopenRowless(t.Context(), st, tbl, rowlessRecord, doctorRun{by: by})
	if len(lines) != 1 || !strings.HasPrefix(lines[0], `reopens "worker rowless1" in the desktop, which gives it its row and warms its CLI (no row in the desktop since its start at`) {
		t.Fatalf("lines:\n%s", strings.Join(lines, "\n"))
	}
	self, _ := os.Executable()
	if len(l.started) != 1 || !strings.HasPrefix(l.started[0].Name, "beekeeper-reopen-rowless1-") || l.started[0].Dir != "/work/rowless1" ||
		strings.Join(l.started[0].Argv, " ") != self+" agents reopen rowless1" {
		t.Fatalf("started %+v, want one reopen unit of rowless1", l.started)
	}
	if !strings.Contains(lines[0], "; unit "+l.started[0].Name+")") {
		t.Errorf("the line does not name the unit: %s", lines[0])
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == "agent.reopen" })
	if err != nil || len(evs) != 1 || !strings.HasPrefix(evs[0].Detail, "worker rowless1: reopens") || evs[0].By.Name != by.Name {
		t.Errorf("agent.reopen events %+v, %v", evs, err)
	}

	// The reopen unit runs: the worker is in hand, and the CLIs the two
	// reopens under way warm fill the cap with the running ones.
	l.running = append(l.running, l.started[0].Name)
	if lines := a.reopenRowless(t.Context(), st, tbl, rowlessRecord, doctorRun{by: by, dryRun: true}); len(lines) != 1 ||
		lines[0] != `would leave "worker rowless2" without its desktop row: the desktop runs its cap of 4 CLIs` {
		t.Errorf("with the reopen running:\n%s", strings.Join(lines, "\n"))
	}
	// Room again (reopen01's reopen ended): the next worker is reopened,
	// and reopen01, a candidate again, waits for room.
	l.running = []string{stoppingUnit, l.started[0].Name}
	lines = a.reopenRowless(t.Context(), st, tbl, rowlessRecord, doctorRun{by: by})
	if len(lines) != 1 || !strings.HasPrefix(lines[0], `reopens "worker rowless2"`) || !strings.Contains(lines[0], "the desktop runs 2 of its cap of 4 CLIs, 1 reopen under way") || len(l.started) != 2 {
		t.Errorf("with room again: %q, started %d", lines, len(l.started))
	}

	// With the desktop down nothing is reopened (rowless1's reopen still
	// runs: it waits for the desktop).
	plat.Opener = stubDesktopDown{}
	l.started = nil
	if lines := a.reopenRowless(t.Context(), st, tbl, rowlessRecord, doctorRun{by: by, dryRun: true}); len(lines) != 2 ||
		lines[0] != `would leave "worker rowless2" without its desktop row: the desktop does not run` {
		t.Errorf("desktop down:\n%s", strings.Join(lines, "\n"))
	}
	if lines := a.reopenRowless(t.Context(), st, tbl, rowlessRecord, doctorRun{by: by}); len(lines) != 0 || len(l.started) != 0 {
		t.Errorf("desktop down reopened: %q, %+v", lines, l.started)
	}
}

// stubDesktopDown is a desktop that does not run.
type stubDesktopDown struct{ platform.Opener }

func (stubDesktopDown) Running(*proc.Table) time.Time { return time.Time{} }

// The watch says once per worker that the desktop never imported it, with
// what shows it: the standby's import beside a running turn, else the
// doctor's reopen; a worker with a row, a fresh start, a finished or idle
// one, a role holder and a session a person started are not in it.
func TestWatchSaysNoDesktopRowOnce(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	orig := plat
	plat = platform.Stub()
	t.Cleanup(func() { plat = orig })
	plat.Opener = &recordingOpener{}
	w.cfg.Claude.DesktopDir = recordDir(t, "local_rowed001")
	w.cfg.Claude.DesktopLog = capLog(t, 3)
	w.table = rowlessTable()
	st := rowlessState(w.now)
	for range 2 {
		w.rowlessAgents(st)
	}
	s := out.String()
	if n := strings.Count(s, "NO DESKTOP ROW"); n != 1 {
		t.Fatalf("%d NO DESKTOP ROW lines, want 1:\n%s", n, s)
	}
	for _, want := range []string{
		"NO DESKTOP ROW: the desktop, running 2 of its cap of 3 CLIs, never imported ",
		`"worker rowless1" (no CLI of it runs; the doctor reopens it once the desktop has room)`,
		`"worker turning1" (its first turn runs headless; the standby watch imports it beside the turn)`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	for _, absent := range []string{"rowed001", "fresh001", "done0001", "idle0001", holderID, "the person's own"} {
		if strings.Contains(s, absent) {
			t.Errorf("%s is in the line:\n%s", absent, s)
		}
	}
	// A rowless worker that comes later is said on its own.
	st.Starts = append(st.Starts, state.Start{Party: state.Party{Session: "later001"}, At: w.now.Add(-time.Hour)})
	st.Agents = append(st.Agents, state.Agent{Party: state.Party{Session: "later001", Name: "worker later001"}, Task: rowlessTask})
	w.rowlessAgents(st)
	if s := out.String(); strings.Count(s, "NO DESKTOP ROW") != 2 || !strings.Contains(s, `never imported "worker later001"`) {
		t.Errorf("a later rowless worker:\n%s", s)
	}
	// With the desktop down the line waits.
	plat.Opener = stubDesktopDown{}
	w.rowlessSaid = nil
	w.rowlessAgents(st)
	if s := out.String(); strings.Count(s, "NO DESKTOP ROW") != 2 {
		t.Errorf("said with the desktop down:\n%s", s)
	}
}

// recordDir is a desktop directory holding a record of each host id.
func recordDir(t *testing.T, hosts ...string) string {
	t.Helper()
	dir := t.TempDir()
	sub := filepath.Join(dir, "user", "sessions")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, h := range hosts {
		if err := os.WriteFile(filepath.Join(sub, h+".json"), []byte(`{"sessionId":"`+h+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// agents marks a started session the running desktop never imported "no
// desktop row" (--json: noDesktopRow) and counts them under its table; one
// with a row and a session a person started carry no mark, and nothing is
// marked while the desktop does not run.
func TestAgentsMarkAWorkerWithoutADesktopRow(t *testing.T) {
	a, out := stubApp(t)
	a.cfg.Claude.DesktopDir = recordDir(t, "local_rowed001")
	plat.Machine = tableMachine{plat.Machine}
	plat.Opener = &recordingOpener{}
	st := rowlessState(a.now)
	views := a.agentViews(st, nil)
	byID := map[string]agentView{}
	for _, v := range views {
		byID[v.Session] = v
	}
	if v := byID[rowlessOne]; !v.NoRow || v.Reachable != notRunning+", no desktop row" {
		t.Errorf("rowless: %+v", v)
	}
	if v := byID["rowed001"]; v.NoRow || v.Reachable != notRunning {
		t.Errorf("with a row: %+v", v)
	}
	if v := byID["own00001"]; v.NoRow || v.Reachable != notRunning {
		t.Errorf("the person's own: %+v", v)
	}
	a.printAgents(views)
	if s := out.String(); !strings.Contains(s, notRunning+", no desktop row") || !strings.Contains(s, "10 agents without a desktop row: the desktop never imported the session") {
		t.Errorf("list:\n%s", s)
	}
	plat.Opener = stubDesktopDown{}
	for _, v := range a.agentViews(st, nil) {
		if v.NoRow || v.Reachable != notRunning {
			t.Errorf("with the desktop down: %+v", v)
		}
	}
}
