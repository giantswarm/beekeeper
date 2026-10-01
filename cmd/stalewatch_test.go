package cmd

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/pkg/project"
)

func TestStaleWatches(t *testing.T) {
	const path = "/home/u/.go/bin/beekeeper"
	table := func(ps ...*proc.Process) *proc.Table {
		tb := &proc.Table{ByPID: map[int]*proc.Process{}}
		for _, p := range ps {
			tb.ByPID[p.PID] = p
		}
		return tb
	}
	bk := func(pid, ppid int, args ...string) *proc.Process {
		return &proc.Process{PID: pid, PPID: ppid, Comm: "beekeeper", Args: append([]string{path}, args...)}
	}
	claudeCLI := &proc.Process{PID: 10, PPID: 1, Comm: "claude", Args: []string{"claude"}}
	supervisorWatch := bk(100, 10, "watch", "--notify")
	self := bk(os.Getpid(), 10, "watch")
	everything := table(claudeCLI, supervisorWatch, self, bk(101, 10, "watch", "--once"), bk(102, 10, "status"))

	var out bytes.Buffer
	replaced := map[int]bool{}
	w := &watcher{app: &app{out: &out, cfg: &config.Config{}}, last: map[string]time.Time{}}
	w.owners.Store(&map[int]string{10: "Supervisor 3"})
	w.replaced = func(pid int) (string, bool) { return path, replaced[pid] }
	w.versionOf = func(_ context.Context, file string) string {
		if file == path {
			return "v9.9.9"
		}
		return "v0.1." + strings.TrimSuffix(strings.TrimPrefix(file, "/proc/"), "/exe")
	}
	poll := func(tb *proc.Table) string {
		out.Reset()
		w.staleWatches(context.Background(), tb)
		return out.String()
	}

	if got := poll(everything); got != "" {
		t.Errorf("no binary replaced, the watch says:\n%s", got)
	}

	for pid := range everything.ByPID {
		replaced[pid] = true
	}
	got := poll(everything)
	for _, want := range []string{
		"WATCH STALE beekeeper watch --notify (pid 100, session Supervisor 3): runs v0.1.100; self-update installed v9.9.9 at " + path + ", a re-arm picks it up",
		"WATCH STALE this watch (pid " + strconv.Itoa(os.Getpid()) + ", session Supervisor 3): runs " + project.Version() + "; self-update installed v9.9.9",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the replaced binary's line is missing %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "WATCH STALE"); n != 2 {
		t.Errorf("%d WATCH STALE lines, want 2 (a --once poll and a non-watch are no watch):\n%s", n, got)
	}

	if got := poll(everything); got != "" {
		t.Errorf("the next poll repeats the stale watches:\n%s", got)
	}

	got = poll(table(claudeCLI, self))
	if !strings.Contains(got, "ENDED WATCH STALE beekeeper watch --notify (pid 100, session Supervisor 3)") || strings.Count(got, "\n") != 1 {
		t.Errorf("a re-armed watch's end is not one ENDED line:\n%s", got)
	}
}
