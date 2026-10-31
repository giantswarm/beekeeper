//go:build linux

package proc

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestStopIdleChecksIdentityAndEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slot.lock")
	f, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	c := exec.Command("sleep", "30")
	c.ExtraFiles = []*os.File{f}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	probe, err := os.OpenFile(filepath.Clean(path), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = probe.Close() }()
	if err := unix.Flock(int(probe.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
		t.Fatal("child did not inherit slot lock")
	}
	started, err := Started(c.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := StopIdle(c.Process.Pid, started.Add(time.Second), func() bool { return true }); err == nil {
		t.Fatal("accepted changed PID identity")
	}
	if err := StopIdle(c.Process.Pid, started, func() bool { return false }); err == nil {
		t.Fatal("ignored changed evidence")
	}
	if err := StopIdle(c.Process.Pid, started, func() bool { return true }); err != nil {
		t.Fatal(err)
	}
	if err := c.Wait(); err == nil {
		t.Fatal("idle process not killed")
	}
	if err := unix.Flock(int(probe.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("killed holder retained flock: %v", err)
	}
}

func TestStopIdlePreservesAProcessWithChildren(t *testing.T) {
	c := exec.Command("sh", "-c", "sleep 30 & echo ready; wait")
	out, err := c.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		table, _ := Read()
		if table != nil {
			for _, p := range table.Children(c.Process.Pid) {
				child, _ := os.FindProcess(p.PID)
				_ = child.Kill()
			}
		}
		_ = c.Process.Kill()
		_ = c.Wait()
	})
	if _, err := bufio.NewReader(out).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	started, err := Started(c.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := StopIdle(c.Process.Pid, started, func() bool { return true }); err == nil {
		t.Fatal("killed a wrapper with a child")
	}
}
