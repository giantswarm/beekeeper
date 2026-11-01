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

// The broker that runs the binary an update replaced restarts onto the new
// one, its dropped vault session said; one serving calls (its children; the
// op daemon is reparented) is left to them, the restart due named; one on
// another binary, started after the write or not running stays untouched.
func TestRestartBrokerOn(t *testing.T) {
	written := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	before, after := written.Add(-time.Hour), written.Add(time.Second)
	cases := []struct {
		name    string
		bin     string
		start   time.Time
		child   bool
		down    bool
		failing bool
		want    string
	}{
		{name: "replaced and idle", bin: restartExe, start: before, want: "restart beekeeper-sandbox.service (pid 40): on " + restartExe + " now; install prepares the vault handover"},
		{name: "serving a call", bin: restartExe, start: before, child: true, want: "due     beekeeper-sandbox.service (pid 40) stays on the replaced binary while it serves 1 call(s)"},
		{name: "restart refused", bin: restartExe, start: before, failing: true, want: "due     beekeeper-sandbox.service (pid 40) stays on the replaced binary (refused): rerun beekeeper install to retry onto " + restartExe},
		{name: "another binary", bin: "/opt/beekeeper", start: before},
		{name: "started on the new binary", bin: restartExe, start: after},
		{name: "no broker", bin: restartExe, start: before, down: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tab := &proc.Table{ByPID: map[int]*proc.Process{
				40: {PID: 40, PPID: 1, Comm: project.Name, Start: c.start},
				41: {PID: 41, PPID: 1, Comm: "op"},
			}}
			if c.child {
				tab.ByPID[42] = &proc.Process{PID: 42, PPID: 40, Comm: project.Name}
			}
			prevP, prevR := brokerProcess, restartBroker
			t.Cleanup(func() { brokerProcess, restartBroker = prevP, prevR })
			brokerProcess = func() (int, string, error) {
				if c.down {
					return 0, "", errors.New("down")
				}
				return 40, c.bin, nil
			}
			restarted := false
			restartBroker = func(context.Context) error {
				restarted = true
				if c.failing {
					return errors.New("refused")
				}
				return nil
			}
			var out bytes.Buffer
			restartBrokerOn(context.Background(), &out, restartExe, written, tab, false, false)
			if c.want == "" {
				if out.Len() != 0 || restarted {
					t.Fatalf("restarted %v, said %q; want nothing", restarted, out.String())
				}
				return
			}
			if !strings.Contains(out.String(), c.want) {
				t.Fatalf("said %q, want %q", out.String(), c.want)
			}
			if want := !c.child; restarted != want {
				t.Fatalf("restarted %v, want %v", restarted, want)
			}
		})
	}

	// a dry run names the restart and takes none
	prevP, prevR := brokerProcess, restartBroker
	t.Cleanup(func() { brokerProcess, restartBroker = prevP, prevR })
	brokerProcess = func() (int, string, error) { return 40, restartExe, nil }
	restartBroker = func(context.Context) error { t.Fatal("a dry run restarted the broker"); return nil }
	var out bytes.Buffer
	tab := &proc.Table{ByPID: map[int]*proc.Process{40: {PID: 40, Comm: project.Name, Start: before}}}
	restartBrokerOn(context.Background(), &out, restartExe, written, tab, true, false)
	if !strings.HasPrefix(out.String(), "restart beekeeper-sandbox.service (pid 40)") {
		t.Fatalf("dry run said %q", out.String())
	}
}

func TestRestartBrokerRefusesUnavailableHandover(t *testing.T) {
	prevP, prevR, prevH := brokerProcess, restartBroker, prepareBrokerHandover
	t.Cleanup(func() { brokerProcess, restartBroker, prepareBrokerHandover = prevP, prevR, prevH })
	brokerProcess = func() (int, string, error) { return 40, restartExe, nil }
	prepareBrokerHandover = func(context.Context) error { return errors.New("unsupported handover") }
	restartBroker = func(context.Context) error { t.Fatal("restarted without the session handover"); return nil }
	written := time.Now()
	tab := &proc.Table{ByPID: map[int]*proc.Process{40: {PID: 40, Start: written.Add(-time.Hour)}}}
	var out bytes.Buffer
	restartBrokerOn(context.Background(), &out, restartExe, written, tab, false, true)
	if !strings.Contains(out.String(), "restart refused") {
		t.Fatal("missing warning before restart")
	}
	for _, want := range []string{"systemctl --user restart beekeeper-sandbox.service", "drops the vault session", "new sign-in"} {
		if !strings.Contains(out.String(), want) {
			t.Fatal("missing actionable legacy remedy", want)
		}
	}
	if strings.Contains(out.String(), "rerun beekeeper install") {
		t.Fatal("suggested retry cannot update the running legacy broker")
	}
}

func TestRestartBrokerPreparesHandoverBeforeStopping(t *testing.T) {
	prevP, prevR, prevH := brokerProcess, restartBroker, prepareBrokerHandover
	t.Cleanup(func() { brokerProcess, restartBroker, prepareBrokerHandover = prevP, prevR, prevH })
	brokerProcess = func() (int, string, error) { return 40, restartExe, nil }
	prepared, restarted := false, false
	prepareBrokerHandover = func(context.Context) error { prepared = true; return nil }
	restartBroker = func(context.Context) error {
		if !prepared {
			t.Fatal("restart preceded handover")
		}
		restarted = true
		return nil
	}
	written := time.Now()
	tab := &proc.Table{ByPID: map[int]*proc.Process{40: {PID: 40, Start: written.Add(-time.Hour)}}}
	var out bytes.Buffer
	restartBrokerOn(context.Background(), &out, restartExe, written, tab, true, true)
	if prepared || restarted {
		t.Fatal("dry run changed broker state")
	}
	restartBrokerOn(context.Background(), &out, restartExe, written, tab, false, true)
	if !prepared || !restarted {
		t.Fatal("restart did not use the prepared handover")
	}
}

func TestRestartBrokerFailureRevokesHandover(t *testing.T) {
	prevP, prevR, prevH, prevV := brokerProcess, restartBroker, prepareBrokerHandover, revokeBrokerHandover
	t.Cleanup(func() {
		brokerProcess, restartBroker, prepareBrokerHandover, revokeBrokerHandover = prevP, prevR, prevH, prevV
	})
	brokerProcess = func() (int, string, error) { return 40, restartExe, nil }
	prepared, revoked := false, false
	prepareBrokerHandover = func(context.Context) error { prepared = true; return nil }
	restartBroker = func(context.Context) error { return errors.New("restart failed") }
	revokeBrokerHandover = func(context.Context) error { revoked = true; return nil }
	written := time.Now()
	tab := &proc.Table{ByPID: map[int]*proc.Process{40: {PID: 40, Start: written.Add(-time.Hour)}}}
	var out bytes.Buffer
	restartBrokerOn(context.Background(), &out, restartExe, written, tab, false, true)
	if !prepared || !revoked {
		t.Fatal("failed restart did not revoke its snapshot")
	}
}
