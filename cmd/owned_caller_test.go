//go:build unix

package cmd

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// fakeGate is a process standing in for a run's gate; stop kills and reaps
// it, as the caller's shell going away does.
func fakeGate(t *testing.T) (pid int, stop func()) {
	t.Helper()
	c := exec.Command("sleep", "60")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop = func() {
		if !stopped {
			stopped = true
			_ = c.Process.Kill()
			_ = c.Wait()
		}
	}
	t.Cleanup(stop)
	return c.Process.Pid, stop
}

// childOf writes spec for a run at base under a fresh runs directory and
// returns base and the file the fake devctl writes its pid to.
func childOf(t *testing.T, name string, spec childSpec) (base, devpid string) {
	t.Helper()
	base = filepath.Join(t.TempDir(), ownedRuns, name)
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		t.Fatal(err)
	}
	spec.Env = os.Environ()
	raw, _ := json.Marshal(spec)
	if err := os.WriteFile(base+".spec", raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return base, filepath.Join(t.TempDir(), "devctl.pid")
}

// pidIn waits for a pid in file.
func pidIn(t *testing.T, file string) int {
	t.Helper()
	var pid int
	if !eventually(childStart, func() bool {
		raw, err := os.ReadFile(file) //nolint:gosec // the test's file
		pid, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		return err == nil && pid > 0
	}) {
		t.Fatal("devctl did not start")
	}
	return pid
}

// A wait ends with its caller: once its gate is gone (the caller's shell
// stopped, its process tree killed) merge-child stops devctl at once, not at
// devctl's own timeout, and the run wakes nobody. A gate that sees its
// caller go away (SIGHUP) returns at once, the run's files left to its
// merge-child.
func TestAWaitEndsWithItsCaller(t *testing.T) {
	gate, stopGate := fakeGate(t)
	argv := strings.Fields("devctl release wait o/r v1.2.3")
	base, devpid := childOf(t, "release-wait-o_r-v1.2.3-1", childSpec{Argv: argv, Owner: state.Party{Session: "s1", Name: ownerName}, Gate: gate, WithCaller: true})
	fakeDevctl(t, `echo $$ >`+devpid+`; echo "waiting for tag v1.2.3 to exist" >&2; exec sleep 60`)
	done := make(chan childResult, 1)
	go func() { done <- mergeChild(base) }()
	dev := pidIn(t, devpid)
	stopGate()
	var r childResult
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("devctl runs on after its caller is gone")
	}
	if !r.callerGone || r.rc != 128+int(syscall.SIGTERM) || r.stop != 0 {
		t.Errorf("callerGone %v, exit %d, stop %v: want true, 143, none", r.callerGone, r.rc, r.stop)
	}
	if !eventually(5*time.Second, func() bool { return !proc.Alive(dev) }) {
		t.Error("devctl is left running")
	}
	if got := r.done().result(); !got.callerGone {
		t.Error("the re-exec's outcome lost callerGone")
	}
	a, _, woke := tellOwnerRun(t, r.spec.Owner)
	a.tellOwner(context.Background(), r)
	if len(*woke) != 0 {
		t.Errorf("a wait ended with its caller woke %q", *woke)
	}

	// The gate's side: its caller's SIGHUP ends the follow at once.
	follow := filepath.Join(t.TempDir(), "pr-wait-o_r-7-1")
	running := sleeper(t)
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGHUP)
	}()
	fr := followChild(follow, running, 0, nil, nil, time.Time{}, true, nil)
	if !fr.ended || fr.left || fr.rc != 128+int(syscall.SIGTERM) {
		t.Errorf("the gate's follow: %+v, want ended with 143", fr)
	}
}

// What outlives its gate keeps doing so and hands its outcome over: a
// merge's own run (a queued merge, whose gate exits 76 at once) and a wait
// its gate left to run on (the caller's timeout, the tool limit) both run
// devctl to its end with their gate gone, and their owner is woken with the
// outcome.
func TestARunLeftToRunOnOutlivesItsGate(t *testing.T) {
	owner := state.Party{Session: "s1", Name: ownerName}
	for _, c := range []struct {
		name string
		spec childSpec
		left bool
	}{
		{name: "pr-merge-o_r-7-1", spec: childSpec{Argv: mergeArgv("o/r"), Owner: owner}},
		{name: "release-wait-o_r-v1.2.3-1", spec: childSpec{Argv: strings.Fields("devctl release wait o/r v1.2.3"), Owner: owner, WithCaller: true}, left: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			gate, stopGate := fakeGate(t)
			c.spec.Gate = gate
			base, devpid := childOf(t, c.name, c.spec)
			if c.left {
				if err := os.WriteFile(base+leftExt, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			release := filepath.Join(t.TempDir(), "release")
			fakeDevctl(t, `echo $$ >`+devpid+`; while [ ! -e `+release+` ]; do sleep 0.1; done; echo '`+greenDoc+`'`)
			done := make(chan childResult, 1)
			go func() { done <- mergeChild(base) }()
			pidIn(t, devpid)
			stopGate()
			time.Sleep(5 * followPoll)
			select {
			case r := <-done:
				t.Fatalf("devctl ended with its gate: exit %d", r.rc)
			default:
			}
			if err := os.WriteFile(release, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			var r childResult
			select {
			case r = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("devctl did not finish")
			}
			if r.callerGone || r.rc != 0 {
				t.Fatalf("callerGone %v, exit %d: want false, 0", r.callerGone, r.rc)
			}
			a, _, woke := tellOwnerRun(t, owner)
			r.base = filepath.Join(a.store.Dir(), ownedRuns, c.name)
			a.tellOwner(context.Background(), r)
			if len(*woke) != 1 || !strings.Contains((*woke)[0], "exit 0: green: every check passed") {
				t.Errorf("woke %q, want the outcome", *woke)
			}
		})
	}
}

// A wait another run follows outlives its own caller while that follower
// runs: the follower's caller still waits for the outcome.
func TestAFollowedWaitOutlivesItsCaller(t *testing.T) {
	base := filepath.Join(t.TempDir(), "release-wait-o_r-v1.2.3-1")
	gate, stopGate := fakeGate(t)
	follower := sleeper(t)
	mark := strings.Replace(followerGlob(base), "*", strconv.Itoa(follower), 1)
	if err := os.WriteFile(mark, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if callerGone(base, gate) {
		t.Error("a gate still running counts as gone")
	}
	stopGate()
	if callerGone(base, gate) {
		t.Error("a followed wait ends with its own caller")
	}
	if err := os.Remove(mark); err != nil {
		t.Fatal(err)
	}
	if !callerGone(base, gate) {
		t.Error("an unfollowed wait outlives its caller")
	}
}
