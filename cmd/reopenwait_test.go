package cmd

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// waitingReopen starts the reopen of the rotation worker on a desktop whose
// window keeps the focus, and returns once it records its wait; done
// carries its result.
func waitingReopen(t *testing.T, a *app, ctx context.Context) (done chan error) {
	t.Helper()
	saved, savedPoll := desktopWindowActive, reopenPoll
	t.Cleanup(func() { desktopWindowActive, reopenPoll = saved, savedPoll })
	desktopWindowActive = func(context.Context) (bool, error) { return true, nil }
	reopenPoll = 20 * time.Millisecond
	plat.Machine = tableMachine{plat.Machine}
	plat.Opener = &recordingOpener{}
	plat.Launcher = &unitLauncher{}
	done = make(chan error, 1)
	go func() { done <- a.reopenSession(ctx, rotationLogin, "") }()
	if !eventually(5*time.Second, func() bool {
		st, _ := a.store.Read()
		i := agentOfSession(st, rotationLogin)
		return i >= 0 && st.Agents[i].Import != nil
	}) {
		t.Fatal("the reopen recorded no wait")
	}
	return done
}

// A second reopen of a session whose reopen waits leaves the showing to
// that one and ends at once: waking a session three times leaves one
// waiter.
func TestOneReopenWaitsPerSession(t *testing.T) {
	a, out := stubApp(t)
	a.cfg.Desktop.TypingQuiet.Duration = -1
	rosterAgent(t, a, state.Agent{Party: rotationParty, Task: rotationTask})
	ctx, cancel := context.WithCancel(t.Context())
	done := waitingReopen(t, a, ctx)
	for range 2 {
		second := *a
		var said bytes.Buffer
		second.out = &said
		start := time.Now()
		if err := second.reopenSession(t.Context(), "local_"+rotationLogin, ""); err != nil {
			t.Fatal(err)
		}
		if took := time.Since(start); took > time.Second {
			t.Errorf("the second reopen took %s", took)
		}
		if !strings.Contains(said.String(), "already waits to show it, left to that one") {
			t.Errorf("second reopen:\n%s", said.String())
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "showed local_") {
		t.Errorf("the waiting reopen showed the session:\n%s", out)
	}
}

// A waiting reopen ends within one poll once its agent reports its work
// done or leaves the roster, and clears its recorded wait.
func TestReopenEndsWithItsAgent(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(st *state.State)
		want string
	}{
		{"reported done", func(st *state.State) { st.Agents[agentOfSession(st, rotationLogin)].Done = true }, "left closed: it reported its work done"},
		{"removed", func(st *state.State) {
			st.Agents = slices.DeleteFunc(st.Agents, func(ag state.Agent) bool { return ag.Session == rotationLogin })
		}, "left closed: it left the roster"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, out := stubApp(t)
			a.cfg.Desktop.TypingQuiet.Duration = -1
			rosterAgent(t, a, state.Agent{Party: rotationParty, Task: rotationTask})
			done := waitingReopen(t, a, t.Context())
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				tc.end(st)
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the reopen still waits")
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("output lacks %q:\n%s", tc.want, out)
			}
			st, _ := a.store.Read()
			if i := agentOfSession(st, rotationLogin); i >= 0 && st.Agents[i].Import != nil {
				t.Errorf("the ended reopen left its wait: %+v", st.Agents[i].Import)
			}
		})
	}
}

// A reopen of a session whose desktop CLI runs already shows nothing and
// keeps that CLI as the warmed one.
func TestReopenKeepsARunningDesktopCLI(t *testing.T) {
	a, out := stubApp(t)
	a.cfg.Desktop.TypingQuiet.Duration = -1
	rosterAgent(t, a, state.Agent{Party: rotationParty, Task: rotationTask, DesktopTurn: time.Now()})
	warm := &atomic.Bool{}
	warm.Store(true)
	plat.Machine = warmMachine{Machine: plat.Machine, id: rotationLogin, warm: warm}
	o := &warmingOpener{warm: warm}
	plat.Opener = o
	plat.Launcher = &unitLauncher{}
	// The retitle after the show waits for a steward none runs here.
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := a.reopenSession(ctx, rotationLogin, ""); err != nil {
		t.Fatal(err)
	}
	if len(o.opened) != 0 {
		t.Errorf("opened %v beside the running desktop CLI", o.opened)
	}
	for _, want := range []string{"no show of local_" + rotationLogin + ": the desktop runs its CLI (PID 7)", "reopen: the desktop runs its CLI (PID 7)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	st, _ := a.store.Read()
	if ag := st.Agents[agentOfSession(st, rotationLogin)]; !ag.DesktopTurn.IsZero() {
		t.Error("the ask for a desktop turn outlived the running desktop CLI")
	}
}
