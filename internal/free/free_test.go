package free

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/proc"
)

var now = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

const (
	deadSID = "11111111-dead-4000-8000-000000000000"
	liveSID = "22222222-live-4000-8000-000000000000"
)

// fixture writes a file of kib KiB under root/rel, touched age ago.
func fixture(t *testing.T, root, rel string, kib int, age time.Duration) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, kib*1024), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
		t.Fatal(err)
	}
}

func newRun(t *testing.T, o Options) (*Run, *bytes.Buffer) {
	t.Helper()
	tmp := t.TempDir()
	out := &bytes.Buffer{}
	r := &Run{
		Options: o,
		Machine: Machine{
			UID:      os.Getuid(),
			TmpDir:   tmp,
			Scratch:  filepath.Join(tmp, "claude-1000"),
			Projects: filepath.Join(tmp, "projects"),
			Table:    &proc.Table{ByPID: map[int]*proc.Process{}},
			Sessions: []*claude.Session{{PID: 4242, ID: liveSID, Name: "Agent one", Cwd: "/work"}},
			Titles:   func() map[string]string { return map[string]string{deadSID: "Finished work"} },
			Now:      now,
		},
		Out:      out,
		anonKiB:  func(int) int { return 0 },
		uid:      func(int) int { return os.Getuid() },
		cgroup:   func(int) string { return "" },
		cwd:      func(int) string { return "" },
		mem:      func() (machine.Mem, error) { return machine.Mem{AvailableMiB: 40000, SwapUsedMiB: 100}, nil },
		scope:    func() string { return "" },
		unitPIDs: func(string) []int { return nil },
		kill:     func([]int) { t.Fatal("kill in a test") },
	}
	if o.Stale == 0 {
		r.Stale = 3 * time.Hour
	}
	return r, out
}

func sessionDir(sid string) string { return filepath.Join("claude-1000", "-home-u-proj", sid) }

func TestSessionDirsFreeOnlyTheDead(t *testing.T) {
	r, out := newRun(t, Options{Apply: true, Only: []string{SessionDirs}})
	dead, live := sessionDir(deadSID), sessionDir(liveSID)
	fresh := sessionDir("33333333-dead-4000-8000-000000000000")
	small := sessionDir("44444444-dead-4000-8000-000000000000")
	fixture(t, r.TmpDir, filepath.Join(dead, "scratchpad", "a"), 2048, 5*time.Hour)
	fixture(t, r.TmpDir, filepath.Join(live, "scratchpad", "a"), 2048, 5*time.Hour)
	fixture(t, r.TmpDir, filepath.Join(fresh, "tasks", "a"), 2048, time.Hour)
	fixture(t, r.TmpDir, filepath.Join(small, "scratchpad", "a"), 10, 5*time.Hour)
	r.Do()
	for dir, want := range map[string]bool{dead: false, live: true, fresh: true, small: true} {
		if _, err := os.Stat(filepath.Join(r.TmpDir, dir)); (err == nil) != want {
			t.Errorf("%s exists = %v, want %v\n%s", dir, err == nil, want, out)
		}
	}
	for _, s := range []string{
		`remove 2 MiB  "Finished work" (` + deadSID + `)  (CLI gone, idle 5 h)`,
		`keep 2 MiB  "Agent one" (` + liveSID + `): CLI alive, pid 4242, idle 5 h`,
		"freed by this run: 2 MiB",
	} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("report lacks %q:\n%s", s, out)
		}
	}
}

func TestSessionDirsTranscriptAndCwdKeep(t *testing.T) {
	r, out := newRun(t, Options{Summary: true, Only: []string{SessionDirs}})
	written := sessionDir("55555555-dead-4000-8000-000000000000")
	inside := sessionDir(deadSID)
	fixture(t, r.TmpDir, filepath.Join(written, "scratchpad", "a"), 2048, 5*time.Hour)
	fixture(t, r.TmpDir, filepath.Join("projects", "-home-u-proj", "55555555-dead-4000-8000-000000000000.jsonl"), 1, time.Minute)
	fixture(t, r.TmpDir, filepath.Join(inside, "scratchpad", "a"), 2048, 5*time.Hour)
	r.Table.ByPID[7] = &proc.Process{PID: 7}
	r.cwd = func(int) string { return filepath.Join(r.TmpDir, inside, "scratchpad") }
	r.Do()
	if out.Len() != 0 {
		t.Errorf("a recent transcript or a process inside must keep the dir; summary:\n%s", out)
	}
}

func TestSummaryRows(t *testing.T) {
	const node, tumblerd = "node", "tumblerd"
	r, out := newRun(t, Options{Summary: true, HeavyMiB: 500, RunawayCPU: 50, Runaway: 10 * time.Minute, TabMiB: 250, Orphan: 30 * time.Minute})
	r.Clusters = []machine.Cluster{{Name: "agentlab", MemMiB: 9000, RunningFor: "2 hours ago"}}
	r.Sessions[0].LastActive = now.Add(-4 * time.Hour)
	r.Sessions[0].MemMiB = 300
	r.Home = "/home/u"
	r.Sessions[0].Cwd = "/home/u/work"
	start := now.Add(-time.Hour)
	anon := map[int]int{10: 600 << 10, 11: 10 << 10, 12: 800 << 10, 20: 600 << 10, 21: 100 << 10, 30: 900 << 10, 40: 700 << 10, 50: 50 << 10}
	r.anonKiB = func(pid int) int { return anon[pid] }
	r.uid = func(pid int) int {
		if pid == 40 {
			return os.Getuid() + 1 // another user's, whoever runs the test
		}
		return os.Getuid()
	}
	unit := "/user.slice/user-1000.slice/user@" + strconv.Itoa(os.Getuid()) + ".service/app.slice/tumblerd.service"
	r.cgroup = func(pid int) string {
		if pid == 20 || pid == 21 {
			return unit
		}
		return "/user.slice/x.scope"
	}
	r.unitPIDs = func(cg string) []int {
		if cg == unit {
			return []int{20, 21}
		}
		return nil
	}
	for _, p := range []*proc.Process{
		{PID: 10, PPID: 2, Comm: node, Args: []string{node, "big\tjob"}, Start: start, RSSKiB: 700 << 10},
		{PID: 11, PPID: 2, Comm: "graphify", Args: []string{"python3", "graphify"}, Start: start, CPU: 40 * time.Minute, RSSKiB: 20 << 10},
		{PID: 12, PPID: 2, Comm: "go", Args: []string{"go", "test"}, Start: start, CPU: 59 * time.Minute, RSSKiB: 900 << 10},
		{PID: 20, PPID: 2, Comm: tumblerd, Args: []string{tumblerd}, Start: start, RSSKiB: 600 << 10},
		{PID: 21, PPID: 20, Comm: tumblerd, Args: []string{tumblerd}, Start: start, RSSKiB: 600 << 10},
		{PID: 30, PPID: 2, Comm: "chrome", Args: []string{"/opt/google/chrome/chrome --type=renderer --lang=en"}, Start: start, RSSKiB: 1000 << 10},
		{PID: 40, PPID: 2, Comm: "dockerd", Args: []string{"dockerd"}, Start: start, RSSKiB: 800 << 10},
		{PID: 50, PPID: 1, Comm: node, Args: []string{node, "processChild.js"}, Start: start, RSSKiB: 60 << 10},
	} {
		r.Table.ByPID[p.PID] = p
	}
	r.mem = func() (machine.Mem, error) { return machine.Mem{AvailableMiB: 40000, SwapUsedMiB: 7000}, nil }
	r.Do()
	want := strings.Join([]string{
		"kind\tagentlab\t9000\t2 hours ago",
		"cli\t4242\t300\t240\t~/work",
		"proc\t12\t800\t01:00\tgo\tgo test\theavy,runaway:98%",
		"proc\t20\t700\t01:00\ttumblerd\tunit:tumblerd.service\theavy",
		"proc\t10\t600\t01:00\tnode\tnode big job\theavy",
		"proc\t11\t10\t01:00\tgraphify\tpython3 graphify\trunaway:66%",
		"tab\t30\t900\t01:00\trenderer",
		"section\torphans\t50\t1\torphaned jest / Claude workers",
		"section\tswap\t7000\t1\tcold pages in swap (reset needs root)",
	}, "\n") + "\n"
	if out.String() != want {
		t.Errorf("summary:\n%s\nwant:\n%s", out, want)
	}
}

func TestSwapIsOnlyPrinted(t *testing.T) {
	r, out := newRun(t, Options{Apply: true, Only: []string{Swap}})
	r.mem = func() (machine.Mem, error) { return machine.Mem{AvailableMiB: 40000, SwapUsedMiB: 7000}, nil }
	r.Do()
	if !strings.Contains(out.String(), "== needs root, run yourself ==\n  "+SwapResetCmd) {
		t.Errorf("the swap reset is not printed as a command:\n%s", out)
	}
	r, out = newRun(t, Options{Summary: true, Only: []string{Swap}})
	r.mem = func() (machine.Mem, error) { return machine.Mem{AvailableMiB: 5000, SwapUsedMiB: 7000}, nil }
	r.Do()
	if out.Len() != 0 {
		t.Errorf("a reset without room in RAM is offered:\n%s", out)
	}
}

func TestTmpDirs(t *testing.T) {
	r, out := newRun(t, Options{Summary: true, Only: []string{TmpDirs}})
	fixture(t, r.TmpDir, "go-build123/a", 3072, 4*time.Hour)
	fixture(t, r.TmpDir, "tmp.fresh/a", 3072, time.Hour)
	fixture(t, r.TmpDir, "unrelated/a", 3072, 4*time.Hour)
	if err := os.MkdirAll(filepath.Join(r.TmpDir, "tmp.empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	r.Do()
	if want := "section\ttmp-dirs\t3\t1\tthrowaway temp dirs in /tmp\n"; out.String() != want {
		t.Errorf("summary %q, want %q", out, want)
	}
}

func TestParseOnly(t *testing.T) {
	if _, err := ParseOnly([]string{"swap", "session-dirs"}); err != nil {
		t.Error(err)
	}
	if _, err := ParseOnly([]string{"sessions"}); err == nil {
		t.Error("an unknown section passed")
	}
}

func TestUptime(t *testing.T) {
	for d, want := range map[time.Duration]string{
		90 * time.Minute:                  "01:30",
		26*time.Hour + 5*time.Minute:      "1d 02:05",
		3*24*time.Hour + 59*time.Second:   "3d 00:00",
		time.Duration(0):                  "00:00",
		23*time.Hour + 59*time.Minute + 1: "23:59",
	} {
		if got := uptime(d); got != want {
			t.Errorf("uptime(%v) = %q, want %q", d, got, want)
		}
	}
}
