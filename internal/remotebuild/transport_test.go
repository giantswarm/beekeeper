//nolint:goconst // Repeated fixture and protocol literals make the failure scenarios readable.
package remotebuild

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRemoteKubectlBoundsInheritedPipes(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux subprocess fixture")
	}
	for _, mode := range []string{"exit", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			fifo := filepath.Join(dir, "ready")
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Fatal(err)
			}
			ready, err := os.OpenFile(fifo, os.O_RDWR|syscall.O_NONBLOCK, 0o600) //nolint:gosec // owned fixture
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = ready.Close() }()
			if err := ready.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			// Like an auth/proxy helper, this descendant inherits the transport's
			// output but outlives kubectl. Readiness reports only its synthetic PID.
			fixture := "#!/bin/bash\nsleep 300 &\nprintf '%s\\n' \"$!\" > \"$FIXTURE_READY\"\nif [ \"$FIXTURE_MODE\" = exit ]; then exit 0; fi\nexec sleep 300\n"
			if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(fixture), 0o700); err != nil { //nolint:gosec // executable fixture
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("FIXTURE_READY", fifo)
			t.Setenv("FIXTURE_MODE", mode)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			runner := Runner{Config: validConfig()}
			go func() { done <- runner.kubectl(ctx, nil, new(bytes.Buffer), "get", "pods", "-o", "json") }()
			line, err := bufio.NewReader(ready).ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(line))
			if err != nil || pid <= 1 {
				t.Fatalf("invalid fixture PID %q: %v", line, err)
			}
			child, err := os.FindProcess(pid)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = child.Kill() }()
			if mode == "cancel" {
				cancel()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Error("transport reported success with an incomplete output stream")
				}
			case <-time.After(3 * time.Second):
				t.Error("kubectl output draining ignored process exit/context cancellation")
				cancel()
				_ = child.Kill()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("transport did not return after fixture cleanup")
				}
			}
		})
	}
}
