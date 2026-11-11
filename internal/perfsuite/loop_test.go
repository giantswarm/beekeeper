//go:build perf && linux

package perfsuite

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// loopIssue is the open issue that has the loop over its budgets today: on an
// event log four times the size, an idle pass reads several times as much.
// Idle passes are within their CPU and process budgets. On the grown event
// log the CPU budget holds over the nightly two-minute window, but not over a
// pull request's 30-second window, which spreads the same reads over a
// quarter of the passes.
const loopIssue = "https://github.com/giantswarm/beekeeper/issues/3657"

// Loop budgets per pass of the supervisor loop on the desk of record. An
// idle pass reads what changed, not the state and the event log again; at the
// 30-second production interval its CPU is a few percent of one core.
var (
	loopIdleCPU      = limit{Max: 0.5}
	loopGrownCPU     = grownCPU()
	loopIdleRead     = limit{Max: 8}
	loopIdleChildren = limit{Max: 5}
	// The event log grows; an idle pass must not: at most this factor of
	// the pass before plus loopGrowthSlack MiB.
	loopGrowthFactor = 1.25
	loopGrowthSlack  = 0.5
)

// grownCPU is the CPU budget of an idle pass on the grown event log: enforced
// over the nightly window, known under loopIssue over the short one.
func grownCPU() limit {
	if os.Getenv("BEEKEEPER_PERF_LONG") == "1" {
		return limit{Max: loopIdleCPU.Max}
	}
	return limit{Max: loopIdleCPU.Max, Known: loopIssue}
}

// loopWindow is how long each measurement runs: a few passes on a pull
// request, more nightly (BEEKEEPER_PERF_LONG=1).
func loopWindow() time.Duration {
	if os.Getenv("BEEKEEPER_PERF_LONG") == "1" {
		return 2 * time.Minute
	}
	return 30 * time.Second
}

// loopUsage is a running loop's cumulative cost.
type loopUsage struct {
	cpu         time.Duration
	read, write int64
	children    int
}

type runningLoop struct {
	d   *desk
	cmd *exec.Cmd
	w   *descendants
}

func (d *desk) startLoop(t *testing.T) *runningLoop {
	t.Helper()
	logFile, err := os.OpenFile(filepath.Join(d.dir, "loop.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command(d.bin, "supervisor", "loop") //nolint:gosec // binary under test, stopped by stop
	c.Env, c.Dir, c.Stdout, c.Stderr = d.env, d.dir, logFile, logFile
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	_ = logFile.Close()
	l := &runningLoop{d: d, cmd: c, w: watchDescendants(c.Process.Pid, filepath.Base(d.bin), true)}
	t.Cleanup(l.stop)
	return l
}

func (l *runningLoop) stop() {
	if l.cmd.ProcessState != nil {
		return
	}
	_ = l.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = l.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		_ = l.cmd.Process.Kill()
		<-done
	}
	l.w.stop()
}

func (l *runningLoop) usage(t *testing.T) loopUsage {
	t.Helper()
	pid := l.cmd.Process.Pid
	ps, ok := readStat(pid)
	if !ok || procState(pid) == "Z" {
		raw, _ := os.ReadFile(filepath.Join(l.d.dir, "loop.log"))
		t.Fatalf("supervisor loop is not running:\n%s", tailString(strings.ReplaceAll(string(raw), l.d.dir, "<fixture>"), 4000))
	}
	u := loopUsage{cpu: ps.cpu, children: l.d.traceCount() + l.w.count()}
	u.read, u.write = readIO(pid)
	return u
}

func procState(pid int) string {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	s := string(raw)
	end := strings.LastIndexByte(s, ')')
	if end < 0 || end+3 > len(s) {
		return ""
	}
	return s[end+2 : end+3]
}

// window measures the loop over d, per pass of interval.
func (l *runningLoop) window(t *testing.T, d time.Duration, interval time.Duration) (cpu, readMiB, writeMiB, children float64) {
	t.Helper()
	before := l.usage(t)
	time.Sleep(d)
	after := l.usage(t)
	passes := d.Seconds() / interval.Seconds()
	return (after.cpu - before.cpu).Seconds() / passes,
		float64(after.read-before.read) / mib / passes,
		float64(after.write-before.write) / mib / passes,
		float64(after.children-before.children) / passes
}

func TestLoopBudgets(t *testing.T) {
	requirePerf(t)
	o := defaultDesk()
	d := newDesk(t, o)
	l := d.startLoop(t)
	// Startup and the first passes read everything once; budgets are for the
	// passes after.
	time.Sleep(3*o.loopInterval + 10*time.Second)
	win := loopWindow()

	cpu, read, write, children := l.window(t, win, o.loopInterval)
	rep.check(t, "idle pass", "cpu", cpu, loopIdleCPU)
	rep.check(t, "idle pass", "read", read, loopIdleRead)
	rep.check(t, "idle pass", "write", write, measured)
	rep.check(t, "idle pass", "children", children, loopIdleChildren)
	idleRead := read

	if err := d.store.Record(state.Event{At: time.Now().UTC(), By: state.Party{Name: "perf worker 3", Person: fixturePerson}, Verb: "agents.park", Subject: fixtureRepo + "#1003", Detail: "[perf] one event"}); err != nil {
		t.Fatal(err)
	}
	cpu, read, write, children = l.window(t, o.loopInterval*2, o.loopInterval)
	// Two passes are too short for CPU and processes to settle: reported.
	rep.check(t, "pass with one event", "cpu", cpu, measured)
	rep.check(t, "pass with one event", "read", read, loopIdleRead)
	rep.check(t, "pass with one event", "write", write, measured)
	rep.check(t, "pass with one event", "children", children, measured)

	// Grow the event log to four times its size, let the loop take the new
	// events in, then an idle pass must cost what it cost before.
	grow := make([]state.Event, 0, 120000)
	since := time.Now().UTC().Add(-time.Hour)
	var added int
	for i := 0; added < 3*o.eventBytes; i++ {
		ev := state.Event{At: since.Add(time.Duration(i) * time.Millisecond), By: state.Party{Name: fmt.Sprintf("perf worker %d", i%deskSessions), Person: fixturePerson}, Verb: "supervisor.stage", Subject: fmt.Sprintf("%s#%d", fixtureRepo, 1000+i%deskPulls), Detail: fmt.Sprintf("[growth %d] %s", i, strings.Repeat("detail ", 40))}
		grow = append(grow, ev)
		added += 330 + len(ev.Detail)
	}
	if err := d.store.Record(grow...); err != nil {
		t.Fatal(err)
	}
	rep.note("%s: event log grown by %d events", t.Name(), len(grow))
	time.Sleep(3*o.loopInterval + 10*time.Second)
	cpu, read, write, children = l.window(t, win, o.loopInterval)
	grown := limit{Max: idleRead*loopGrowthFactor + loopGrowthSlack, Known: loopIssue}
	rep.check(t, "idle pass, 4x event log", "read", read, grown)
	rep.check(t, "idle pass, 4x event log", "cpu", cpu, loopGrownCPU)
	rep.check(t, "idle pass, 4x event log", "write", write, measured)
	rep.check(t, "idle pass, 4x event log", "children", children, loopIdleChildren)
	if idleRead > 0 {
		rep.check(t, "idle pass, 4x event log", "growth", read/idleRead, measured)
	}
}
