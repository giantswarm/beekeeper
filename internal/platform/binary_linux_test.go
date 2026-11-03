package platform

import (
	"bufio"
	"fmt"
	"io"
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

// helperReexeced is the line TestHelperReexec writes once it re-executed.
const helperReexeced = "re-executed"

// TestHelperReexec is a copy that says it runs, waits for a line on its
// stdin and then re-executes the file at its path the way a gate call does
// (Binary.Exec), saying so before it waits to be killed.
func TestHelperReexec(*testing.T) {
	switch {
	case os.Getenv("BEEKEEPER_TEST_REEXECED") == "1":
		fmt.Println(helperReexeced)
		time.Sleep(time.Minute)
	case os.Getenv("BEEKEEPER_TEST_REEXEC") == "1":
		fmt.Println(helperRunning)
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		_ = RunningBinary().Exec("BEEKEEPER_TEST_REEXECED=1")
	}
}

// copier writes executable copies of this test binary, the stand-ins of
// an installed beekeeper.
type copier struct {
	t   *testing.T
	raw []byte
}

func newCopier(t *testing.T) copier {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(self) //nolint:gosec // this test binary, copied to run as a stand-in
	if err != nil {
		t.Fatal(err)
	}
	return copier{t: t, raw: raw}
}

// install puts a new copy at path the way an install does: written beside
// it, renamed over it.
func (c copier) install(path string) {
	if err := os.WriteFile(path+".new", c.raw, 0o755); err != nil { //nolint:gosec // an executable copy in the test's own directory
		c.t.Fatal(err)
	}
	if err := os.Rename(path+".new", path); err != nil {
		c.t.Fatal(err)
	}
}

// helper is a running copy: its pid, its stdout and its stdin.
type helper struct {
	pid   int
	out   *bufio.Reader
	stdin io.WriteCloser
}

// start runs the copy at path as the helper test named, with env, and waits
// for its line: Start can return before the child's exec has swapped its
// image in, and under load /proc/<pid>/exe still names this test binary.
func (c copier) start(path, test string, env ...string) helper {
	cmd := exec.Command(path, "-test.run=^"+test+"$") //nolint:gosec // a copy the test wrote
	cmd.Env = append(os.Environ(), env...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		c.t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		c.t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	h := helper{pid: cmd.Process.Pid, out: bufio.NewReader(stdout), stdin: stdin}
	h.expect(c.t, helperRunning)
	return h
}

// expect waits for the helper's next line to be want.
func (h helper) expect(t *testing.T, want string) {
	t.Helper()
	if line, err := h.out.ReadString('\n'); err != nil || line != want+"\n" {
		t.Fatalf("the copy says %q, %v; want %q", line, err, want)
	}
}

func TestProcessBinaryReplaced(t *testing.T) {
	c := newCopier(t)
	path := filepath.Join(t.TempDir(), "beekeeper")
	c.install(path)
	h := c.start(path, "TestHelperSleep", "BEEKEEPER_TEST_SLEEP=1")
	if b := ProcessBinary(h.pid); b == nil || b.Path != path || b.Replaced() {
		t.Fatalf("the running copy reads as %+v, want its path %s, not replaced", b, path)
	}
	before := time.Now().Add(-time.Second)
	c.install(path)
	b := ProcessBinary(h.pid)
	if b == nil || b.Path != path || !b.Replaced() {
		t.Fatalf("after a rename over it the copy reads as %+v, want its path %s, replaced", b, path)
	}
	in, _ := b.Replacement()
	if in.Path != path || in.At.Before(before) || in.At.After(time.Now().Add(time.Second)) {
		t.Errorf("the replacement = %+v, want %s installed just now", in, path)
	}
}

// A call that re-executed the install renamed over its path runs that
// install: not replaced, though the process started before the install.
func TestProcessBinaryReexeced(t *testing.T) {
	c := newCopier(t)
	path := filepath.Join(t.TempDir(), "beekeeper")
	c.install(path)
	h := c.start(path, "TestHelperReexec", "BEEKEEPER_TEST_REEXEC=1")
	c.install(path)
	if !ProcessBinary(h.pid).Replaced() {
		t.Fatal("the copy waiting to re-execute does not read as replaced")
	}
	if _, err := io.WriteString(h.stdin, "go\n"); err != nil {
		t.Fatal(err)
	}
	h.expect(t, helperReexeced)
	if b := ProcessBinary(h.pid); b == nil || b.Path != path || b.Replaced() {
		t.Errorf("the re-executed copy reads as %+v, want its path %s, not replaced", b, path)
	}
}

// A process started from a second copy on the PATH runs that copy: an
// install over the first does not replace it, and one over its own copy
// is named by that copy's path.
func TestProcessBinarySecondCopyOnPath(t *testing.T) {
	c := newCopier(t)
	first, second := filepath.Join(t.TempDir(), "beekeeper"), filepath.Join(t.TempDir(), "beekeeper")
	c.install(first)
	c.install(second)
	t.Setenv("PATH", filepath.Dir(second)+string(os.PathListSeparator)+filepath.Dir(first))
	found, err := exec.LookPath("beekeeper")
	if err != nil || found != second {
		t.Fatalf("the PATH finds %q, %v; want %s", found, err, second)
	}
	h := c.start(found, "TestHelperSleep", "BEEKEEPER_TEST_SLEEP=1")
	c.install(first)
	if b := ProcessBinary(h.pid); b == nil || b.Path != second || b.Replaced() {
		t.Fatalf("after an install over the first copy, the second reads as %+v, want its path %s, not replaced", b, second)
	}
	c.install(second)
	if in, ok := ProcessBinary(h.pid).Replacement(); !ok || in.Path != second {
		t.Errorf("after an install over its own copy the replacement = %+v, %v; want %s", in, ok, second)
	}
}
