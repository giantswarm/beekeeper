package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

const systemdProcessName = "systemd"

func orphanWorktree(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, ".git"), []byte("gitdir: fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestWorktreeOrphans(t *testing.T) {
	dir := orphanWorktree(t)
	other := orphanWorktree(t)
	now := time.Now()
	ended := state.Party{Session: "ended-session", Name: "ended worker"}
	active := state.Party{Session: "active", Name: "active worker"}
	start := state.Start{Party: ended, Dir: dir, At: now.Add(-time.Hour)}
	tb := proc.NewTable(
		&proc.Process{PID: 10, PPID: 1, Start: now.Add(-time.Minute)},
		&proc.Process{PID: 11, PPID: 10, Start: now.Add(-time.Minute)},
		&proc.Process{PID: 12, PPID: 1, Start: now.Add(-2 * time.Hour)},
		&proc.Process{PID: 13, PPID: 1, Start: now.Add(-time.Minute)},
		&proc.Process{PID: 14, PPID: 99, Start: now.Add(-time.Minute)},
		&proc.Process{PID: 99, PPID: 1, Comm: systemdProcessName},
	)
	cwd := func(pid int) string {
		if pid == 13 {
			return other
		}
		if pid == 99 {
			return "/"
		}
		return dir
	}
	env := func(int) (map[string]string, error) { return map[string]string{}, nil }
	cases := []struct {
		name     string
		st       state.State
		sessions []*claude.Session
		env      func(int) (map[string]string, error)
		want     []int
	}{
		{name: "ended server and children", st: state.State{Starts: []state.Start{start}}, want: []int{10, 11, 14}},
		{name: "rostered even done", st: state.State{Starts: []state.Start{start}, Agents: []state.Agent{{Party: ended, Done: true}}}},
		{name: "shared worktree", st: state.State{Starts: []state.Start{start, {Party: active, Dir: dir}}, Agents: []state.Agent{{Party: active}}}},
		{name: "live unregistered CLI", st: state.State{Starts: []state.Start{start}}, sessions: []*claude.Session{{Cwd: dir}}},
		{name: "unknown worktree", st: state.State{}},
		{name: "manually registered agent with servers started before registration", st: state.State{EndedWorkers: []state.Start{{Party: ended, At: now}}}, env: func(int) (map[string]string, error) {
			return map[string]string{orphanSessionEnv: ended.Session}, nil
		}, want: []int{10, 11, 12, 13, 14}},
		{name: "different session identity", st: state.State{Starts: []state.Start{start}}, env: func(int) (map[string]string, error) {
			return map[string]string{orphanSessionEnv: "unknown-other-session"}, nil
		}},
		{name: "remote start", st: state.State{Starts: []state.Start{{Party: ended, Dir: dir, At: start.At, Execution: &state.ExecutionRecord{}}}}},
		{name: "active worker changed directory", st: state.State{Starts: []state.Start{start}, Agents: []state.Agent{{Party: active}}}, env: func(int) (map[string]string, error) {
			return map[string]string{orphanSessionEnv: active.Session}, nil
		}},
		{name: "ended worker changed directory", st: state.State{Starts: []state.Start{{Party: ended, Dir: other, At: start.At}}}, env: func(int) (map[string]string, error) {
			return map[string]string{orphanSessionEnv: ended.Session}, nil
		}, want: []int{10, 11, 12, 13, 14}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			readEnv := env
			if tc.env != nil {
				readEnv = tc.env
			}
			got := worktreeOrphans(&tc.st, tb, tc.sessions, cwd, readEnv)
			var pids []int
			for _, o := range got {
				pids = append(pids, o.process.PID)
			}
			if !slices.Equal(pids, tc.want) {
				t.Fatalf("pids %v, want %v", pids, tc.want)
			}
		})
	}
}

// An actual adopted server, TERM ignored, exercises escalation and the log
// against scratch state. No test signals an unrelated machine process.
func TestStopWorktreeOrphansLive(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("requires Linux proc")
	}
	dir := orphanWorktree(t)
	launch := exec.Command("sh", "-c", "(trap '' TERM; exec sleep 60) </dev/null >/dev/null 2>&1 & echo $!")
	launch.Dir = dir
	launch.Env = []string{"PATH=" + os.Getenv("PATH"), "CLAUDE_CODE_SESSION_ID=orphan-live"}
	raw, err := launch.Output()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p, _ := os.FindProcess(pid)
		if p != nil {
			_ = p.Kill()
		}
	})
	a, out := stubApp(t)
	plat.Machine = orphanProcMachine{plat.Machine}
	owner := state.Party{Session: "orphan-live", Name: "orphan live"}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = append(st.Starts, state.Start{Party: owner, Dir: dir, At: time.Now().Add(-time.Minute)})
		st.Agents = append(st.Agents, state.Agent{Party: owner, Registered: time.Now().Add(-time.Minute)})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	lines, err := a.stopWorktreeOrphans(owner, false)
	if err != nil || len(lines) != 0 || proc.Cwd(pid) != dir {
		t.Fatalf("rostered server touched: %v %v", lines, err)
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) { removeAgent(st, len(st.Agents)-1); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	a.json = true
	if err := a.psCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	var rows []psRow
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(rows, func(r psRow) bool { return r.PID == pid && r.Orphan && r.Worktree == dir && r.Age != "" }) {
		t.Fatal("ps did not flag the orphan with its worktree and age")
	}
	lines, err = a.stopWorktreeOrphans(owner, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(lines, "\n"), fmt.Sprint(pid)) {
		t.Fatalf("dry run: %v", lines)
	}
	if proc.Cwd(pid) != dir {
		t.Fatal("dry run stopped the server")
	}
	lines, err = a.stopWorktreeOrphans(owner, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(lines, "\n"), fmt.Sprint(pid)) {
		t.Fatalf("stop: %v", lines)
	}
	// A killed adopted child may remain a zombie until PID 1 reaps it, but
	// it no longer holds a working directory or any server resources.
	for deadline := time.Now().Add(time.Second); proc.Cwd(pid) != "" && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if cwd := proc.Cwd(pid); cwd != "" {
		t.Fatalf("server still holds cwd %q", cwd)
	}
	events, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == orphanArchiveVerb })
	if err != nil || len(events) != 1 {
		t.Fatalf("archive log: %v, %v", events, err)
	}
}

type orphanProcMachine struct{ platform.Machine }

func (orphanProcMachine) Processes() (*proc.Table, error) { return proc.Read() }

func TestRemoveAgentKeepsOrphanIdentity(t *testing.T) {
	owner := state.Party{Session: "manual-session", Name: "manual worker"}
	st := &state.State{Agents: []state.Agent{{Party: owner, Registered: time.Now()}}}
	removeAgent(st, 0)
	if len(st.EndedWorkers) != 1 || !st.EndedWorkers[0].Is(owner) || len(st.Starts) != 0 {
		t.Fatalf("removed identity: %+v", st.EndedWorkers)
	}
}

func TestWorktreeOrphansHarnessIdentities(t *testing.T) {
	const rawID = "worker-id"
	for _, harness := range []struct{ name, env, prefix string }{
		{name: codex.Harness, env: codex.EnvAgent, prefix: codex.HostPrefix},
		{name: omp.Harness, env: omp.EnvAgent, prefix: omp.HostPrefix},
		{name: agy.Harness, env: agy.EnvAgent, prefix: agy.HostPrefix},
		{name: claudeComm, env: orphanSessionEnv},
	} {
		t.Run(harness.name, func(t *testing.T) {
			target, origin := orphanWorktree(t), orphanWorktree(t)
			now := time.Now()
			owner := state.Party{HostSession: harness.prefix + rawID}
			if harness.prefix == "" {
				owner = state.Party{Session: rawID}
			}
			tb := proc.NewTable(&proc.Process{PID: 10, PPID: 1, Start: now.Add(-time.Minute)}, &proc.Process{PID: 11, PPID: 10, Start: now.Add(-time.Minute)})
			cwd := func(int) string { return target }
			env := func(pid int) (map[string]string, error) {
				if pid == 11 {
					return map[string]string{}, nil
				}
				// A hosted harness may inherit its launching Claude session too.
				e := map[string]string{orphanSessionEnv: "launching-session"}
				e[harness.env] = rawID
				return e, nil
			}
			for _, dir := range []string{target, origin} {
				t.Run("ended-"+filepath.Base(dir), func(t *testing.T) {
					st := &state.State{Starts: []state.Start{{Party: owner, Dir: dir, At: now.Add(-time.Hour)}}}
					got := worktreeOrphans(st, tb, nil, cwd, env)
					// The identified parent is owned even after changing worktree; a
					// child without identity needs the recorded directory as evidence.
					want := 1
					if dir == target {
						want = 2
					}
					if len(got) != want || !got[0].owner.Is(owner) {
						t.Fatalf("ended %s: %+v, want %d", harness.name, got, want)
					}
				})
			}
			t.Run("active-changed-directory-cleared-child", func(t *testing.T) {
				st := &state.State{Agents: []state.Agent{{Party: owner}}, Starts: []state.Start{
					{Party: state.Party{Session: "previous-worker"}, Dir: target, At: now.Add(-time.Hour)},
					{Party: owner, Dir: origin, At: now.Add(-time.Hour)},
				}}
				if got := worktreeOrphans(st, tb, nil, cwd, env); len(got) != 0 {
					t.Fatalf("rostered %s's cleared child selected: %+v", harness.name, got)
				}
			})
			t.Run("manual-removed-identity", func(t *testing.T) {
				st := &state.State{Agents: []state.Agent{{Party: owner, Registered: now}}}
				removeAgent(st, 0)
				got := worktreeOrphans(st, tb, nil, cwd, env)
				if len(got) != 1 || !got[0].owner.Is(owner) {
					t.Fatalf("removed %s: %+v", harness.name, got)
				}
			})
		})
	}
}
