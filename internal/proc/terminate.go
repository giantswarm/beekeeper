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

// processHandle pins one process identity even after its PID is reused.
type processHandle interface {
	signal(syscall.Signal) error
	close() error
}

type terminationOS struct {
	open    func(int) (processHandle, error)
	started func(int) (time.Time, error)
}

// TerminateMatching pins a stable process handle before checking the snapshot's
// start time and match. It rechecks both before escalation and signals only the
// pinned handle. If a stable handle is unavailable, it fails closed. It returns
// the PIDs successfully sent TERM.
func TerminateMatching(ps []*Process, grace time.Duration, match func(*Process) bool) []int {
	return terminateMatching(ps, grace, match, terminationOS{openProcessHandle, Started})
}

func terminateMatching(ps []*Process, grace time.Duration, match func(*Process) bool, ops terminationOS) []int {
	type target struct {
		process *Process
		handle  processHandle
	}
	var targets []target
	var handles []processHandle
	defer func() {
		for _, handle := range handles {
			_ = handle.close()
		}
	}()
	signal := func(t target, sig syscall.Signal) bool {
		start, err := ops.started(t.process.PID)
		return err == nil && start.Equal(t.process.Start) && match(t.process) && t.handle.signal(sig) == nil
	}
	var sent []int
	for _, p := range ps {
		if p.PID <= 1 {
			continue
		}
		handle, err := ops.open(p.PID)
		if err != nil {
			continue
		}
		handles = append(handles, handle)
		t := target{p, handle}
		if !signal(t, syscall.SIGTERM) {
			continue
		}
		targets = append(targets, t)
		sent = append(sent, p.PID)
	}
	for deadline := time.Now().Add(grace); time.Now().Before(deadline) && slices.ContainsFunc(targets, func(t target) bool { return t.handle.signal(0) == nil }); {
		time.Sleep(100 * time.Millisecond)
	}
	for _, t := range targets {
		signal(t, syscall.SIGKILL)
	}
	return sent
}
