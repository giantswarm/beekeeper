package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestTerminateMatchingProtectsReusedPID(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("requires Linux proc")
	}
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	handle, err := openProcessHandle(cmd.Process.Pid)
	if err != nil {
		t.Skipf("requires stable process handles: %v", err)
	}
	_ = handle.close()
	start, err := Started(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	p := &Process{PID: cmd.Process.Pid, Start: start.Add(-time.Second)}
	if got := TerminateMatching([]*Process{p}, 0, func(*Process) bool { return true }); len(got) != 0 {
		t.Fatalf("stale identity signalled: %v", got)
	}
	p.Start = start
	if got := TerminateMatching([]*Process{p}, 0, func(*Process) bool { return false }); len(got) != 0 {
		t.Fatalf("failed recheck signalled: %v", got)
	}
	if got := TerminateMatching([]*Process{p}, 0, func(*Process) bool { return true }); len(got) != 1 {
		t.Fatalf("matching server not signalled: %v", got)
	}
}

// generationHandle models a handle bound to a process generation, independently
// of the process currently occupying its numeric PID.
type generationHandle struct {
	generation int
	current    *int
	signals    *[]int
	closed     int
}

func (h *generationHandle) signal(sig syscall.Signal) error {
	if h.generation != *h.current {
		return os.ErrProcessDone
	}
	if sig != 0 {
		*h.signals = append(*h.signals, h.generation)
	}
	return nil
}

func (h *generationHandle) close() error { h.closed++; return nil }

func TestTerminateMatchingPinsIdentityBeforeChecks(t *testing.T) {
	for _, phase := range []int{1, 2} {
		t.Run(fmt.Sprintf("match_%d", phase), func(t *testing.T) {
			current := 1
			var signals []int
			var handles []*generationHandle
			start := time.Unix(100, 0)
			p := &Process{PID: 42, Start: start}
			checks := 0
			ops := terminationOS{
				open: func(int) (processHandle, error) {
					h := &generationHandle{generation: current, current: &current, signals: &signals}
					handles = append(handles, h)
					return h, nil
				},
				started: func(int) (time.Time, error) { return start, nil },
			}
			got := terminateMatching([]*Process{p}, 0, func(*Process) bool {
				checks++
				if checks == phase {
					current++
				}
				return true
			}, ops)
			wantSent := phase - 1
			if len(got) != wantSent {
				t.Fatalf("TERM results = %v, want %d", got, wantSent)
			}
			for _, generation := range signals {
				if generation != 1 {
					t.Fatalf("signalled replacement generation %d", generation)
				}
			}
			if len(signals) != wantSent {
				t.Fatalf("signals = %v, want %d original-process signals", signals, wantSent)
			}
			if len(handles) != 1 || handles[0].closed != 1 {
				t.Fatalf("handles not released exactly once: %+v", handles)
			}
		})
	}
}

// The ways terminateMatching can be refused, one subtest each.
const (
	failOpen      = "open"
	failStart     = "start"
	failStale     = "stale"
	failMatch     = "match"
	failSignal    = "signal"
	failProtected = "protected"
)

func TestTerminateMatchingFailsClosed(t *testing.T) {
	for _, failure := range []string{failOpen, failStart, failStale, failMatch, failSignal, failProtected} {
		t.Run(failure, func(t *testing.T) {
			current := 1
			var signals []int
			h := &generationHandle{generation: 1, current: &current, signals: &signals}
			start := time.Unix(100, 0)
			p := &Process{PID: 42, Start: start}
			opened, checked := 0, 0
			if failure == failProtected {
				p.PID = 1
			}
			ops := terminationOS{
				open: func(int) (processHandle, error) {
					opened++
					if failure == failOpen {
						return nil, errors.New("pidfds unavailable")
					}
					return h, nil
				},
				started: func(int) (time.Time, error) {
					checked++
					if failure == failStart {
						return time.Time{}, os.ErrNotExist
					}
					if failure == failStale {
						return start.Add(time.Second), nil
					}
					return start, nil
				},
			}
			got := terminateMatching([]*Process{p}, 0, func(*Process) bool {
				if failure == failSignal {
					current++
				}
				return failure != failMatch
			}, ops)
			if len(got) != 0 || len(signals) != 0 {
				t.Fatalf("unexpected signals: %v, %v", got, signals)
			}
			wantClosed := 1
			if failure == failOpen || failure == failProtected {
				wantClosed = 0
			}
			if h.closed != wantClosed {
				t.Fatalf("closed %d times, want %d", h.closed, wantClosed)
			}
			if failure == failOpen && checked != 0 {
				t.Fatal("read evidence without a stable handle")
			}
			if failure == failProtected && opened != 0 {
				t.Fatal("opened protected PID")
			}
		})
	}
}
