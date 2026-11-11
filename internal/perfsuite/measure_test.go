//go:build perf && linux

package perfsuite

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	"golang.org/x/sys/unix"
)

// sample is what one process cost: its own and its waited-for descendants'.
type sample struct {
	Wall     time.Duration
	CPU      time.Duration
	RSS      int64 // peak resident set of the process or a descendant, bytes
	Read     int64 // bytes read through read syscalls (rchar), cached or not
	Write    int64 // bytes written (wchar)
	Children int   // processes started: fakes traced, beekeeper re-executions seen
	Exit     int
	Output   string
}

// clockTicks is USER_HZ, the unit of /proc/<pid>/stat times on Linux.
const clockTicks = 100

// run runs the binary under test with args on the desk and measures it. The
// exited process is held as a zombie until its /proc/<pid>/io, which then
// includes its reaped descendants, is read.
func (d *desk) run(ctx context.Context, args ...string) sample {
	c := exec.CommandContext(ctx, d.bin, args...) //nolint:gosec // binary under test
	c.Env = d.env
	c.Dir = d.dir
	c.WaitDelay = 5 * time.Second
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	before := d.traceCount()
	started := time.Now()
	if err := c.Start(); err != nil {
		return sample{Exit: -1, Output: err.Error()}
	}
	pid := c.Process.Pid
	w := watchDescendants(pid, filepath.Base(d.bin))
	var info unix.Siginfo
	for {
		if err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil); !errors.Is(err, unix.EINTR) {
			break
		}
	}
	s := sample{Wall: time.Since(started)}
	s.Read, s.Write = readIO(pid)
	selfRuns := w.stop()
	err := c.Wait()
	if c.ProcessState != nil {
		if ru, ok := c.ProcessState.SysUsage().(*syscall.Rusage); ok {
			s.CPU = time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
			s.RSS = ru.Maxrss * 1024
		}
		s.Exit = c.ProcessState.ExitCode()
	} else if err != nil {
		s.Exit = -1
	}
	s.Children = d.traceCount() - before + selfRuns
	s.Output = tailString(out.String(), 2000)
	return s
}

func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// readIO is a process's rchar and wchar from /proc/<pid>/io.
func readIO(pid int) (read, write int64) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/io")
	if err != nil {
		return -1, -1
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		switch k {
		case "rchar":
			read = n
		case "wchar":
			write = n
		}
	}
	return read, write
}

// procStat is a process's parent, start time, name and CPU (its own and its
// reaped children's) from /proc/<pid>/stat.
type procStat struct {
	ppid  int
	start uint64
	comm  string
	cpu   time.Duration
}

func readStat(pid int) (procStat, bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return procStat{}, false
	}
	s := string(raw)
	open, end := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if open < 0 || end < open {
		return procStat{}, false
	}
	f := strings.Fields(s[end+1:])
	// f[0] is field 3 (state): ppid is field 4, utime..cstime 14..17,
	// starttime 22.
	if len(f) < 20 {
		return procStat{}, false
	}
	ps := procStat{comm: s[open+1 : end]}
	ps.ppid, _ = strconv.Atoi(f[1])
	var ticks uint64
	for _, i := range []int{11, 12, 13, 14} {
		n, _ := strconv.ParseUint(f[i], 10, 64)
		ticks += n
	}
	ps.cpu = time.Duration(ticks) * time.Second / clockTicks
	ps.start, _ = strconv.ParseUint(f[19], 10, 64)
	return ps, true
}

// descendants samples /proc every 20ms for the processes below root named
// name (beekeeper re-executing itself); fakes count themselves in the trace.
type descendants struct {
	mu    sync.Mutex
	seen  map[[2]uint64]bool
	done  chan struct{}
	ended chan struct{}
}

func watchDescendants(root int, name string) *descendants {
	w := &descendants{seen: map[[2]uint64]bool{}, done: make(chan struct{}), ended: make(chan struct{})}
	if len(name) > 15 {
		name = name[:15]
	}
	go func() {
		defer close(w.ended)
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			w.scan(root, name)
			select {
			case <-w.done:
				return
			case <-tick.C:
			}
		}
	}()
	return w
}

func (w *descendants) scan(root int, name string) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	stats := map[int]procStat{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if ps, ok := readStat(pid); ok {
			stats[pid] = ps
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for pid, ps := range stats {
		if pid == root || ps.comm != name {
			continue
		}
		for p, hops := ps.ppid, 0; p > 1 && hops < 64; hops++ {
			if p == root {
				w.seen[[2]uint64{uint64(pid), ps.start}] = true //nolint:gosec // pid is positive
				break
			}
			p = stats[p].ppid
		}
	}
}

// count is the number of distinct descendants seen so far.
func (w *descendants) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.seen)
}

// stop ends the sampling and returns the count.
func (w *descendants) stop() int {
	close(w.done)
	<-w.ended
	return w.count()
}

// lockProbe measures the longest time the state lock was held exclusively:
// it takes and drops a shared lock every 2ms, which only an exclusive holder
// refuses, and keeps the longest run of refusals.
type lockProbe struct {
	longest time.Duration
	done    chan struct{}
	ended   chan struct{}
}

func probeLock(stateDir string) *lockProbe {
	p := &lockProbe{done: make(chan struct{}), ended: make(chan struct{})}
	go func() {
		defer close(p.ended)
		l := flock.New(filepath.Join(stateDir, "state.lock"))
		var since time.Time
		tick := time.NewTicker(2 * time.Millisecond)
		defer tick.Stop()
		for {
			ok, err := l.TryRLock()
			now := time.Now()
			switch {
			case err == nil && ok:
				_ = l.Unlock()
				if !since.IsZero() {
					p.longest = max(p.longest, now.Sub(since))
					since = time.Time{}
				}
			case since.IsZero():
				since = now
			}
			select {
			case <-p.done:
				if !since.IsZero() {
					p.longest = max(p.longest, time.Since(since))
				}
				return
			case <-tick.C:
			}
		}
	}()
	return p
}

func (p *lockProbe) stop() time.Duration {
	close(p.done)
	<-p.ended
	return p.longest
}
