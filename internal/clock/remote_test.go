//go:build scenario

package clock

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var start = time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)

// serve starts a world's clock at start on a fresh socket.
func serve(t *testing.T) (*Server, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "clock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "clock.sock")
	s := NewServer(start)
	if err := s.Serve(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

// connect is one process's clock on the world at path.
func connect(t *testing.T, path string) *remote {
	t.Helper()
	r, err := dial(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.conn.Close() })
	return r
}

// near fails unless got is within a second after want: the world's time
// runs on between the two readings.
func near(t *testing.T, what string, got, want time.Time) {
	t.Helper()
	if got.Before(want) || got.After(want.Add(time.Second)) {
		t.Fatalf("%s: %s, want %s", what, got, want)
	}
}

func advance(t *testing.T, s *Server, d time.Duration) Advanced {
	t.Helper()
	a, err := s.Advance(d)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// received says whether c holds a time now, without waiting: an advance
// returns only once what it made due has fired.
func received(c <-chan time.Time) (time.Time, bool) {
	select {
	case at := <-c:
		return at, true
	default:
		return time.Time{}, false
	}
}

func TestEveryProcessReadsTheWorldsTime(t *testing.T) {
	s, path := serve(t)
	first, second := connect(t, path), connect(t, path)
	near(t, "first process", first.now(), start)
	near(t, "second process", second.now(), start)
	a := advance(t, s, time.Hour)
	if a.Processes != 2 {
		t.Fatalf("advance acknowledged by %d processes, want 2", a.Processes)
	}
	near(t, "advance's time", a.Now, start.Add(time.Hour))
	near(t, "first process after the advance", first.now(), start.Add(time.Hour))
	near(t, "second process after the advance", second.now(), start.Add(time.Hour))
	near(t, "the world", s.Now(), start.Add(time.Hour))
	near(t, "a process connecting later", connect(t, path).now(), start.Add(time.Hour))
	if got := len(s.Processes()); got != 3 {
		t.Fatalf("%d processes connected, want 3: %q", got, s.Processes())
	}
}

func TestAdvanceFiresWhatIsDue(t *testing.T) {
	s, path := serve(t)
	r := connect(t, path)
	timer := r.newTimer(30*time.Minute, nil)
	if a := advance(t, s, 29*time.Minute); a.Fired != 0 {
		t.Fatalf("a timer fired a minute early: %+v", a)
	}
	if at, ok := received(timer.C); ok {
		t.Fatalf("a timer fired a minute early at %s", at)
	}
	if a := advance(t, s, time.Minute); a.Fired != 1 {
		t.Fatalf("the due timer did not fire: %+v", a)
	}
	at, ok := received(timer.C)
	if !ok {
		t.Fatal("the advance returned before the due timer fired")
	}
	near(t, "the timer's time", at, start.Add(30*time.Minute))
	if timer.Stop() {
		t.Fatal("Stop of a fired timer returned true")
	}
}

func TestTickerDropsTheTicksItMissed(t *testing.T) {
	s, path := serve(t)
	r := connect(t, path)
	ticker := r.newTicker(time.Minute)
	defer ticker.Stop()
	if a := advance(t, s, 5*time.Minute+30*time.Second); a.Fired != 1 {
		t.Fatalf("five missed ticks fired %d times, want once", a.Fired)
	}
	if _, ok := received(ticker.C); !ok {
		t.Fatal("no tick after five periods")
	}
	if _, ok := received(ticker.C); ok {
		t.Fatal("a second tick for the same advance")
	}
	// The next tick stays on the period: at 6m, not 6m30s.
	if a := advance(t, s, 30*time.Second); a.Fired != 1 {
		t.Fatalf("the tick at 6m did not fire: %+v", a)
	}
	if _, ok := received(ticker.C); !ok {
		t.Fatal("no tick at 6m")
	}
	ticker.Reset(time.Hour)
	if a := advance(t, s, 59*time.Minute); a.Fired != 0 {
		t.Fatalf("a reset ticker kept its old period: %+v", a)
	}
}

func TestSleepWakesOnAdvance(t *testing.T) {
	s, path := serve(t)
	r := connect(t, path)
	woke := make(chan struct{})
	go func() {
		r.sleep(time.Hour)
		close(woke)
	}()
	// Wait until the sleep is on the clock.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		r.mu.Lock()
		n := len(r.waiters)
		r.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the sleep never waited on the clock")
		}
	}
	advance(t, s, time.Hour)
	select {
	case <-woke:
	case <-time.After(10 * time.Second):
		t.Fatal("an hour's sleep did not wake when the world advanced an hour")
	}
}

func TestStopAndReset(t *testing.T) {
	s, path := serve(t)
	r := connect(t, path)
	timer := r.newTimer(10*time.Minute, nil)
	if !timer.Stop() {
		t.Fatal("Stop of a pending timer returned false")
	}
	if a := advance(t, s, time.Hour); a.Fired != 0 {
		t.Fatalf("a stopped timer fired: %+v", a)
	}
	if timer.Reset(10 * time.Minute) {
		t.Fatal("Reset of a stopped timer returned true")
	}
	advance(t, s, 10*time.Minute)
	if _, ok := received(timer.C); !ok {
		t.Fatal("a reset timer did not fire")
	}
	// A time delivered but not received is gone after Reset.
	timer.Reset(time.Minute)
	advance(t, s, time.Minute)
	timer.Reset(time.Hour)
	if at, ok := received(timer.C); ok {
		t.Fatalf("a stale time %s after Reset", at)
	}
	var called atomic.Bool
	done := make(chan struct{})
	r.newTimer(time.Minute, func() { called.Store(true); close(done) })
	advance(t, s, time.Minute)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("AfterFunc's function never ran")
	}
	if !called.Load() {
		t.Fatal("AfterFunc's function did not run")
	}
}

// The world's time runs as the system's between advances: a timer fires
// without one.
func TestTheWorldsTimeRuns(t *testing.T) {
	_, path := serve(t)
	r := connect(t, path)
	timer := r.newTimer(20*time.Millisecond, nil)
	select {
	case at := <-timer.C:
		near(t, "a short timer", at, start.Add(20*time.Millisecond))
	case <-time.After(10 * time.Second):
		t.Fatal("a 20ms timer did not fire without an advance")
	}
	if r.now().Before(start.Add(20 * time.Millisecond)) {
		t.Fatalf("the world's time stood still: %s", r.now())
	}
}

func TestAdvanceDoesNotWaitForAProcessThatEnded(t *testing.T) {
	s, path := serve(t)
	r := connect(t, path)
	_ = r.conn.Close()
	for deadline := time.Now().Add(10 * time.Second); len(s.Processes()) > 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("an ended process stayed connected")
		}
	}
	if a := advance(t, s, time.Minute); a.Processes != 0 {
		t.Fatalf("an ended process acknowledged: %+v", a)
	}
}

func TestAdvanceNamesAProcessThatDoesNotAnswer(t *testing.T) {
	old := ackTimeout
	ackTimeout = 200 * time.Millisecond
	t.Cleanup(func() { ackTimeout = old })
	s, path := serve(t)
	answering := connect(t, path)
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := json.NewEncoder(c).Encode(hello{PID: 4242, Command: "stuck"}); err != nil {
		t.Fatal(err)
	}
	var first offset
	if err := json.NewDecoder(c).Decode(&first); err != nil {
		t.Fatal(err)
	}
	a, err := s.Advance(time.Minute)
	if err == nil {
		t.Fatal("an advance nobody answered succeeded")
	}
	if want := "pid 4242 (stuck)"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not name %s", err, want)
	}
	if a.Processes != 1 {
		t.Fatalf("the answering process was not counted: %+v", a)
	}
	near(t, "the answering process", answering.now(), start.Add(time.Minute))
	if _, err := s.Advance(-time.Second); err == nil {
		t.Fatal("the world's time went back")
	}
}

// A process notes each timer, ticker or sleep it arms, so that the world
// sees when the desk has gone quiet; re-arming on an advance or a tick is
// none of the process's doing and is not noted.
func TestArmsAreNoted(t *testing.T) {
	s, path := serve(t)
	r := connect(t, path)
	if !s.LastArmed().IsZero() {
		t.Fatalf("armed before anything was: %s", s.LastArmed())
	}
	noted := func(after time.Time) time.Time {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
			if at := s.LastArmed(); at.After(after) {
				return at
			}
			if time.Now().After(deadline) {
				t.Fatal("the arm was never noted")
			}
		}
	}
	r.newTimer(time.Hour, nil)
	timer := noted(time.Time{})
	advance(t, s, time.Minute)
	time.Sleep(50 * time.Millisecond)
	if at := s.LastArmed(); !at.Equal(timer) {
		t.Fatalf("re-arming on an advance was noted at %s", at)
	}
	ticker := r.newTicker(time.Minute)
	defer ticker.Stop()
	ticked := noted(timer)
	advance(t, s, 5*time.Minute)
	time.Sleep(50 * time.Millisecond)
	if at := s.LastArmed(); !at.Equal(ticked) {
		t.Fatalf("a tick was noted at %s", at)
	}
}
