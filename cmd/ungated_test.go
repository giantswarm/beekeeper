package cmd

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// ungatedTable is a machine's process table: a worker's CLI (pid 10) whose
// script runs a merge (12) and whose python code runs another (44), a
// promotion in the CLI's own shell (40), a gate's merge-child running a
// merge (21), the devctl commands the gate does not run: the gate's own
// dry run (31), a --team promotion (41) and a pr wait (42), and a test run's
// fake devctl merges: under its cmd.test merge-child (51), under a
// watch.test (53), and reparented scripts their shell runs from a test's
// temporary directory: under /tmp (54), under a TMPDIR of the test's own (55)
// and a benchmark's (56).
func ungatedTable() *proc.Table {
	p := func(pid, ppid int, comm string, args ...string) *proc.Process {
		return &proc.Process{PID: pid, PPID: ppid, Comm: comm, Args: args, Start: time.Now().Add(-time.Hour), StartTicks: int64(pid) * 100}
	}
	tb := &proc.Table{ByPID: map[int]*proc.Process{}}
	for _, pr := range []*proc.Process{
		p(1, 0, "systemd", "/sbin/init"),
		p(10, 1, claudeComm, claudeComm),
		p(11, 10, "bash", "bash", "/home/u/scratch/merge.sh"),
		p(12, 11, merge.Tool, "devctl", "pr", "merge", "o/r", "1"),
		p(20, 1, project.Name, "/home/u/.go/bin/beekeeper", MergeChildCmd, "/home/u/.local/state/beekeeper/merges/o-r-2"),
		p(21, 20, merge.Tool, "/home/u/.go/bin/devctl", "pr", "merge", "--timeout", "30m", "o/r", "2"),
		p(30, 10, project.Name, "/home/u/.go/bin/beekeeper", GateCmdName, "--", "devctl", "release", "promote", "o/r"),
		p(31, 30, merge.Tool, "devctl", "release", "promote", "o/r", "--dry-run"),
		p(40, 10, merge.Tool, "devctl", "release", "promote", "o/r"),
		p(41, 10, merge.Tool, "devctl", "release", "promote", "--team", "x"),
		p(42, 10, merge.Tool, "devctl", "pr", "wait", "o/r", "3"),
		p(43, 10, "python3", "python3", "-c", "import subprocess; subprocess.run(['devctl', 'pr', 'merge', 'o/r', '4'])"),
		p(44, 43, merge.Tool, "devctl", "pr", "merge", "o/r", "4"),
		p(50, 10, "cmd.test", "/tmp/go-build1/b001/cmd.test", "-test.run=TestHelperMergeChild", "--", "/tmp/TestGate1/001/state"),
		p(51, 50, merge.Tool, "/tmp/TestGate1/001/devctl", "pr", "merge", "o/r", "5"),
		p(52, 10, "watch.test", "/tmp/go-build2/b002/watch.test"),
		p(53, 52, merge.Tool, "devctl", "pr", "merge", "o/r", "6"),
		p(54, 1, merge.Tool, "sh", "/tmp/TestGate2/001/devctl", "pr", "merge", "o/r", "7"),
		p(55, 1, merge.Tool, "sh", "/home/u/.cache/tmp/TestAGateAtItsToolLimit405443892/001/devctl", "pr", "merge", "o/r", "8"),
		p(56, 1, merge.Tool, "/var/tmp/BenchmarkGate7/002/devctl", "pr", "merge", "o/r", "9"),
	} {
		tb.ByPID[pr.PID] = pr
	}
	return tb
}

const ungatedWhat = "): runs with no beekeeper gate among its ancestors, outside the lanes, the holds and the budget floor, " +
	"with devctl's own bypass; a merge runs as its own Bash command line, which the hook gates"

// workerSession is the session id of the worker's CLI, pid 10.
const workerSession = "s10"

// ungatedWorker is the name of the worker's CLI, pid 10.
const ungatedWorker = "Worker A"

// The merges and the one-repository promotion with no gate or merge-child
// among their ancestors are ungated, by pid, each naming its session and its
// parent; a merge under a merge-child, the gate's own dry run, a --team
// promotion, a pr wait and a test run's fake devctl merges are none.
func TestUngatedMerges(t *testing.T) {
	sessions := []*claude.Session{{PID: 10, ID: workerSession, Name: ungatedWorker}}
	got := ungatedMerges(sessions, ungatedTable())
	want := []ungatedMerge{
		{PID: 12, Line: `devctl pr merge o/r 1 (pid 12, session "Worker A", under bash /home/u/scratch/merge.sh (pid 11)` + ungatedWhat},
		{PID: 40, Line: `devctl release promote o/r (pid 40, session "Worker A", under claude (pid 10)` + ungatedWhat},
		{PID: 44, Line: `devctl pr merge o/r 4 (pid 44, session "Worker A", under python3 -c (pid 43)` + ungatedWhat},
	}
	if len(got) != len(want) {
		t.Fatalf("ungated merges = %+v, want %d", got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ungated merge %d:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}
	if m := ungatedMerges(nil, ungatedTable()); len(m) != 3 || !strings.Contains(m[0].Line, "(pid 12, no session, under") {
		t.Errorf("outside every session: %+v", m)
	}
}

// The watch says each ungated merge once while it runs, naming its session
// and its parent, and its end once, as one ENDED line with the same label.
func TestTheWatchSaysAnUngatedMerge(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	w.now = time.Now()
	sessions := []*claude.Session{{PID: 10, ID: workerSession, Name: ungatedWorker}}
	tb := ungatedTable()
	w.ungated(sessions, tb)
	w.ungated(sessions, tb)
	s := out.String()
	for _, want := range []string{
		`UNGATED MERGE devctl pr merge o/r 1 (pid 12, session "Worker A", under bash /home/u/scratch/merge.sh (pid 11)` + ungatedWhat,
		`UNGATED MERGE devctl release promote o/r (pid 40, session "Worker A", under claude (pid 10)` + ungatedWhat,
		`UNGATED MERGE devctl pr merge o/r 4 (pid 44, session "Worker A", under python3 -c (pid 43)` + ungatedWhat,
	} {
		if strings.Count(s, want) != 1 {
			t.Errorf("want once: %q\n%s", want, s)
		}
	}
	for _, gated := range []string{"(pid 21,", "(pid 31,", "(pid 41,", "(pid 42,", "(pid 51,", "(pid 53,", "(pid 54,"} {
		if strings.Contains(s, gated) {
			t.Errorf("a gated or ungateable command is said: %s\n%s", gated, s)
		}
	}
	delete(tb.ByPID, 12)
	delete(tb.ByPID, 11)
	out.Reset()
	w.ungated(sessions, tb)
	if got := out.String(); strings.Count(got, "\n") != 1 ||
		!strings.Contains(got, `ENDED UNGATED MERGE devctl pr merge o/r 1 (pid 12, session "Worker A", under bash /home/u/scratch/merge.sh (pid 11)) (since `) {
		t.Errorf("the merge's end is not one ENDED line:\n%s", got)
	}
}

// ungatedMachine is a machine whose process table is given.
type ungatedMachine struct {
	platform.Machine
	table *proc.Table
}

func (m ungatedMachine) Processes() (*proc.Table, error) { return m.table, nil }

// The doctor reports each ungated merge with its session, found from the
// CLI records like the watch does.
func TestTheDoctorReportsAnUngatedMerge(t *testing.T) {
	a, _ := stubApp(t)
	writeFile(t, filepath.Join(a.cfg.Claude.SessionsDir, "10.json"),
		`{"pid":10,"sessionId":"s10","name":"Worker A","procStart":"1000","kind":"interactive"}`)
	plat.Machine = ungatedMachine{Machine: plat.Machine, table: ungatedTable()}
	rep, err := a.doctor(context.Background(), doctorRun{by: state.Party{Name: agentOne}, dryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.ungated) != 3 || !strings.HasPrefix(rep.ungated[0], `devctl pr merge o/r 1 (pid 12, session "Worker A", under bash /home/u/scratch/merge.sh (pid 11)): runs with no beekeeper gate`) {
		t.Fatalf("ungated = %q", rep.ungated)
	}
}

// A snapshot's waits mark the merges running outside the gate, a gated one
// and a wait not.
// A test binary and an executable in testing's TempDir layout are a test's,
// under any TMPDIR; an installed devctl or a directory merely named Test… is
// not.
func TestTestExecutable(t *testing.T) {
	for path, want := range map[string]bool{
		"/tmp/go-build1/b001/cmd.test":                    true,
		"/tmp/TestGate1/001/devctl":                       true,
		"/home/u/.cache/tmp/TestGate405443892/001/devctl": true,
		"/var/tmp/BenchmarkGate7/002/bin/devctl":          true,
		"/tmp/FuzzArgs99/001/devctl":                      true,
		"/home/u/.go/bin/devctl":                          false,
		merge.Tool:                                        false,
		"/home/u/TestRepos/devctl":                        false,
		"/home/u/Testing/v2/devctl":                       false,
		"/home/u/Test/001/devctl":                         false,
	} {
		if got := testExecutable(path); got != want {
			t.Errorf("testExecutable(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestFindWaitsMarksAnUngatedMerge(t *testing.T) {
	byPID := map[int]wait{}
	for _, w := range findWaits(ungatedTable(), nil, time.Now()) {
		byPID[w.PID] = w
	}
	for pid, want := range map[int]bool{12: true, 44: true, 21: false, 42: false, 51: false, 53: false, 54: false} {
		w, ok := byPID[pid]
		if !ok || w.Ungated != want {
			t.Errorf("wait %d: listed %v, ungated %v, want %v", pid, ok, w.Ungated, want)
		}
	}
}

// runnerTable is a worker's CLI (pid 10) whose merges a runner started from
// its arguments: xargs (61), find -exec (63) and parallel through its shell
// (66), and one xargs started with the gate written in (69, under its gate's
// merge-child).
func runnerTable() *proc.Table {
	p := func(pid, ppid int, comm string, args ...string) *proc.Process {
		return &proc.Process{PID: pid, PPID: ppid, Comm: comm, Args: args, Start: time.Now().Add(-time.Hour), StartTicks: int64(pid) * 100}
	}
	tb := &proc.Table{ByPID: map[int]*proc.Process{}}
	for _, pr := range []*proc.Process{
		p(1, 0, "systemd", "/sbin/init"),
		p(10, 1, claudeComm, claudeComm),
		p(60, 10, "xargs", "xargs", "-n1", "devctl", "pr", "merge", "o/r"),
		p(61, 60, merge.Tool, "devctl", "pr", "merge", "o/r", "11"),
		p(62, 10, "find", "find", ".", "-name", "*.pr", "-exec", "devctl", "pr", "merge", "o/r", "{}", ";"),
		p(63, 62, merge.Tool, "devctl", "pr", "merge", "o/r", "12"),
		p(64, 10, "parallel", "perl", "/usr/bin/parallel", "devctl", "pr", "merge", "o/r", ":::", "13"),
		p(65, 64, "sh", "/bin/sh", "-c", "devctl pr merge o/r 13"),
		p(66, 65, merge.Tool, "devctl", "pr", "merge", "o/r", "13"),
		p(67, 10, "xargs", "xargs", "-n1", "/home/u/.go/bin/beekeeper", "gate", "--", "devctl", "pr", "merge", "o/r"),
		p(68, 67, project.Name, "/home/u/.go/bin/beekeeper", MergeChildCmd, "/home/u/.local/state/beekeeper/merges/o-r-14"),
		p(69, 68, merge.Tool, "devctl", "pr", "merge", "o/r", "14"),
	} {
		tb.ByPID[pr.PID] = pr
	}
	return tb
}

// A merge a runner started from its arguments, which slipped past the hook,
// is ungated in the watch and the doctor, naming the runner as its parent;
// one the runner started through the gate is none.
func TestUngatedMergesUnderRunners(t *testing.T) {
	sessions := []*claude.Session{{PID: 10, ID: workerSession, Name: ungatedWorker}}
	want := []string{
		`devctl pr merge o/r 11 (pid 61, session "Worker A", under xargs -n (pid 60)` + ungatedWhat,
		`devctl pr merge o/r 12 (pid 63, session "Worker A", under find . -n -e (pid 62)` + ungatedWhat,
		`devctl pr merge o/r 13 (pid 66, session "Worker A", under sh -c (pid 65)` + ungatedWhat,
	}
	if got := ungatedLines(sessions, runnerTable()); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("doctor's ungated lines:\n got %q\nwant %q", got, want)
	}
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	w.now = time.Now()
	w.ungated(sessions, runnerTable())
	s := out.String()
	for _, l := range want {
		if strings.Count(s, "UNGATED MERGE "+l) != 1 {
			t.Errorf("the watch does not say once: %q\n%s", l, s)
		}
	}
	if strings.Contains(s, "(pid 69,") {
		t.Errorf("a merge under the gate is said:\n%s", s)
	}
}

// A merge a script starts beneath a shell's -c is still visible to both
// watch and doctor if it slips past the hook; shell ancestry is no gate.
func TestUngatedMergesInShellCode(t *testing.T) {
	for _, command := range []string{"bash merge.sh", ". merge.sh", "./merge.sh", "sh -c 'bash merge.sh'"} {
		t.Run(command, func(t *testing.T) {
			tb := ungatedTable()
			tb.ByPID[60] = &proc.Process{PID: 60, PPID: 10, Comm: testShell, Args: []string{testShell, "-c", command}}
			tb.ByPID[11].PPID = 60
			sessions := []*claude.Session{{PID: 10, ID: workerSession, Name: ungatedWorker}}
			want := `devctl pr merge o/r 1 (pid 12, session "Worker A", under bash /home/u/scratch/merge.sh (pid 11)` + ungatedWhat
			if got := ungatedLines(sessions, tb); len(got) != 3 || got[0] != want {
				t.Fatalf("doctor's ungated lines = %q", got)
			}
			w, _, out := notifyingWatch(t, t.TempDir(), false)
			w.now = time.Now()
			w.ungated(sessions, tb)
			if strings.Count(out.String(), "UNGATED MERGE "+want) != 1 {
				t.Fatalf("watch did not report the script's merge: %s", out.String())
			}
		})
	}
}
