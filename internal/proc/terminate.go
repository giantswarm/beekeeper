package proc

import (
	"os"
	"slices"
	"syscall"
	"time"
)

// Terminate sends the processes pids SIGTERM, and SIGKILL to those still
// alive after grace.
func Terminate(pids []int, grace time.Duration) {
	signal := func(sig syscall.Signal) {
		for _, pid := range pids {
			if p, err := os.FindProcess(pid); err == nil {
				_ = p.Signal(sig)
			}
		}
	}
	signal(syscall.SIGTERM)
	for deadline := time.Now().Add(grace); time.Now().Before(deadline) && slices.ContainsFunc(pids, Alive); {
		time.Sleep(100 * time.Millisecond)
	}
	signal(syscall.SIGKILL)
}

// TerminateMatching rechecks the snapshot's start time and match before
// each signal, including escalation, so an exited PID reused by another
// process is never signalled. It returns the PIDs successfully sent TERM.
// Without Linux pidfds it leaves the processes alone.
func TerminateMatching(ps []*Process, grace time.Duration, match func(*Process) bool) []int {
	return terminateMatching(ps, grace, match, terminationIO{started: Started, open: openMatchingProcess})
}

type processSignals struct {
	signal func(syscall.Signal) error
	close  func()
}

type terminationIO struct {
	started func(int) (time.Time, error)
	open    func(int) (processSignals, error)
}

func terminateMatching(ps []*Process, grace time.Duration, match func(*Process) bool, files terminationIO) []int {
	signal := func(p *Process, sig syscall.Signal) bool {
		if p.PID <= 1 {
			return false
		}
		// Pin the target before inspecting the numeric PID. Exit/reuse
		// during either recheck must not redirect the subsequent signal.
		live, err := files.open(p.PID)
		if err != nil {
			return false
		}
		defer live.close()
		start, err := files.started(p.PID)
		if err != nil || !start.Equal(p.Start) || !match(p) {
			return false
		}
		return live.signal(sig) == nil
	}
	var sent []int
	for _, p := range ps {
		if signal(p, syscall.SIGTERM) {
			sent = append(sent, p.PID)
		}
	}
	for deadline := time.Now().Add(grace); time.Now().Before(deadline) && slices.ContainsFunc(sent, Alive); {
		time.Sleep(100 * time.Millisecond)
	}
	for _, p := range ps {
		if slices.Contains(sent, p.PID) {
			signal(p, syscall.SIGKILL)
		}
	}
	return sent
}
