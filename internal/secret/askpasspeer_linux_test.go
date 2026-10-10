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
	sock := filepath.Join(t.TempDir(), "s")
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
