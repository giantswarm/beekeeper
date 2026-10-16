package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/pkg/project"
)

const restartExe = "/home/u/.go/bin/beekeeper"

// An install asks the watches (the standby unit's included) and the waiting
// reopens that still run the binary it replaced to restart, names each and
// leaves the rest: a --once poll, a reopen's detaching stop-post, an alerts
// or guide watch, a gate, another binary's process, one not replaced.
func TestRequestRestarts(t *testing.T) {
	procs := map[int]string{
		3:  "beekeeper watch --standby --notify",
		5:  "beekeeper agents reopen --turn beekeeper-start-x.service 1234",
		7:  watchOwner,
		9:  "beekeeper watch --once",
		11: "beekeeper agents reopen --detach 1234",
		13: "beekeeper alerts watch",
		15: "beekeeper guide watch",
		17: "beekeeper gate -- devctl pr merge o/r 1",
		19: watchOwner, // another binary
		21: watchOwner, // not replaced
		25: "beekeeper --config /c.yaml watch --standby",
		27: "beekeeper --config /c.yaml alerts watch",
	}
	tab := &proc.Table{ByPID: map[int]*proc.Process{}}
	for pid, cmd := range procs {
		tab.ByPID[pid] = &proc.Process{PID: pid, Comm: project.Name, Args: strings.Fields(cmd)}
	}
	tab.ByPID[23] = &proc.Process{PID: 23, Comm: "zsh", Args: strings.Fields(watchOwner)}
	binary := func(pid int) (string, bool) {
		switch pid {
		case 19:
			return "/tmp/wt/beekeeper", true
		case 21:
			return restartExe, false
		}
		return restartExe, true
	}
	dir := t.TempDir()
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

	var out bytes.Buffer
	due, err := requestRestarts(&out, dir, restartExe, tab, binary, now, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 4 {
		t.Fatalf("dry run asked %d processes, want 4:\n%s", len(due), out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, restartFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a dry run wrote the request: %v", err)
	}

	out.Reset()
	if _, err := requestRestarts(&out, dir, restartExe, tab, binary, now, false); err != nil {
		t.Fatal(err)
	}
	want := "restart pid 3 (beekeeper watch --standby --notify): on " + restartExe + " once its current step ends\n" +
		"restart pid 5 (beekeeper agents reopen --turn): on " + restartExe + " once its current step ends\n" +
		"restart pid 7 (beekeeper watch): on " + restartExe + " once its current step ends\n" +
		"restart pid 25 (beekeeper --config --standby): on " + restartExe + " once its current step ends\n"
	if got := out.String(); got != want {
		t.Errorf("restarts said:\n%s\nwant:\n%s", got, want)
	}
	r := &restartWatch{file: filepath.Join(dir, restartFile), since: now.Add(-time.Hour), binary: func() (string, bool) { return restartExe, true }}
	req := r.due()
	if req == nil || !req.At.Equal(now) || req.Binary != restartExe {
		t.Fatalf("the request read back as %+v", req)
	}

	// Nothing to restart writes nothing.
	empty := t.TempDir()
	if due, err := requestRestarts(&out, empty, "/other/beekeeper", tab, binary, now, false); err != nil || len(due) != 0 {
		t.Fatalf("another path asked %d processes: %v", len(due), err)
	}
	if _, err := os.Stat(filepath.Join(empty, restartFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no restart wrote the request: %v", err)
	}
}

// A process restarts for a request newer than its start or the request it
// restarted for, of the path it runs, once another file replaced it: a
// re-executed process, no longer replaced, never restarts twice.
func TestRestartDue(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	tab := &proc.Table{ByPID: map[int]*proc.Process{3: {PID: 3, Comm: project.Name, Args: []string{project.Name, watchName}}}}
	if _, err := requestRestarts(&bytes.Buffer{}, dir, restartExe, tab, func(int) (string, bool) { return restartExe, true }, at, false); err != nil {
		t.Fatal(err)
	}
	path, replaced := restartExe, true
	r := &restartWatch{file: filepath.Join(dir, restartFile), binary: func() (string, bool) { return path, replaced }}
	for _, c := range []struct {
		name     string
		since    time.Time
		path     string
		replaced bool
		due      bool
	}{
		{"started before the request", at.Add(-time.Minute), restartExe, true, true},
		{"started after the request", at.Add(time.Minute), restartExe, true, false},
		{"answered the request", at, restartExe, true, false},
		{"runs the installed binary", at.Add(-time.Minute), restartExe, false, false},
		{"runs another path", at.Add(-time.Minute), "/tmp/wt/beekeeper", true, false},
	} {
		r.since, path, replaced = c.since, c.path, c.replaced
		if got := r.due() != nil; got != c.due {
			t.Errorf("%s: due %v, want %v", c.name, got, c.due)
		}
	}
	r.file = filepath.Join(dir, "missing.json")
	r.since, path, replaced = at.Add(-time.Minute), restartExe, true
	if r.due() != nil {
		t.Error("no request file is due")
	}
}

// A watch an install asks to restart ends its run after the poll, once every
// background run it started ended, with the request it restarts for.
func TestWatchRestartsAfterBackgroundRuns(t *testing.T) {
	w, _ := loopWatcher(t, "1h", "", nil)
	at := time.Now().Add(-time.Second)
	tab := &proc.Table{ByPID: map[int]*proc.Process{3: {PID: 3, Comm: project.Name, Args: []string{project.Name, watchName}}}}
	if _, err := requestRestarts(&bytes.Buffer{}, w.cfg.StateDir, restartExe, tab, func(int) (string, bool) { return restartExe, true }, at, false); err != nil {
		t.Fatal(err)
	}
	w.restart = &restartWatch{file: filepath.Join(w.cfg.StateDir, restartFile), since: at.Add(-time.Hour),
		binary: func() (string, bool) { return restartExe, true }}
	saving := make(chan struct{})
	w.bg.Go(func() { <-saving })

	done := make(chan error, 1)
	go func() { done <- w.run(context.Background(), false) }()
	select {
	case err := <-done:
		t.Fatalf("the run ended with a save under way: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(saving)
	select {
	case err := <-done:
		if !errors.Is(err, errRestart) {
			t.Fatalf("the run ended with %v, want the restart", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the run did not end for the restart")
	}
	if w.restartFor == nil || w.restartFor.Binary != restartExe {
		t.Errorf("restarts for %+v", w.restartFor)
	}
}

// A watch without a request runs on: poll after poll, each asking for the
// request, its loops wait for the next, and only the cancel ends the run.
func TestWatchWithoutRequestRunsOn(t *testing.T) {
	w, _ := loopWatcher(t, "1h", "", nil)
	clk := newFakeClock(t)
	w.clock = clk
	w.restart = &restartWatch{file: filepath.Join(w.cfg.StateDir, restartFile), since: clk.Now(),
		binary: func() (string, bool) { return restartExe, true }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.run(ctx, false) }()
	// Every loop armed its timer: the poll before ran and asked for the
	// request. A run that ended arms none after the next step, and settle
	// fails.
	clk.settle(loopTimers)
	clk.step(loopTimers)
	clk.settle(loopTimers)
	if n := w.polls.Load(); n != 2 {
		t.Fatalf("%d polls after one interval, want 2", n)
	}
	select {
	case err := <-done:
		t.Fatalf("the run ended by itself: %v", err)
	default:
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("the cancelled run ended with %v", err)
	}
}
