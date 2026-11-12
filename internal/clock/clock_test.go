package clock_test

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/clock"
)

// childEnv makes this test binary print clock.Now in Unix nanoseconds and
// exit: a fresh process reads Env the way beekeeper does.
const childEnv = "BEEKEEPER_CLOCK_TEST_CHILD"

func TestMain(m *testing.M) {
	if os.Getenv(childEnv) != "" {
		fmt.Println(clock.Now().UnixNano())
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// worldAgo is how far a world served by fakeWorld sets the clock back.
const worldAgo = 20 * 365 * 24 * time.Hour

// fakeWorld serves a scenario world's clock on a Unix socket, speaking its
// protocol by hand: whoever connects is counted and told the world's time
// is worldAgo before the system's.
func fakeWorld(t *testing.T) (path string, connects *atomic.Int32) {
	t.Helper()
	dir, err := os.MkdirTemp("", "clock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path = filepath.Join(dir, "clock.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	connects = &atomic.Int32{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			connects.Add(1)
			go func() {
				defer func() { _ = c.Close() }()
				if _, err := bufio.NewReader(c).ReadString('\n'); err != nil {
					return
				}
				_, _ = fmt.Fprintf(c, "{\"seq\":0,\"offset\":%d}\n", -int64(worldAgo))
				_, _ = bufio.NewReader(c).ReadString('\n')
			}()
		}
	}()
	return path, connects
}

// childNow is clock.Now in a fresh process of this test binary with env.
func childNow(t *testing.T, env ...string) time.Time {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command(self, "-test.run=^$") //nolint:gosec // this test binary
	c.Env = append(os.Environ(), append(env, childEnv+"=1")...)
	out, err := c.Output()
	if err != nil {
		t.Fatalf("child: %v", err)
	}
	ns, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		t.Fatalf("child printed %q: %v", out, err)
	}
	return time.Unix(0, ns)
}

// TestReleaseIgnoresScenarioClock: a release build never reads
// BEEKEEPER_SCENARIO_CLOCK, even when it names a live world, and keeps the
// system clock. A scenario build (-tags scenario) reads it: the same
// process then has the world's time, so the check is not vacuous.
func TestReleaseIgnoresScenarioClock(t *testing.T) {
	path, connects := fakeWorld(t)
	before := time.Now()
	got := childNow(t, clock.Env+"="+path)
	after := time.Now()
	if !clock.Scenario {
		if got.Before(before.Add(-time.Second)) || got.After(after.Add(time.Second)) {
			t.Fatalf("a release build with %s set read %s, not the system's time between %s and %s", clock.Env, got, before, after)
		}
		if n := connects.Load(); n != 0 {
			t.Fatalf("a release build connected %d times to the scenario clock %s names", n, clock.Env)
		}
		return
	}
	want := before.Add(-worldAgo)
	if got.Before(want.Add(-time.Second)) || got.After(after.Add(-worldAgo).Add(time.Second)) {
		t.Fatalf("a scenario build with %s set read %s, not the world's time %s", clock.Env, got, want)
	}
	if n := connects.Load(); n != 1 {
		t.Fatalf("a scenario build connected %d times to the world's clock, want 1", n)
	}
}

// TestWithoutWorldTheSystemClock: without BEEKEEPER_SCENARIO_CLOCK every
// build keeps the system clock.
func TestWithoutWorldTheSystemClock(t *testing.T) {
	before := time.Now()
	got := childNow(t, clock.Env+"=")
	after := time.Now()
	if got.Before(before.Add(-time.Second)) || got.After(after.Add(time.Second)) {
		t.Fatalf("without %s the clock read %s, not the system's time between %s and %s", clock.Env, got, before, after)
	}
}

// TestSystemClock: in this test process (no world) the functions are the
// time package's.
func TestSystemClock(t *testing.T) {
	if d := clock.Since(time.Now()); d < 0 || d > time.Second {
		t.Fatalf("Since(now) = %s", d)
	}
	if d := clock.Until(time.Now().Add(time.Hour)); d < 59*time.Minute || d > time.Hour {
		t.Fatalf("Until(now+1h) = %s", d)
	}
	start := time.Now()
	clock.Sleep(10 * time.Millisecond)
	<-clock.After(10 * time.Millisecond)
	if d := time.Since(start); d < 20*time.Millisecond {
		t.Fatalf("Sleep and After took %s, want at least 20ms", d)
	}
	timer := clock.NewTimer(time.Hour)
	if !timer.Stop() {
		t.Fatal("Stop of a pending timer returned false")
	}
	timer.Reset(time.Millisecond)
	<-timer.C
	ticker := clock.NewTicker(time.Millisecond)
	<-ticker.C
	ticker.Reset(2 * time.Millisecond)
	<-ticker.C
	ticker.Stop()
	fired := make(chan struct{})
	clock.AfterFunc(time.Millisecond, func() { close(fired) })
	select {
	case <-fired:
	case <-time.After(10 * time.Second):
		t.Fatal("AfterFunc never ran")
	}
}
