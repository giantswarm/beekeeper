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
func TerminateMatching(ps []*Process, grace time.Duration, match func(*Process) bool) []int {
	signal := func(p *Process, sig syscall.Signal) bool {
		start, err := Started(p.PID)
		if err != nil || p.PID <= 1 || !start.Equal(p.Start) || !match(p) {
			return false
		}
		live, err := os.FindProcess(p.PID)
		return err == nil && live.Signal(sig) == nil
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
