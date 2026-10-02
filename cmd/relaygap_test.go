package cmd

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
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
	run := state.Party{Session: "4579230b-e90f-41da-bfea-c92b8aab2f90", HostSession: "local_4579230b-e90f-41da-bfea-c92b8aab2f90", Name: supervisorRole.runName(67)}
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
	eventually(2*time.Second, func() bool { return revived.Load() == 1 })
	if n := revived.Load(); n != 1 {
		t.Fatalf("%d headless resumes, want 1:\n%s", n, out)
	}
	if l := out.String(); !strings.Contains(l, `SUPERVISOR GONE: "Supervisor run 67"`) || !strings.Contains(l, "resuming it headless") {
		t.Errorf("watch lines:\n%s", l)
	}
}

// While the headless turn of a start or wake runs, its holder is not gone.
func TestStandbyWaitsForARunningTurn(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), true)
	run := state.Party{Session: "4579230b-e90f-41da-bfea-c92b8aab2f90", Name: supervisorRole.runName(67)}
	useLauncher(t, &unitLauncher{active: []string{"beekeeper-wake-4579230b-0a1b2c3d.service"}})
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
// window yields to the headless resume the standby started meanwhile: the
// desktop warms no second CLI beside it.
func TestReopenYieldsToAHeadlessResume(t *testing.T) {
	a, out := stubApp(t)
	a.cfg.Desktop.TypingQuiet.Duration = -1
	id := "4579230b-e90f-41da-bfea-c92b8aab2f90"
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
	plat.Launcher = &unitLauncher{active: []string{"beekeeper-wake-4579230b-0a1b2c3d.service"}}
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

func (tableMachine) Processes() (*proc.Table, error) { return &proc.Table{ByPID: map[int]*proc.Process{}}, nil }

// recordingOpener is a running desktop that records the links it opens.
type recordingOpener struct{ opened []string }

func (*recordingOpener) Running(*proc.Table) time.Time { return time.Now() }

func (o *recordingOpener) Open(_ context.Context, url string, _ bool) error {
	o.opened = append(o.opened, url)
	return nil
}
