package secret

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A process that does not descend from git gets no answer on the socket,
// though it runs as the same user and found the path.
func TestAskpassAnswersGitsProcessesOnly(t *testing.T) {
	dir, err := askpassDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, askpassSocket)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	other := exec.Command("sleep", "30")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
	done := make(chan struct{})
	go func() { defer close(done); serveAskpass(l, other.Process.Pid, "oauth2", "planted-value") }()
	var b strings.Builder
	if err := Askpass(sock, "Password for 'https://h': ", &b); err == nil || b.Len() != 0 {
		t.Errorf("a stranger got an answer (%d bytes): %v", b.Len(), err)
	}
	_ = l.Close()
	<-done

	// this process's own pid: the answer comes
	l, err = net.Listen("unix", sock+"2")
	if err != nil {
		t.Fatal(err)
	}
	done = make(chan struct{})
	go func() { defer close(done); serveAskpass(l, os.Getpid(), "oauth2", "planted-value") }()
	b.Reset()
	if err := Askpass(sock+"2", "Username for 'https://h': ", &b); err != nil || b.String() != "oauth2\n" {
		t.Errorf("git's own process got %q, %v", b.String(), err)
	}
	_ = l.Close()
	<-done
}

// A TMPDIR too deep for a Unix socket, as a build slot's can be, still leaves
// the helper a directory that binds, private to this user.
func TestAskpassDirBindsUnderADeepTMPDIR(t *testing.T) {
	deep := filepath.Join(t.TempDir(), strings.Repeat("d", 60), strings.Repeat("e", 60))
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", deep)
	dir, err := askpassDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if strings.HasPrefix(dir, deep) {
		t.Fatalf("askpass directory stayed under the deep TMPDIR: %s", dir)
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("askpass directory = %v, %v, want 0700", info, err)
	}
	l, err := net.Listen("unix", filepath.Join(dir, askpassSocket))
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
	if entries, err := os.ReadDir(deep); err != nil || len(entries) != 0 {
		t.Fatalf("the deep TMPDIR keeps %v, %v", entries, err)
	}
}
