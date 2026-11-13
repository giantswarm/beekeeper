//go:build perf && linux

package perfsuite

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The feeds a desk runs for as long as it runs, beside the supervisor loop:
// the guide's (guide watch) and the person's desktop one (watch --notify
// --standby). Idle, with the loop running and no session's turn, each costs
// at most this share of one core, and reads at most watchIdleRead MiB a
// second: it waits for a change, not re-reads on a timer.
var (
	watchIdleCPU  = limit{Max: 0.05}
	watchIdleRead = limit{Max: 1}
)

var watchFeeds = []struct {
	name string
	args []string
}{
	{"guide watch", []string{"--as", "perf guide", "guide", "watch"}},
	{"watch --notify --standby", []string{"watch", "--notify", "--standby"}},
}

type runningFeed struct {
	name  string
	cmd   *exec.Cmd
	log   string
	files *fileReads
}

func (d *desk) startFeed(t *testing.T, name string, args []string) *runningFeed {
	t.Helper()
	logPath := filepath.Join(d.dir, strings.ReplaceAll(name, " ", "-")+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // fixture log
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command(d.bin, args...) //nolint:gosec // binary under test, stopped by stop
	// Its CPU profile names the code behind its CPU, as the loop's does.
	profile := filepath.Join(d.dir, strings.ReplaceAll(name, " ", "-")+".pprof")
	c.Env, c.Dir, c.Stdout, c.Stderr = append(slices.Clone(d.env), "BEEKEEPER_CPUPROFILE="+profile), d.dir, logFile, logFile
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	_ = logFile.Close()
	f := &runningFeed{name: name, cmd: c, log: logPath, files: watchFileReads(c.Process.Pid, d.dir)}
	t.Cleanup(func() { f.stop(d) })
	return f
}

func (f *runningFeed) stop(d *desk) {
	if f.cmd.ProcessState != nil {
		return
	}
	_ = f.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = f.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		_ = f.cmd.Process.Kill()
		<-done
	}
	f.files.stop()
	if dest := os.Getenv("BEEKEEPER_PERF_REPORT"); dest != "" {
		if raw, err := os.ReadFile(f.log); err == nil {
			dest = filepath.Join(dest, strings.ReplaceAll(d.t.Name(), "/", "-"))
			if os.MkdirAll(dest, 0o700) == nil {
				_ = os.WriteFile(filepath.Join(dest, filepath.Base(f.log)), []byte(strings.ReplaceAll(string(raw), d.dir, "<fixture>")), 0o600)
			}
		}
	}
}

// usage is the feed's CPU (its own and its reaped children's) and reads.
func (f *runningFeed) usage(t *testing.T) (time.Duration, int64) {
	t.Helper()
	pid := f.cmd.Process.Pid
	ps, ok := readStat(pid)
	if !ok || procState(pid) == "Z" {
		raw, _ := os.ReadFile(f.log)
		t.Fatalf("%s is not running:\n%s", f.name, tailString(string(raw), 4000))
	}
	read, _ := readIO(pid)
	return ps.cpu, read
}

func TestWatchBudgets(t *testing.T) {
	requirePerf(t)
	o := defaultDesk()
	d := newDesk(t, o)
	d.startLoop(t)
	feeds := make([]*runningFeed, 0, len(watchFeeds))
	for _, w := range watchFeeds {
		feeds = append(feeds, d.startFeed(t, w.name, w.args))
	}
	// A feed's first poll reads everything once, as the loop's first passes
	// do; budgets are for the polls after.
	time.Sleep(3*o.loopInterval + 10*time.Second)
	win := loopWindow()
	type usage struct {
		cpu   time.Duration
		read  int64
		files map[string]int64
	}
	before := make([]usage, len(feeds))
	for i, f := range feeds {
		cpu, read := f.usage(t)
		before[i] = usage{cpu, read, f.files.snapshot()}
	}
	time.Sleep(win)
	for i, f := range feeds {
		cpu, read := f.usage(t)
		share := (cpu - before[i].cpu).Seconds() / win.Seconds()
		rep.check(t, f.name+", idle", "core", share, watchIdleCPU)
		rep.check(t, f.name+", idle", "read/s", float64(read-before[i].read)/mib/win.Seconds(), watchIdleRead)
		rep.note("%s %s, idle: %s (%s)", t.Name(), f.name, readsBetween(before[i].files, f.files.snapshot(), win.Seconds(), "second"), runnerLoad())
	}
}
