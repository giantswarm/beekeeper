package remotebuild

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRemoteKubectlBoundsInheritedPipes(t *testing.T) {
	for _, mode := range []string{"exit", "cancel"} {
		for _, stream := range []string{"stdout", "stderr"} {
			t.Run(mode+"/"+stream, func(t *testing.T) {
				dir := t.TempDir()
				pidFile := filepath.Join(dir, "pid")
				// Keep only the selected output descriptor open in the descendant.
				redirect := "2>/dev/null"
				if stream == "stderr" {
					redirect = ">/dev/null"
				}
				script := "#!/bin/sh\nsleep 60 " + redirect + " &\nprintf '%s' \"$!\" > \"$TEST_KUBECTL_PID\"\n"
				if mode == "cancel" {
					script += "exec sleep 60\n"
				} else {
					script += "exit 0\n"
				}
				if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o750); err != nil { //nolint:gosec // executable test fixture
					t.Fatal(err)
				}
				t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
				t.Setenv("TEST_KUBECTL_PID", pidFile)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				result := make(chan error, 1)
				go func() { result <- (Runner{}).kubectl(ctx, nil, io.Discard, "get", "pods") }()

				// Wait for the descendant before cancelling so both cases exercise
				// inherited pipes, rather than cancellation before process startup.
				deadline := time.Now().Add(2 * time.Second)
				var pid int
				for time.Now().Before(deadline) {
					data, err := os.ReadFile(pidFile) //nolint:gosec // owned test fixture
					if err == nil {
						pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
						if pid > 0 {
							break
						}
					}
					time.Sleep(10 * time.Millisecond)
				}
				if pid <= 0 {
					t.Fatal("fake kubectl did not report its descendant")
				}
				descendant, err := os.FindProcess(pid)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = descendant.Kill() }()
				if mode == "cancel" {
					cancel()
				}
				select {
				case err := <-result:
					if err == nil {
						t.Fatal("incomplete output must not report success")
					}
					if mode == "exit" && !errors.Is(err, exec.ErrWaitDelay) {
						t.Fatalf("got %v, want exec.ErrWaitDelay", err)
					}
				case <-time.After(3 * time.Second):
					cancel()
					_ = descendant.Kill()
					<-result
					t.Fatal("kubectl waited for inherited output descriptors")
				}
			})
		}
	}
}
