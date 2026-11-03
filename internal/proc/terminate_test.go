package proc

import (
	"os"
	"os/exec"
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
