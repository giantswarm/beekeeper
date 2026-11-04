package free

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

var now = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

const freeFixtureCwd = "cwd"

// The build CPU line: the slice's reading, its throttling, a slice without
// a quota, and the configured budget while the slice does not run.
func TestBuildCPULine(t *testing.T) {
	const quota = "1200%"
	configured := platform.Cap{CPUQuota: quota, CPUWeight: 50}
	for name, tc := range map[string]struct {
		b    *machine.BuildCPU
		want string
	}{
		"in use":      {&machine.BuildCPU{Quota: quota, Weight: 50, UsePct: 1180.4}, "build CPU: 1180% of 1200% in use (memcap.slice CPUQuota, weight 50; 100% is one core)"},
		"throttled":   {&machine.BuildCPU{Quota: quota, Weight: 50, UsePct: 1199, Throttled: 2}, "build CPU: 1199% of 1200% in use (memcap.slice CPUQuota, weight 50; 100% is one core), throttled in 2 periods of the last 500ms"},
		"no quota":    {&machine.BuildCPU{Quota: "max", Weight: 100, UsePct: 340}, "build CPU: 340% of max in use (memcap.slice CPUQuota, weight 100; 100% is one core); NO QUOTA: the next beekeeper run sets 1200% (memcap.cpuQuota), weight 50"},
		"not running": {nil, "build CPU: memcap.slice not running; its budget 1200% (memcap.cpuQuota), weight 50"},
	} {
		if got := BuildCPULine(tc.b, configured); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, tc.want)
		}
	}
	if got := BuildCPULine(nil, platform.Cap{CPUWeight: 50}); got != "build CPU: memcap.slice not running; its budget none (memcap.cpuQuota), weight 50" {
		t.Errorf("without a quota configured: %q", got)
	}
}

const (
	deadSID = "11111111-dead-4000-8000-000000000000"
	liveSID = "22222222-a11e-4000-8000-000000000000"
	// workDir is the live sessions' working folder.
	workDir = "/work"
)

// fixture writes a file of kib KiB under root/rel, its tree touched age ago.
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
	for d := filepath.Dir(p); ; d = filepath.Dir(d) {
		if err := os.Chtimes(d, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
		if d == root {
			break
		}
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
			Sessions: []*claude.Session{{PID: 4242, ID: liveSID, Name: "Agent one", Cwd: workDir}},
			Desk:     &state.State{},
			Records: func() map[string]*claude.Record {
				return map[string]*claude.Record{deadSID: {SessionID: "local_dead", Title: "Finished work"}}
			},
			Now: now,
		},
		Out:      out,
		anonKiB:  func(int) int { return 0 },
		uid:      func(int) int { return os.Getuid() },
		cgroup:   func(int) string { return "" },
		holds:    func(int) []string { return nil },
		mem:      func() (machine.Mem, error) { return machine.Mem{AvailableMiB: 40000, SwapUsedMiB: 100}, nil },
		scope:    func() (*machine.Scope, error) { return nil, nil },
		unitPIDs: func(string) []int { return nil },
		oomd:     func() (machine.OOMDSwap, error) { return machine.OOMDSwap{LimitPercent: 90}, nil },
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
	// A small dead dir gets no line of its own, yet goes: the total counts it.
	for dir, want := range map[string]bool{dead: false, live: true, fresh: true, small: false} {
		if _, err := os.Stat(filepath.Join(r.TmpDir, dir)); (err == nil) != want {
			t.Errorf("%s exists = %v, want %v\n%s", dir, err == nil, want, out)
		}
	}
	for _, s := range []string{
		`remove 2 MiB  "Finished work" (` + deadSID + `)  (CLI gone, idle 5 h)`,
		`keep 2 MiB  "Agent one" (` + liveSID + `): CLI alive, pid 4242, idle 5 h`,
		"freed 2 dead sessions' dirs, 2 MiB",
		"freed by this run: 2 MiB",
	} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("report lacks %q:\n%s", s, out)
		}
	}
	if strings.Contains(out.String(), "44444444") {
		t.Errorf("a small dir has a line of its own:\n%s", out)
	}
}

func TestSessionDirsStayInsideScratch(t *testing.T) {
	for _, level := range []string{"project", "session", "internal-project", "internal-session"} {
		t.Run(level, func(t *testing.T) {
			r, _ := newRun(t, Options{Apply: true, Only: []string{SessionDirs}})
			external := t.TempDir()
			fixture(t, external, filepath.Join(deadSID, "scratchpad", "keep"), 1, 5*time.Hour)
			project := filepath.Join(r.Scratch, "-outside")
			link, target := project, external
			if strings.HasPrefix(level, "internal-") {
				target = filepath.Join(r.Scratch, "target")
				fixture(t, target, filepath.Join("source", deadSID, "scratchpad", "keep"), 1, 5*time.Hour)
				target = filepath.Join(target, "source")
			}
			keep := filepath.Join(target, deadSID, "scratchpad", "keep")
			if strings.HasSuffix(level, "session") {
				link, target = filepath.Join(project, deadSID), filepath.Join(target, deadSID)
			}
			if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			if got := r.Free(); len(got) != 0 {
				t.Fatalf("symlink counted as a session: %v", got)
			}
			if _, err := os.Stat(keep); err != nil {
				t.Fatalf("target file lost: %v", err)
			}
			if _, err := os.Lstat(link); err != nil {
				t.Fatalf("symlink removed: %v", err)
			}
		})
	}
}

// Replace a checked project after scanning, before deletion. A pathname-only
// containment check would still let RemoveAll follow the replacement alias.
func TestSessionDirsProjectReplacedBeforeRemoval(t *testing.T) {
	r, _ := newRun(t, Options{Apply: true, Only: []string{SessionDirs}})
	fixture(t, r.TmpDir, filepath.Join(sessionDir(deadSID), "scratchpad", "a"), 1, 5*time.Hour)
	external := t.TempDir()
	keep := filepath.Join(deadSID, "scratchpad", "keep")
	fixture(t, external, keep, 1, 5*time.Hour)
	r.Records = func() map[string]*claude.Record {
		project := filepath.Dir(filepath.Join(r.TmpDir, sessionDir(deadSID)))
		if err := os.Rename(project, project+"-moved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, project); err != nil {
			t.Fatal(err)
		}
		return map[string]*claude.Record{}
	}
	if got := r.Free(); len(got) != 0 {
		t.Fatalf("failed deletion counted: %v", got)
	}
	if _, err := os.Stat(filepath.Join(external, keep)); err != nil {
		t.Fatalf("external file lost: %v", err)
	}
}

func TestSessionDirsScratchAlias(t *testing.T) {
	r, _ := newRun(t, Options{Apply: true, Only: []string{SessionDirs}})
	fixture(t, r.TmpDir, filepath.Join(sessionDir(deadSID), "scratchpad", "a"), 1, 5*time.Hour)
	original := r.Scratch
	r.Scratch = filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(original, r.Scratch); err != nil {
		t.Fatal(err)
	}
	if got := r.Free(); len(got) != 1 || got[0].N != 1 {
		t.Fatalf("normal cleanup through alias: %v", got)
	}
	if _, err := os.Stat(filepath.Join(r.TmpDir, sessionDir(deadSID))); !os.IsNotExist(err) {
		t.Fatalf("session remains: %v", err)
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
	r.holds = func(int) []string { return []string{filepath.Join(r.TmpDir, inside, "scratchpad")} }
	r.Do()
	if out.Len() != 0 {
		t.Errorf("a recent transcript or a process inside must keep the dir; summary:\n%s", out)
	}
}

// transcript writes the transcript of the session sid in the fixtures'
// project, recording cwd as its working folder, untouched for five hours.
func transcript(t *testing.T, r *Run, sid, cwd string) {
	t.Helper()
	entry, err := json.Marshal(map[string]string{freeFixtureCwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(r.Projects, "-home-u-proj", sid+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, append([]byte(`{"type":"queue-operation"}`+"\n"), append(entry, '\n')...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, now.Add(-5*time.Hour), now.Add(-5*time.Hour)); err != nil {
		t.Fatal(err)
	}
}

// What --apply keeps and what it removes: a session's dir while the session
// lives on or a process holds a file inside, a go-build dir while its go
// command may run, a file inside is held or it changed within the hour.
func TestFreeKeepsTheLiveAndRemovesTheDead(t *testing.T) {
	const newerSID, goDir = "66666666-a11e-4000-8000-000000000000", "go-build123"
	dead := sessionDir(deadSID)
	desk := func(st *state.State) func(*Run) { return func(r *Run) { r.Desk = st } }
	goCommand := func(began time.Duration) func(*Run) {
		return func(r *Run) {
			r.Table.ByPID[30] = &proc.Process{PID: 30, Comm: "go", Args: []string{"go", "test"}, Start: now.Add(-began)}
		}
	}
	holding := func(dir string) func(*Run) {
		return func(r *Run) {
			r.Table.ByPID[7] = &proc.Process{PID: 7, Comm: "tail"}
			r.holds = func(pid int) []string {
				if pid != 7 {
					return nil
				}
				return []string{workDir, filepath.Join(r.TmpDir, dir, "out.log")}
			}
		}
	}
	worktree := func(remains bool) func(*Run) {
		return func(r *Run) {
			wt := filepath.Join(r.TmpDir, "repo-wt")
			transcript(t, r, deadSID, wt)
			if !remains {
				return
			}
			if err := os.MkdirAll(wt, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /repo/.git/worktrees/repo-wt\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	restarted := func(r *Run) {
		r.Sessions = append(r.Sessions, &claude.Session{PID: 4343, ID: newerSID, HostID: "local_dead"})
	}
	parked := state.Agent{Party: state.Party{Session: deadSID, Name: "Parked worker"}, Task: "serves #1", Park: &state.Park{}}
	finished := parked
	finished.Done = true
	for name, tc := range map[string]struct {
		section string
		dir     string
		age     time.Duration
		setup   func(*Run)
		kept    bool
	}{
		"a live session's dir":                           {SessionDirs, sessionDir(liveSID), 5 * time.Hour, nil, true},
		"a dead session's dir":                           {SessionDirs, dead, 5 * time.Hour, nil, false},
		"a dead session's dir with a file open":          {SessionDirs, dead, 5 * time.Hour, holding(dead), true},
		"a dead session's dir in its worktree":           {SessionDirs, dead, 5 * time.Hour, worktree(true), true},
		"a dead session's dir, its worktree gone":        {SessionDirs, dead, 5 * time.Hour, worktree(false), false},
		"a parked agent's dir":                           {SessionDirs, dead, 5 * time.Hour, desk(&state.State{Agents: []state.Agent{parked}}), true},
		"a finished agent's dir":                         {SessionDirs, dead, 5 * time.Hour, desk(&state.State{Agents: []state.Agent{finished}}), false},
		"every dir while the desk cannot be read":        {SessionDirs, dead, 5 * time.Hour, desk(nil), true},
		"the guide's dir":                                {SessionDirs, dead, 5 * time.Hour, desk(&state.State{Guide: &state.Role{Holder: &state.Supervisor{Party: parked.Party}}}), true},
		"a dir of a desktop session that runs anew":      {SessionDirs, dead, 5 * time.Hour, restarted, true},
		"a go-build dir whose go command runs":           {GoBuild, goDir, 2 * time.Hour, goCommand(3 * time.Hour), true},
		"a go-build dir whose go command is gone":        {GoBuild, goDir, 2 * time.Hour, nil, false},
		"a go-build dir a later go command cannot own":   {GoBuild, goDir, 2 * time.Hour, goCommand(30 * time.Minute), false},
		"a go-build dir with a file open":                {GoBuild, goDir, 2 * time.Hour, holding(goDir), true},
		"a go-build dir changed within the hour":         {GoBuild, goDir, 30 * time.Minute, nil, true},
		"a session's dir is no go-build dir to the rule": {GoBuild, dead, 5 * time.Hour, nil, true},
	} {
		t.Run(name, func(t *testing.T) {
			r, out := newRun(t, Options{Apply: true, Only: []string{tc.section}})
			fixture(t, r.TmpDir, filepath.Join(tc.dir, "scratchpad", "a"), 2048, tc.age)
			if tc.setup != nil {
				tc.setup(r)
			}
			r.Do()
			if _, err := os.Stat(filepath.Join(r.TmpDir, tc.dir)); (err == nil) != tc.kept {
				t.Errorf("kept = %v, want %v\n%s", err == nil, tc.kept, out)
			}
		})
	}
}

// Each section of dirs ends with its count and size, a dry run's as what it
// would free; Free returns them for the watch, without the report around the
// sections, and removes what it counts.
func TestFreeCountsEachClass(t *testing.T) {
	setup := func(r *Run) {
		fixture(t, r.TmpDir, filepath.Join(sessionDir(deadSID), "scratchpad", "a"), 2048, 5*time.Hour)
		fixture(t, r.TmpDir, filepath.Join(sessionDir("44444444-dead-4000-8000-000000000000"), "tasks", "a"), 10, 5*time.Hour)
		fixture(t, r.TmpDir, "go-build1/b001/a", 3072, 2*time.Hour)
	}
	r, out := newRun(t, Options{Only: Automatic})
	setup(r)
	r.Do()
	for _, s := range []string{"would free 2 dead sessions' dirs, 2 MiB", "would free 1 orphaned go-build dir, 3 MiB", "dry run; add --apply to act"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("report lacks %q:\n%s", s, out)
		}
	}
	if _, err := os.Stat(filepath.Join(r.TmpDir, "go-build1")); err != nil {
		t.Errorf("a dry run removed: %v", err)
	}
	r, out = newRun(t, Options{Apply: true, Only: Automatic})
	setup(r)
	got := r.Free()
	want := []Freed{{Section: SessionDirs, N: 2, KiB: 2058}, {Section: GoBuild, N: 1, KiB: 3072}}
	if !slices.Equal(got, want) {
		t.Errorf("Free = %+v, want %+v", got, want)
	}
	if strings.Contains(out.String(), "== before ==") || strings.Contains(out.String(), "kind clusters") {
		t.Errorf("Free printed the report around the sections:\n%s", out)
	}
	if left, _ := filepath.Glob(filepath.Join(r.TmpDir, "*", "*", "*")); len(left) != 0 {
		t.Errorf("Free left %v", left)
	}
	if s := got[0].String() + "; " + got[1].String(); s != "2 dead sessions' dirs, 2 MiB; 1 orphaned go-build dir, 3 MiB" {
		t.Errorf("said %q", s)
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
		"cli\t4242\t300\t240\t~/work\t" + liveSID + "\tAgent one\t-\t-\t4\t-",
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
	fixture(t, r.TmpDir, "tmp.stale/a", 3072, 4*time.Hour)
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

// Without a process table and memory reads, every section that needs them
// says so in one line and frees nothing: no dir can be told unused.
func TestMissingPartsFreeNothing(t *testing.T) {
	stub := platform.Stub().Machine
	r, out := newRun(t, Options{Apply: true, HeavyMiB: 1, TabMiB: 1})
	_, r.TableErr = stub.Processes()
	r.mem, r.scope = stub.Mem, stub.DesktopScope
	r.Sessions = nil
	dead, stale := sessionDir(deadSID), "go-build123"
	fixture(t, r.TmpDir, filepath.Join(dead, "scratchpad", "a"), 2048, 5*time.Hour)
	fixture(t, r.TmpDir, filepath.Join(stale, "a"), 2048, 5*time.Hour)
	r.Do()
	for _, dir := range []string{dead, stale} {
		if _, err := os.Stat(filepath.Join(r.TmpDir, dir)); err != nil {
			t.Errorf("%s removed without a process table:\n%s", dir, out)
		}
	}
	for line, want := range map[string]int{
		platform.Unavailable("processes"):     7, // CLIs, heavy, tabs, session dirs, go-build, temp dirs, orphans
		platform.Unavailable("memory"):        3, // before, swap, after
		platform.Unavailable("desktop scope"): 2, // before, after
	} {
		if n := strings.Count(out.String(), line); n != want {
			t.Errorf("%q said %d times, want %d:\n%s", line, n, want, out)
		}
	}
	if strings.Contains(out.String(), "remove ") || strings.Contains(out.String(), "RAM available") {
		t.Errorf("free acted on or reported what it cannot read:\n%s", out)
	}
}

func TestStateSplitsSwapAndNamesOomdRule(t *testing.T) {
	mem := machine.Mem{TotalMiB: 88064, AvailableMiB: 40000, SwapTotalMiB: 16383, SwapUsedMiB: 16000, ZswappedMiB: 13000, ZswapPoolMiB: 4000}
	r, out := newRun(t, Options{Only: []string{Swap}})
	r.mem = func() (machine.Mem, error) { return mem, nil }
	r.Do()
	if !strings.Contains(out.String(), "  swap: disk 3000 MiB + zswap 13000 MiB in a 4000 MiB pool\n") {
		t.Errorf("swap is not split into disk and zswap:\n%s", out)
	}
	if strings.Contains(out.String(), "trigger") || !strings.Contains(out.String(), "systemd-oomd watches no cgroup for swap") {
		t.Errorf("an oomd trigger without a swap-monitored cgroup:\n%s", out)
	}
	r, out = newRun(t, Options{Only: []string{Swap}})
	r.mem = func() (machine.Mem, error) { return mem, nil }
	r.oomd = func() (machine.OOMDSwap, error) {
		return machine.OOMDSwap{LimitPercent: 90, Monitored: []string{"/user.slice"}}, nil
	}
	r.Do()
	if !strings.Contains(out.String(), "-1256 MiB before systemd-oomd's 90 % swap trigger") {
		t.Errorf("the oomd trigger of a swap-monitored cgroup is not named:\n%s", out)
	}
}

// Process-held paths use the resolved spelling, including deleted open files
// whose former path no longer exists. No live processes are read here.
func TestFreeKeepsHeldDirsThroughTempAlias(t *testing.T) {
	for _, section := range []string{SessionDirs, GoBuild, TmpDirs} {
		for _, heldKind := range []string{freeFixtureCwd, "exe", "open", "deleted"} {
			t.Run(section+"/"+heldKind, func(t *testing.T) {
				r, out := newRun(t, Options{Apply: true, Only: []string{section}})
				realRoot, err := filepath.EvalSymlinks(r.TmpDir)
				if err != nil {
					t.Fatal(err)
				}
				alias := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(realRoot, alias); err != nil {
					t.Fatal(err)
				}
				r.TmpDir = alias
				r.Scratch = filepath.Join(alias, "claude-1000")
				held, unused := "tmp.held", "tmp.unused"
				switch section {
				case SessionDirs:
					held, unused = sessionDir(deadSID), sessionDir("33333333-dead-4000-8000-000000000000")
				case GoBuild:
					held, unused = "go-build-held", "go-build-unused"
				}
				for _, dir := range []string{held, unused} {
					fixture(t, realRoot, filepath.Join(dir, "scratchpad", "a"), 2048, 5*time.Hour)
				}
				heldPath := filepath.Join(realRoot, held, "scratchpad", "a")
				switch heldKind {
				case freeFixtureCwd:
					heldPath = filepath.Dir(heldPath)
				case "deleted":
					// proc.Held strips the kernel's " (deleted)" suffix.
					heldPath = filepath.Join(realRoot, held, "scratchpad", "removed")
					if _, err := os.Stat(heldPath); !os.IsNotExist(err) {
						t.Fatalf("deleted fixture exists: %v", err)
					}
				}
				r.Table = proc.NewTable(&proc.Process{PID: 7, Comm: "worker"})
				r.holds = func(int) []string { return []string{heldPath} }
				freed := r.Free()
				if _, err := os.Stat(filepath.Join(realRoot, held, "scratchpad", "a")); err != nil {
					t.Errorf("held candidate removed: %v\n%s", err, out)
				}
				if _, err := os.Stat(filepath.Join(realRoot, unused)); !os.IsNotExist(err) {
					t.Errorf("unused candidate remains: %v\n%s", err, out)
				}
				if len(freed) != 1 || freed[0].Section != section || freed[0].N != 1 {
					t.Errorf("freed = %+v, want only unused candidate", freed)
				}
			})
		}
	}
}

func TestFreeKeepsHeldDirThroughScratchAlias(t *testing.T) {
	r, out := newRun(t, Options{Apply: true, Only: []string{SessionDirs}})
	scratch, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(scratch, r.Scratch); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join("-home-u-proj", deadSID)
	fixture(t, scratch, filepath.Join(dir, "scratchpad", "a"), 2048, 5*time.Hour)
	r.Table = proc.NewTable(&proc.Process{PID: 7})
	r.holds = func(int) []string { return []string{filepath.Join(scratch, dir, "removed")} }
	if freed := r.Free(); len(freed) != 0 {
		t.Errorf("freed held scratch: %+v\n%s", freed, out)
	}
	if _, err := os.Stat(filepath.Join(scratch, dir, "scratchpad", "a")); err != nil {
		t.Fatal(err)
	}
}
