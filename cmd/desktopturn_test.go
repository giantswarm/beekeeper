package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A turn runs in the desktop only where the desktop can run it: a desktop
// session with a row, the desktop running, its CLI or a steward to take
// the message; otherwise it names why, and the caller's turn runs headless.
func TestTurnInDesktopSaysWhyNot(t *testing.T) {
	a, _ := stubApp(t)
	a.cfg.Claude.DesktopDir = t.TempDir()
	id := "d" + runSixtySeven[1:]
	w := wakeTarget{name: agentTwentyEight, id: id, host: "local_" + id, dir: t.TempDir(), mode: state.ModeBypass}
	if _, err := a.turnInDesktop(t.Context(), wakeTarget{name: w.name, id: id}, "go", 0); err == nil || !strings.Contains(err.Error(), "no desktop session") {
		t.Errorf("no desktop session: %v", err)
	}
	if _, err := a.turnInDesktop(t.Context(), w, "go", 0); err == nil || !strings.Contains(err.Error(), "no row") {
		t.Errorf("no row: %v", err)
	}
	rec := filepath.Join(a.cfg.Claude.DesktopDir, "u", "o", w.host+".json")
	if err := os.MkdirAll(filepath.Dir(rec), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rec, []byte(`{"sessionId":"`+w.host+`","cliSessionId":"`+id+`","cwd":"/tmp","title":"`+w.name+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plat.Machine = tableMachine{plat.Machine}
	if _, err := a.turnInDesktop(t.Context(), w, "go", 0); err == nil || !strings.Contains(err.Error(), "desktop does not run") {
		t.Errorf("desktop not running: %v", err)
	}
	plat.Opener = &recordingOpener{}
	if _, err := a.turnInDesktop(t.Context(), w, "go", 0); err == nil || !strings.Contains(err.Error(), "no idle desktop CLI") {
		t.Errorf("no CLI and no steward: %v", err)
	}
}

// A seed turn has no tools and no MCP servers, and its prompt is the brief
// with the note that it only creates the session.
func TestSeedArgv(t *testing.T) {
	argv := agentArgv("claude", "s1", agentTwentyEight, "", "brief\n\n"+seedNote, "--tools", "", "--strict-mcp-config")
	i := slices.Index(argv, "--tools")
	if i < 0 || argv[i+1] != "" || !slices.Contains(argv, "--strict-mcp-config") {
		t.Fatalf("seed argv %q lacks its tool limits", argv)
	}
	if last := argv[len(argv)-1]; argv[len(argv)-2] != "--" || !strings.HasSuffix(last, seedNote) {
		t.Fatalf("seed prompt %q", last)
	}
}

// A warm CLI must accept the task itself. Its existence is not evidence
// that a steward delivered anything, and a failed send gets one retry.
func TestStartDesktopTurnWarmCLI(t *testing.T) {
	for _, tc := range []struct {
		name         string
		failures     int
		noSocket     bool
		readyOnRetry bool
		wantSends    int
		wantErr      bool
	}{
		{name: "first attempt", wantSends: 1},
		{name: "retry", failures: 1, wantSends: 2},
		{name: "socket becomes ready on retry", noSocket: true, readyOnRetry: true, wantSends: 1},
		{name: "send fails twice", failures: 2, wantSends: 2, wantErr: true},
		{name: "still warming", noSocket: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const id = "start-warm"
			a, launcher, _ := wakeTwinSetup(t, id, true)
			a.cfg.Claude.DesktopDir = t.TempDir()
			plat.Opener = &recordingOpener{}
			w := wakeTarget{name: litWorker, id: id, host: "local_" + id, model: sonnetModel}
			rec := filepath.Join(a.cfg.Claude.DesktopDir, "u", "o", w.host+".json")
			writeFile(t, rec, `{"sessionId":"`+w.host+`","cliSessionId":"`+id+`","title":"Worker","model":"`+sonnetModel+`"}`)
			if tc.noSocket {
				if err := os.Remove(peerSocket(os.Getenv("XDG_RUNTIME_DIR"), 7)); err != nil {
					t.Fatal(err)
				}
			}
			if tc.readyOnRetry {
				plat.Machine = &readySocketMachine{Machine: plat.Machine, ready: func() {
					writeFile(t, peerSocket(os.Getenv("XDG_RUNTIME_DIR"), 7), "")
				}}
			}
			calls := 0
			cliSend = func(_ context.Context, _ *app, to, msg string) error {
				calls++
				if to != "uds:"+peerSocket(os.Getenv("XDG_RUNTIME_DIR"), 7) || msg != taskTurn {
					t.Fatalf("unexpected delivery: %q %q", to, msg)
				}
				r, ok := claude.ReadRecord(a.cfg, w.host)
				if !ok || r.Model != w.model {
					t.Fatalf("model before delivery = %+v", r)
				}
				if calls <= tc.failures {
					return errors.New("not ready")
				}
				return nil
			}
			_, err := a.startDesktopTurn(t.Context(), w, taskTurn, 0)
			if (err != nil) != tc.wantErr || calls != tc.wantSends {
				t.Fatalf("delivery = %v, sends %d; want error %v, sends %d", err, calls, tc.wantErr, tc.wantSends)
			}
			if tc.wantErr {
				var delivery desktopDeliveryError
				if !errors.As(err, &delivery) {
					t.Fatalf("error permits headless fallback: %v", err)
				}
				p := state.Party{Name: w.name, Session: id, HostSession: w.host}
				rosterAgent(t, a, state.Agent{Party: p})
				if err := a.undelivered(waker, p, err); err == nil {
					t.Fatal("undelivered start succeeded")
				}
				st, _ := a.store.Read()
				if st.Agents[0].Undelivered == "" {
					t.Fatal("missing undelivered marker")
				}
			}
			if len(launcher.units) != 0 {
				t.Fatal("started a headless turn beside the warm CLI")
			}
		})
	}
}

// The first attempt sees a live CLI without a socket. On the second
// attempt the CLI finishes warming, without a wall-clock dependent test.
type readySocketMachine struct {
	platform.Machine
	reads int
	ready func()
}

func (m *readySocketMachine) Processes() (*proc.Table, error) {
	m.reads++
	if m.reads == 5 {
		m.ready()
	}
	return m.Machine.Processes()
}
