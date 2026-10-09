package platform

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestBinaryReplaced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "beekeeper")
	write := func(p string, mode os.FileMode) {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(path, 0o755)
	self, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// The running binary stays open while it runs: keep its inode allocated,
	// or a filesystem that reuses it at once gives the replacement the same.
	if err := os.Link(path, filepath.Join(dir, "running")); err != nil {
		t.Fatal(err)
	}
	b := &Binary{Path: path, self: self}
	if b.Replaced() {
		t.Error("the running file reads as replaced")
	}
	write(path+".new", 0o644)
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
	if b.Replaced() {
		t.Error("a file that is not executable reads as a replacement")
	}
	write(path+".new", 0o755)
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
	if !b.Replaced() {
		t.Error("a new binary renamed over the path is not noticed")
	}
	var none *Binary
	if none.Replaced() {
		t.Error("an unknown binary reads as replaced")
	}
	if RunningBinary() == nil {
		t.Error("the test binary is not found")
	}
}

func TestProcessBinary(t *testing.T) {
	b := ProcessBinary(os.Getpid())
	if b == nil {
		t.Fatal("the test process's binary is not found")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if b.Path != self {
		t.Errorf("Path = %q, want %q", b.Path, self)
	}
	if b.Replaced() {
		t.Error("the running test binary reads as replaced")
	}
	if ProcessBinary(-1) != nil {
		t.Error("a process that does not exist has a binary")
	}
}

// helperRunning is the line TestHelperSleep writes once the copy runs.
const helperRunning = "running"

// TestHelperSleep is the process TestProcessBinaryReplaced runs: a copy of
// this test binary that says it runs and waits to be killed.
func TestHelperSleep(*testing.T) {
	if os.Getenv("BEEKEEPER_TEST_SLEEP") == "1" {
		fmt.Println(helperRunning)
		time.Sleep(time.Minute)
	}
}

func TestProcessBinaryReplaced(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(self) //nolint:gosec // this test binary, copied to run as a stand-in
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "beekeeper")
	write := func(p string) {
		if err := os.WriteFile(p, raw, 0o755); err != nil { //nolint:gosec // an executable copy in the test's own directory
			t.Fatal(err)
		}
	}
	write(path)
	cmd := exec.Command(path, "-test.run=^TestHelperSleep$") //nolint:gosec // the copy written above
	cmd.Env = append(os.Environ(), "BEEKEEPER_TEST_SLEEP=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	// Start can return before the child's exec has swapped its image in:
	// under load /proc/<pid>/exe still names this test binary. The copy's
	// own line says it runs.
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != helperRunning+"\n" {
		t.Fatalf("the copy does not say it runs: %q, %v", line, err)
	}
	if b := ProcessBinary(cmd.Process.Pid); b == nil || b.Path != path || b.Replaced() {
		t.Fatalf("the running copy reads as %+v, want its path %s, not replaced", b, path)
	}
	write(path + ".new")
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
	b := ProcessBinary(cmd.Process.Pid)
	if b == nil || b.Path != path || !b.Replaced() {
		t.Errorf("after a rename over it the copy reads as %+v, want its path %s, replaced", b, path)
	}
}
