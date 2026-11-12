//go:build perf && linux

package perfsuite

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// loopSettle is how long the loop takes in what it has not read yet before a
// window: its slowest history reader, the throughput check, reads once a
// minute (throughputCadence), so the history it has not read is read within
// a minute and a pass. Until then a window measures that one catch-up, not
// what an idle pass reads.
const loopSettle = 75 * time.Second

// settle waits until the loop has read what is new.
func settle(interval time.Duration) {
	time.Sleep(max(3*interval+10*time.Second, loopSettle))
}

// Loop budgets per pass of the supervisor loop on the desk of record. An
// idle pass reads what changed, not the state and the event log again; at the
// 30-second production interval its CPU is a few percent of one core.
var (
	loopIdleCPU      = limit{Max: 0.5}
	loopGrownCPU     = limit{Max: loopIdleCPU.Max}
	loopIdleRead     = limit{Max: 8}
	loopIdleChildren = limit{Max: 5}
	// The event log grows; an idle pass must not: at most this factor of
	// the pass before plus loopGrowthSlack MiB.
	loopGrowthFactor = 1.25
	loopGrowthSlack  = 0.5
)

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
	d     *desk
	cmd   *exec.Cmd
	w     *descendants
	files *fileReads
}

func (d *desk) startLoop(t *testing.T) *runningLoop {
	t.Helper()
	logFile, err := os.OpenFile(filepath.Join(d.dir, "loop.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command(d.bin, "supervisor", "loop") //nolint:gosec // binary under test, stopped by stop
	// Its CPU profile names the code behind a pass's CPU and reads; the
	// artifacts keep it for go tool pprof.
	c.Env, c.Dir, c.Stdout, c.Stderr = append(slices.Clone(d.env), "BEEKEEPER_CPUPROFILE="+filepath.Join(d.dir, "loop.pprof")), d.dir, logFile, logFile
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	_ = logFile.Close()
	l := &runningLoop{d: d, cmd: c, w: watchDescendants(c.Process.Pid, filepath.Base(d.bin), true), files: watchFileReads(c.Process.Pid, d.dir)}
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
	l.files.stop()
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

// window measures the loop over d, per pass of interval, and notes the
// files it read for kase and what it read in each pass's time: a one-time
// catch-up shows as one large value, a pass that reads too much as many.
func (l *runningLoop) window(t *testing.T, kase string, d time.Duration, interval time.Duration) (cpu, readMiB, writeMiB, children float64) {
	t.Helper()
	before, files := l.usage(t), l.files.snapshot()
	var perPass []string
	last := before.read
	for end := time.Now().Add(d); time.Until(end) > 0; {
		time.Sleep(min(interval, time.Until(end)))
		u := l.usage(t)
		perPass = append(perPass, fmt.Sprintf("%.2f", float64(u.read-last)/mib))
		last = u.read
	}
	after := l.usage(t)
	passes := d.Seconds() / interval.Seconds()
	rep.note("%s %s: %s; MiB read in each pass's time: %s", t.Name(), kase, readsBetween(files, l.files.snapshot(), passes, "pass"), strings.Join(perPass, " "))
	return (after.cpu - before.cpu).Seconds() / passes,
		float64(after.read-before.read) / mib / passes,
		float64(after.write-before.write) / mib / passes,
		float64(after.children-before.children) / passes
}

// checkCPU checks a pass's CPU against its budget. A value over it is
// measured again over one more window and the lower one counts: a loop over
// its budget is over in both, a loaded CI runner's slow moment in one.
func (l *runningLoop) checkCPU(t *testing.T, kase string, cpu float64, budget limit, d, interval time.Duration) {
	t.Helper()
	if budget.Max >= 0 && cpu > budget.Max {
		again, _, _, _ := l.window(t, kase+" (again)", d, interval)
		rep.note("%s %s: cpu %.3f s over the budget of %.3f s, measured again: %.3f s (%s)", t.Name(), kase, cpu, budget.Max, again, runnerLoad())
		cpu = min(cpu, again)
	}
	rep.check(t, kase, "cpu", cpu, budget)
}

// runnerLoad is the machine's load for the report beside a CPU value.
func runnerLoad() string {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return "load unknown"
	}
	f := strings.Fields(string(raw))
	if len(f) < 3 {
		return "load unknown"
	}
	return fmt.Sprintf("load %s %s %s on %d CPUs", f[0], f[1], f[2], runtime.NumCPU())
}

func TestLoopBudgets(t *testing.T) {
	requirePerf(t)
	o := defaultDesk()
	d := newDesk(t, o)
	l := d.startLoop(t)
	// Startup and the first passes read everything once; budgets are for the
	// passes after.
	settle(o.loopInterval)
	win := loopWindow()

	cpu, read, write, children := l.window(t, "idle pass", win, o.loopInterval)
	l.checkCPU(t, "idle pass", cpu, loopIdleCPU, win, o.loopInterval)
	rep.check(t, "idle pass", "read", read, loopIdleRead)
	rep.check(t, "idle pass", "write", write, measured)
	rep.check(t, "idle pass", "children", children, loopIdleChildren)
	idleRead := read

	if err := d.store.Record(state.Event{At: time.Now().UTC(), By: state.Party{Name: "perf worker 3", Person: fixturePerson}, Verb: "agents.park", Subject: fixtureRepo + "#1003", Detail: "[perf] one event"}); err != nil {
		t.Fatal(err)
	}
	cpu, read, write, children = l.window(t, "pass with one event", o.loopInterval*2, o.loopInterval)
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
	settle(o.loopInterval)
	cpu, read, write, children = l.window(t, "idle pass, 4x event log", win, o.loopInterval)
	grown := limit{Max: idleRead*loopGrowthFactor + loopGrowthSlack}
	rep.check(t, "idle pass, 4x event log", "read", read, grown)
	l.checkCPU(t, "idle pass, 4x event log", cpu, loopGrownCPU, win, o.loopInterval)
	rep.check(t, "idle pass, 4x event log", "write", write, measured)
	rep.check(t, "idle pass, 4x event log", "children", children, loopIdleChildren)
	if idleRead > 0 {
		rep.check(t, "idle pass, 4x event log", "growth", read/idleRead, measured)
	}
}
