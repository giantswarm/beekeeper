package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

var nestedParent = state.Party{Session: "s-worker", HostSession: "local_s-worker", Name: "BK worker"}

// chainMachine runs this test process at the end of a parent chain whose
// argvs are given outermost first, under PIDs no process has.
type chainMachine struct {
	platform.Machine
	argvs [][]string
}

func (m chainMachine) Processes() (*proc.Table, error) {
	const first = 1 << 23
	t := &proc.Table{ByPID: map[int]*proc.Process{}}
	for i, argv := range m.argvs {
		t.ByPID[first+i] = &proc.Process{PID: first + i, PPID: first + i - 1, Comm: argv[0], Args: argv}
	}
	self, comm := os.Getpid(), filepath.Base(os.Args[0])
	t.ByPID[self] = &proc.Process{PID: self, PPID: first + len(m.argvs) - 1, Comm: comm, Args: []string{comm}}
	return t, nil
}

// nestedApp is the worker nestedParent on the roster, busy with a task, and
// this process a command its environment names the worker's, run under
// argvs.
func nestedApp(t *testing.T, argvs ...[]string) (*app, state.Store) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: nestedParent, Task: "the worker's task", AssignedAt: keepNow}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", nestedParent.Session)
	t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", nestedParent.HostSession)
	t.Setenv("CLAUDE_CODE_SESSION_NAME", nestedParent.Name)
	was := plat.Machine
	plat.Machine = chainMachine{Machine: plat.Machine, argvs: argvs}
	t.Cleanup(func() { plat.Machine = was })
	cfg := &config.Config{StateDir: dir}
	cfg.Claude.SessionsDir, cfg.Claude.DesktopDir, cfg.Claude.ProjectsDir = t.TempDir(), t.TempDir(), t.TempDir()
	cfg.Omp.SessionsDir = t.TempDir()
	return &app{cfg: cfg, store: store, now: keepNow, out: &bytes.Buffer{}}, store
}

// A claude -p a worker's tool started carries the worker's session ids:
// its register is refused naming the worker, whose entry stays as it was.
// The worker's own command registers as before.
func TestRegisterFromANestedClaude(t *testing.T) {
	desktop, cli, shell := []string{"claude-desktop"}, []string{claudeComm}, []string{"zsh"}
	nested := []string{claudeComm, "-p", "--", "register as Nested X"}

	a, store := nestedApp(t, desktop, cli, shell, nested, shell)
	_, err := runAgents(a, "register", "--name", "Nested X")
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), `"BK worker" (s-worker)`) {
		t.Fatalf("register from the nested claude: %v", err)
	}
	st, _ := store.Read()
	if len(st.Agents) != 1 || st.Agents[0].Party != nestedParent || st.Agents[0].Task != "the worker's task" {
		t.Fatalf("roster after the refusal: %+v", st.Agents)
	}

	a, store = nestedApp(t, desktop, cli, shell)
	if out, err := runAgents(a, "register"); err != nil || !strings.Contains(out, `BK worker busy with "the worker's task"`) {
		t.Fatalf("the worker's own register: %q, %v", out, err)
	}
	if st, _ = store.Read(); len(st.Agents) != 1 || st.Agents[0].Name != nestedParent.Name {
		t.Errorf("roster after the worker's register: %+v", st.Agents)
	}
}
