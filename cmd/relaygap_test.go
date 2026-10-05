package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// runSixtySeven is the session of a relay's successor, wakeSixtySeven the
// unit of its headless resume.
const (
	runSixtySeven  = "4579230b-e90f-41da-bfea-c92b8aab2f90"
	wakeSixtySeven = "beekeeper-wake-4579230b-0a1b2c3d.service"
)

// unitLauncher reports units by their state: active ones always, stopping
// ones (a start's reopen after its turn) only when asked for those too.
type unitLauncher struct {
	platform.Launcher
	active, stopping []string
}

func (l *unitLauncher) Running(_ context.Context, stopping bool, _ ...string) []string {
	if stopping {
		return append(append([]string(nil), l.active...), l.stopping...)
	}
	return l.active
}

// useLauncher puts l in plat for the test.
func useLauncher(t *testing.T, l platform.Launcher) {
	t.Helper()
	orig := plat.Launcher
	plat.Launcher = l
	t.Cleanup(func() { plat.Launcher = orig })
}

// A relay's successor whose first turn took the role and ended, and whose
// reopen waits on a desktop window the person works in, is resumed headless
// past the restart grace: the reopen is no turn.
func TestStandbyResumesASuccessorWhoseReopenWaitsOnTheFocus(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), true)
	run := state.Party{Session: runSixtySeven, HostSession: "local_" + runSixtySeven, Name: supervisorRole.runName(67)}
	l := &unitLauncher{stopping: []string{"beekeeper-agent-4579230b.service"}}
	useLauncher(t, l)
	var revived atomic.Int32
	w.stand = standbyWatch{
		turning: unitsTurning,
		revive: func(_ context.Context, _ role, holder state.Party, msg string) error {
			if !holder.Is(run) || !strings.Contains(msg, "this headless turn keeps") {
				t.Errorf("revive %q: %q", holder.Name, msg)
			}
			revived.Add(1)
			return nil
		},
		succeed: func(context.Context, role, state.Party) (state.Party, error) {
			t.Error("a fresh successor for a successor that can be resumed")
			return state.Party{}, nil
		},
	}
	t.Cleanup(w.stand.inflight.Wait)
	w.now = relayNow
	supervisedBy(t, w, run, relayNow.Add(-3*time.Minute))
	w.pending(context.Background(), nil)
	eventually(2*time.Second, func() bool { return revived.Load() == 1 && !w.stand.starting.Load() })
	if n := revived.Load(); n != 1 {
		t.Fatalf("%d headless resumes, want 1:\n%s", n, out)
	}
	if l := out.String(); !strings.Contains(l, `SUPERVISOR GONE: "Supervisor run 67"`) || !strings.Contains(l, "resuming it headless") {
		t.Errorf("watch lines:\n%s", l)
	}
}

// A successor that asked for a desktop turn (agents desktop) is not resumed
// headless while its start unit runs the reopen: the reopen goes ahead at
// once and warms the desktop's CLI, which a resume would run beside.
func TestStandbyWaitsForAnUrgentReopen(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), true)
	run := state.Party{Session: runSixtySeven, HostSession: "local_" + runSixtySeven, Name: supervisorRole.runName(67)}
	useLauncher(t, &unitLauncher{stopping: []string{"beekeeper-agent-4579230b.service"}})
	w.stand = standbyWatch{
		turning:   unitsTurning,
		reopening: unitsReopening,
		revive: func(context.Context, role, state.Party, string) error {
			t.Error("resumed a holder whose reopen shows it at once")
			return nil
		},
	}
	w.now = relayNow
	supervisedBy(t, w, run, relayNow.Add(-3*time.Minute))
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = append(st.Agents, state.Agent{Party: run, DesktopTurn: relayNow.Add(-time.Minute)})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	w.pending(context.Background(), nil)
	w.stand.inflight.Wait()
	if strings.Contains(out.String(), "GONE") {
		t.Errorf("an urgent reopen said gone:\n%s", out)
	}
}

// While the headless turn of a start or wake runs, its holder is not gone.
func TestStandbyWaitsForARunningTurn(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), true)
	run := state.Party{Session: runSixtySeven, Name: supervisorRole.runName(67)}
	useLauncher(t, &unitLauncher{active: []string{wakeSixtySeven}})
	w.stand = standbyWatch{
		turning: unitsTurning,
		revive: func(context.Context, role, state.Party, string) error {
			t.Error("resumed a holder whose turn runs")
			return nil
		},
	}
	w.now = relayNow
	supervisedBy(t, w, run, relayNow.Add(-3*time.Minute))
	w.pending(context.Background(), nil)
	if strings.Contains(out.String(), "GONE") {
		t.Errorf("a running turn said gone:\n%s", out)
	}
}

// A start's reopen that waited out the person's work in the desktop's
// window yields to the headless resume the standby started meanwhile when
// the desktop imported the session already: the desktop warms no second
// CLI beside it.
func TestReopenYieldsToAHeadlessResume(t *testing.T) {
	a, out := stubApp(t)
	a.cfg.Desktop.TypingQuiet.Duration = -1
	id := runSixtySeven
	a.cfg.Claude.DesktopDir = t.TempDir()
	desktopRecord(t, a.cfg.Claude.DesktopDir, "local_"+id, id, false)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		p := state.Party{Session: id, Name: supervisorRole.runName(67)}
		st.Starts = append(st.Starts, state.Start{Party: p, Mode: state.ModeBypass, At: time.Now()})
		st.Agents = append(st.Agents, state.Agent{Party: p})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	saved := desktopWindowActive
	t.Cleanup(func() { desktopWindowActive = saved })
	desktopWindowActive = func(context.Context) (bool, error) { return false, nil }
	plat.Machine = tableMachine{plat.Machine}
	o := &recordingOpener{}
	plat.Opener = o
	plat.Launcher = &unitLauncher{active: []string{wakeSixtySeven}}
	c := a.agentReopenCmd()
	c.SetArgs([]string{id})
	if err := c.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(o.opened) > 0 {
		t.Errorf("opened %v beside the headless resume", o.opened)
	}
	if !strings.Contains(out.String(), "resumed headless meanwhile") {
		t.Errorf("output: %s", out)
	}
}

// wakeLaterLauncher runs the wake unit from its second look on: a resume
// the standby started while a reopen waited for the person.
type wakeLaterLauncher struct {
	platform.Launcher
	looks atomic.Int32
}

func (l *wakeLaterLauncher) Running(context.Context, bool, ...string) []string {
	if l.looks.Add(1) < 2 {
		return nil
	}
	return []string{wakeSixtySeven}
}

// A resume that starts while the reopen waits for the person's typing to
// pause is found right before the show: the reopen yields, and the desktop
// warms no CLI beside it.
func TestReopenYieldsToAResumeStartedDuringItsWait(t *testing.T) {
	a, out := stubApp(t)
	a.cfg.Desktop.TypingQuiet.Duration = -1
	id := runSixtySeven
	a.cfg.Claude.DesktopDir = t.TempDir()
	desktopRecord(t, a.cfg.Claude.DesktopDir, "local_"+id, id, false)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		p := state.Party{Session: id, Name: supervisorRole.runName(67)}
		st.Starts = append(st.Starts, state.Start{Party: p, Mode: state.ModeBypass, At: time.Now()})
		st.Agents = append(st.Agents, state.Agent{Party: p})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	saved := desktopWindowActive
	t.Cleanup(func() { desktopWindowActive = saved })
	desktopWindowActive = func(context.Context) (bool, error) { return false, nil }
	plat.Machine = tableMachine{plat.Machine}
	o := &recordingOpener{}
	plat.Opener = o
	plat.Launcher = &wakeLaterLauncher{}
	c := a.agentReopenCmd()
	c.SetArgs([]string{id})
	if err := c.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(o.opened) > 0 {
		t.Errorf("opened %v beside the headless resume", o.opened)
	}
	if !strings.Contains(out.String(), "resumed headless meanwhile") {
		t.Errorf("output: %s", out)
	}
}

// tableMachine reads an empty process table.
type tableMachine struct{ platform.Machine }

func (tableMachine) Processes() (*proc.Table, error) {
	return &proc.Table{ByPID: map[int]*proc.Process{}}, nil
}

// recordingOpener is a running desktop that records the links it opens.
type recordingOpener struct{ opened []string }

func (*recordingOpener) Running(*proc.Table) time.Time { return time.Now() }

func (o *recordingOpener) Open(_ context.Context, url string, _ bool) error {
	o.opened = append(o.opened, url)
	return nil
}

// A reopen that finds the standby's headless resume running and a session
// the desktop never imported imports it now, beside that turn: the resume
// keeps a role's watch and does not end before the next relay, and the
// person sees the role's holder only in the desktop's sidebar. The resume
// stays frozen while the desktop reads the transcript and remains the
// session's only CLI.
func TestReopenImportsBesideAHeadlessResume(t *testing.T) {
	a, out := stubApp(t)
	a.cfg.Desktop.TypingQuiet.Duration = -1
	a.cfg.Claude.DesktopDir = t.TempDir()
	id, name := runSixtySeven, supervisorRole.runName(67)
	transcript := filepath.Join(a.cfg.Claude.ProjectsDir, "lab", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte(`{"type":"user","sessionId":"`+id+`"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		p := state.Party{Session: id, HostSession: "local_" + id, Name: name}
		st.Starts = append(st.Starts, state.Start{Party: p, Mode: state.ModeBypass, At: time.Now()})
		st.Agents = append(st.Agents, state.Agent{Party: p})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	saved := desktopWindowActive
	t.Cleanup(func() { desktopWindowActive = saved })
	desktopWindowActive = func(context.Context) (bool, error) { return false, nil }
	// The desktop's CLI the import warms, which the reopen stops.
	twin := exec.Command("sleep", "60")
	if err := twin.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = twin.Process.Kill(); _ = twin.Wait() })
	m := &importMachine{Machine: plat.Machine, id: id, twin: twin.Process.Pid}
	plat.Machine = m
	l := &freezingLauncher{unitLauncher: unitLauncher{active: []string{wakeSixtySeven}}}
	plat.Launcher = l
	o := &importingOpener{dir: a.cfg.Claude.DesktopDir, id: id, title: name, machine: m}
	plat.Opener = o
	c := a.agentReopenCmd()
	c.SetArgs([]string{id})
	if err := c.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(o.opened, []string{resumeURL(id)}) {
		t.Errorf("opened %v, want the import\n%s", o.opened, out)
	}
	if !o.frozen || !slices.Equal(l.thawed, []string{wakeSixtySeven}) {
		t.Errorf("the resume frozen during the import %v, thawed %v", o.frozen, l.thawed)
	}
	if err := twin.Wait(); err == nil {
		t.Error("the desktop's CLI beside the resume still ran")
	}
	if !strings.Contains(out.String(), "imported local_"+id+" into the desktop beside its headless turn") {
		t.Errorf("output:\n%s", out)
	}
	raw, _ := os.ReadFile(filepath.Clean(transcript))
	if !strings.Contains(string(raw), `"customTitle":"`+name+`"`) {
		t.Errorf("the transcript is not titled: %s", raw)
	}
}

// freezingLauncher records the units it freezes and thaws.
type freezingLauncher struct {
	unitLauncher
	frozen, thawed []string
}

func (l *freezingLauncher) Freeze(_ context.Context, unit string) error {
	l.frozen = append(l.frozen, unit)
	return nil
}

func (l *freezingLauncher) Thaw(_ context.Context, unit string) error {
	l.thawed = append(l.thawed, unit)
	return nil
}

func (*freezingLauncher) State(context.Context, string) string { return "active" }

// importMachine runs the headless resume of session id, and the desktop's
// CLI twin once the desktop imported it.
type importMachine struct {
	platform.Machine
	id       string
	twin     int
	imported bool
}

func (m *importMachine) Processes() (*proc.Table, error) {
	t := &proc.Table{ByPID: map[int]*proc.Process{
		5: {PID: 5, Comm: claudeComm, Args: []string{claudeComm, "-p", resumeFlag, m.id, "--", "keep the watch"}},
	}}
	if m.imported {
		t.ByPID[m.twin] = &proc.Process{PID: m.twin, Comm: claudeComm, Args: []string{claudeComm, resumeFlag, m.id}}
	}
	return t, nil
}

// importingOpener is a running desktop whose import writes the session's
// record, titled, and warms its CLI; frozen says whether the launcher had
// frozen the resume when the link opened.
type importingOpener struct {
	recordingOpener
	dir, id, title string
	machine        *importMachine
	frozen         bool
}

func (o *importingOpener) Open(ctx context.Context, url string, running bool) error {
	l, _ := plat.Launcher.(*freezingLauncher)
	o.frozen = l != nil && len(l.frozen) > 0 && len(l.thawed) == 0
	d := filepath.Join(o.dir, "a", "b")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	host := "local_" + o.id
	raw := `{"sessionId":"` + host + `","cliSessionId":"` + o.id + `","title":"` + o.title + `"}`
	if err := os.WriteFile(filepath.Join(d, host+".json"), []byte(raw), 0o600); err != nil {
		return err
	}
	o.machine.imported = true
	return o.recordingOpener.Open(ctx, url, running)
}
