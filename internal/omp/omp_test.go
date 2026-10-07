package omp

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// testdata/busy.jsonl and idle.jsonl are omp 18.4 session files reduced to
// the entries beekeeper reads, with neutral paths and fake ids: busy ends on
// a failed tool result its turn still answers, idle on a finished reply and
// a title change.
const (
	busyID = "01a0f6fc-0000-7000-8000-000000000001"
	idleID = "01a0f6fc-0000-7000-8000-000000000002"
	repo   = "/work/beekeeper"
)

// fakeProc is a process of a synthetic /proc.
type fakeProc struct {
	pid, ppid int
	args      []string
	cwd       string
	env       []string
	// age is how long before now it started.
	age time.Duration
}

// procRoot writes a synthetic /proc booted boot ago with ps.
func procRoot(t *testing.T, now time.Time, ps ...fakeProc) *proc.Table {
	t.Helper()
	root := t.TempDir()
	boot := now.Add(-time.Hour).Truncate(time.Second)
	write(t, filepath.Join(root, "stat"), fmt.Sprintf("cpu 0\nbtime %d\n", boot.Unix()))
	for _, p := range ps {
		dir := filepath.Join(root, strconv.Itoa(p.pid))
		ticks := int64(now.Add(-p.age).Sub(boot) / (time.Second / 100))
		comm := filepath.Base(p.args[0])
		// Fields 3 on: state ppid, then zeros up to starttime (22) and rss (24).
		f := make([]string, 22)
		for i := range f {
			f[i] = "0"
		}
		f[0], f[1], f[19], f[21] = "S", strconv.Itoa(p.ppid), strconv.FormatInt(ticks, 10), "10"
		write(t, filepath.Join(dir, "stat"), fmt.Sprintf("%d (%s) %s\n", p.pid, comm, strings.Join(f, " ")))
		write(t, filepath.Join(dir, "cmdline"), strings.Join(p.args, "\x00")+"\x00")
		write(t, filepath.Join(dir, "environ"), strings.Join(p.env, "\x00")+"\x00")
		if p.cwd != "" {
			if err := os.Symlink(p.cwd, filepath.Join(dir, "cwd")); err != nil {
				t.Fatal(err)
			}
		}
	}
	tab, err := proc.ReadAt(root)
	if err != nil {
		t.Fatal(err)
	}
	return tab
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil { //nolint:gosec // under the test's temporary folder
		t.Fatal(err)
	}
}

// recorded is a recorded session file's layout: created and last written
// so long before now.
type recorded struct{ created, written time.Duration }

// sessionsDir lays the recorded session files out as omp does, with a
// subagent's file in the busy session's folder.
func sessionsDir(t *testing.T, now time.Time, busy, idle recorded) string {
	t.Helper()
	dir := t.TempDir()
	project := filepath.Join(dir, "-work-beekeeper")
	for _, f := range []struct {
		file, id, header string
		r                recorded
	}{
		{"busy.jsonl", busyID, "2026-10-01T10:00:00.000Z", busy},
		{"idle.jsonl", idleID, "2026-10-01T09:00:00.000Z", idle},
	} {
		raw, err := os.ReadFile(filepath.Join("testdata", f.file)) //nolint:gosec // the test's recorded files
		if err != nil {
			t.Fatal(err)
		}
		id, header, r := f.id, f.header, f.r
		content := strings.Replace(string(raw), `"timestamp":"`+header+`"`, `"timestamp":"`+now.Add(-r.created).UTC().Format(time.RFC3339Nano)+`"`, 1)
		path := filepath.Join(project, "2026-10-01T10-00-00-000Z_"+id+".jsonl")
		write(t, path, content)
		at := now.Add(-r.written)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(project, "2026-10-01T10-00-00-000Z_"+busyID, "Subtask.jsonl"),
		`{"type":"session","version":3,"id":"sub","timestamp":"2026-10-01T10:00:00.000Z","cwd":"/work/beekeeper"}`+"\n")
	return dir
}

func TestDiscover(t *testing.T) {
	now := time.Now()
	// busy was created by the agent, idle half an hour ago.
	dir := sessionsDir(t, now, recorded{5*time.Minute - time.Second, 10 * time.Second}, recorded{30 * time.Minute, 20 * time.Minute})
	tab := procRoot(t, now,
		// beekeeper's agent: rpc mode behind its inbox, started 5 minutes ago.
		fakeProc{pid: 100, ppid: 1, args: []string{Comm, "--mode", "rpc", "--no-ui", "--model=local/qwen3-4b"}, cwd: repo, age: 5 * time.Minute,
			env: []string{EnvAgent + "=1234abcd-0000-4000-8000-000000000000", EnvName + "=omp worker"}},
		// a subagent it runs, and a helper: no sessions.
		fakeProc{pid: 101, ppid: 100, args: []string{Comm, "--mode", "rpc"}, cwd: repo, age: time.Minute},
		fakeProc{pid: 200, ppid: 1, args: []string{Comm, "models", "ls"}, cwd: repo, age: time.Minute},
		// the person's interactive omp, started a minute ago and not yet
		// prompted: idle's file was written before it started.
		fakeProc{pid: 300, ppid: 1, args: []string{"/usr/bin/omp", "--model", "opus"}, cwd: repo, age: time.Minute},
		// a Claude Code CLI: not omp's.
		fakeProc{pid: 400, ppid: 1, args: []string{"claude"}, cwd: repo, age: time.Minute},
	)
	got := map[int]*claude.Session{}
	for _, s := range Discover(dir, tab, now) {
		got[s.PID] = s
	}
	if len(got) != 2 {
		t.Fatalf("discovered %d sessions, want 2 (100, 300): %v", len(got), got)
	}
	a := got[100]
	if a == nil {
		t.Fatal("beekeeper's omp agent not discovered")
	}
	if a.Harness != Harness || a.ID != busyID || a.HostID != HostPrefix+"1234abcd-0000-4000-8000-000000000000" || a.Name != "omp worker" {
		t.Errorf("agent = harness %q id %q host %q name %q", a.Harness, a.ID, a.HostID, a.Name)
	}
	if a.State != StateBusy || a.Model != "local/qwen3-8b" || a.Cwd != repo {
		t.Errorf("agent state %q model %q cwd %q", a.State, a.Model, a.Cwd)
	}
	p := got[300]
	if p == nil {
		t.Fatal("the interactive omp not discovered")
	}
	if p.Transcript != "" || p.Name != "omp in beekeeper" || p.State != StateIdle || p.Model != "opus" {
		t.Errorf("unprompted session = transcript %q name %q state %q model %q", p.Transcript, p.Name, p.State, p.Model)
	}
	if !a.Party().Is(state.Party{HostSession: HostPrefix + "1234abcd-0000-4000-8000-000000000000", Name: "omp worker"}) {
		t.Error("the agent's party does not match its roster entry")
	}
}

func TestDiscoverIdle(t *testing.T) {
	now := time.Now()
	// The person resumed idle a minute ago and it replied since; busy ended
	// two hours ago.
	dir := sessionsDir(t, now, recorded{3 * time.Hour, 2 * time.Hour}, recorded{30 * time.Minute, 10 * time.Second})
	tab := procRoot(t, now, fakeProc{pid: 300, ppid: 1, args: []string{Comm, "--resume", idleID}, cwd: repo, age: time.Minute})
	ss := Discover(dir, tab, now)
	if len(ss) != 1 || ss[0].ID != idleID || ss[0].State != StateIdle || ss[0].Name != "Ping pong" {
		t.Fatalf("got %+v, want the idle session titled by its title change", ss)
	}
	ended := Ended(dir, ss, now.Add(-24*time.Hour))
	if len(ended) != 1 || ended[0].ID != busyID || ended[0].State != StateEnded || ended[0].Name != "Fix the flaky lease test" {
		t.Fatalf("ended = %+v, want the busy file, which no process runs", ended)
	}
}

func TestRunsSession(t *testing.T) {
	for args, want := range map[string]bool{
		"omp":                          true,
		"omp --model opus fix the bug": true,
		"omp acp":                      true,
		"omp --mode rpc --no-ui":       true,
		"omp models ls":                false,
		"omp ps":                       false,
		"omp --version":                false,
		"omp --export s.jsonl":         false,
		"node omp":                     false,
	} {
		f := strings.Fields(args)
		p := &proc.Process{Comm: filepath.Base(f[0]), Args: f}
		if got := runsSession(p); got != want {
			t.Errorf("runsSession(%q) = %v, want %v", args, got, want)
		}
	}
}

func TestTail(t *testing.T) {
	turns, err := Tail(filepath.Join("testdata", "busy.jsonl"), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 || turns[0].Role != "user" || turns[1].Text != "Reading the issue first." {
		t.Fatalf("turns = %+v, want the person's message and the reply's text, no thinking or tool output", turns)
	}
	if turns, _ := Tail(filepath.Join("testdata", "idle.jsonl"), 1); len(turns) != 1 || turns[0].Text != "pong" {
		t.Fatalf("last turn = %+v, want pong (a string content is read too)", turns)
	}
}

func TestFollow(t *testing.T) {
	turns, err := Follow(filepath.Join("testdata", "busy.jsonl"), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 3 || turns[2].Role != claude.RoleTool || turns[2].Text != "bash: gh issue view 999 --repo giantswarm/beekeeper" {
		t.Fatalf("turns = %+v, want the reply's text, then its bash call", turns)
	}
}

func TestReadTranscript(t *testing.T) {
	at, _ := time.Parse(time.RFC3339, "2026-10-01T10:30:00Z")
	w, a := ReadTranscript(filepath.Join("testdata", "busy.jsonl"), at)
	if len(w.Refs) == 0 || w.Refs[0] != "giantswarm/beekeeper#999" {
		t.Errorf("refs = %v, want giantswarm/beekeeper#999", w.Refs)
	}
	c := a.LastHour
	if c.Turns != 1 || c.ToolCalls != 1 || c.ToolErrors != 1 || c.GitHubCalls != 1 {
		t.Errorf("last hour = %+v, want 1 turn, 1 tool call that failed, 1 GitHub call", c)
	}
	if c.Tokens.Input != 1000 || c.Tokens.CacheRead != 200 || c.Tokens.Output != 50 || a.Context != 1200 {
		t.Errorf("tokens %+v context %d", c.Tokens, a.Context)
	}
	if c.CostUSD == nil || *c.CostUSD != 0.0015 {
		t.Errorf("cost = %v, want omp's 0.0015", c.CostUSD)
	}
	_, later := ReadTranscript(filepath.Join("testdata", "busy.jsonl"), at.Add(2*time.Hour))
	if later.LastHour.Turns != 0 || later.Total.Turns != 1 {
		t.Errorf("two hours later: last hour %+v total %+v", later.LastHour, later.Total)
	}
}

func TestShellArgv(t *testing.T) {
	argv := ShellArgv("/state/omp/x.in", []string{keyVar, otherKeyVar}, "omp", "--mode", "rpc")
	if argv[0] != "/bin/sh" || argv[4] != "/state/omp/x.in" || argv[5] != "A_KEY B_KEY" || strings.Join(argv[6:], " ") != "omp --mode rpc" {
		t.Fatalf("argv = %q", argv)
	}
}
