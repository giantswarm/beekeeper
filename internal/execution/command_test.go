package execution

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"
)

const (
	commandPipeFixture = "--execution-command-pipe-fixture"
	commandPipeCancel  = "cancel"
)

func TestExecutionCommandsBoundInheritedPipes(t *testing.T) {
	if runtime.GOOS != messageTestLinux {
		t.Skip("fixture uses Linux abstract Unix sockets")
	}
	runners := map[string]CommandRunner{
		"SSH": SSHCommand,
		"probe stdout": func(ctx context.Context, argv []string, _ []byte) ([]byte, error) {
			return hostCommand(ctx, "", os.Environ(), argv[0], argv[1:]...)
		},
		"probe combined": func(ctx context.Context, argv []string, _ []byte) ([]byte, error) {
			return hostCombined(ctx, "", os.Environ(), argv[0], argv[1:]...)
		},
	}
	for name, run := range runners {
		for _, mode := range []string{"exit", commandPipeCancel} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				listener, err := net.ListenUnix("unix", &net.UnixAddr{Net: "unix", Name: "\x00beekeeper-command-test-" + newID()})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = listener.Close() }()
				if err := listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				argv := []string{os.Args[0], "-test.run=^TestExecutionCommandPipeHelper$", "--", commandPipeFixture, mode, listener.Addr().String()}
				done := make(chan error, 1)
				go func() { _, err := run(ctx, argv, nil); done <- err }()
				ready, err := listener.AcceptUnix()
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = ready.Close() }()
				if err := ready.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
					t.Fatal(err)
				}
				pidLine, err := bufio.NewReader(ready).ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				pid, err := strconv.Atoi(pidLine[:len(pidLine)-1])
				if err != nil || pid <= 0 {
					t.Fatalf("invalid fixture child PID: %q, %v", pidLine, err)
				}
				child, err := os.FindProcess(pid)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = child.Kill() }()
				if mode == commandPipeCancel {
					cancel()
				}
				select {
				case err := <-done:
					if err == nil || mode == "exit" && !errors.Is(err, exec.ErrWaitDelay) {
						t.Fatalf("incomplete output must return an error: %v", err)
					}
				case <-time.After(3 * time.Second):
					// Release the fixture's inherited descriptors before failing,
					// so the pre-fix reproducer leaves no stuck command goroutine.
					_ = child.Kill()
					cancel()
					exitErr := <-done
					t.Fatalf("command waited for inherited pipes after exit or cancellation: %v", exitErr)
				}
			})
		}
	}
}

// The direct child and its descendant share stdout/stderr just as SSH proxy
// commands and wrappers can. Only the direct child belongs to CommandContext.
func TestExecutionCommandPipeHelper(t *testing.T) {
	i := slices.Index(os.Args, commandPipeFixture)
	if i < 0 {
		return
	}
	mode := os.Args[i+1]
	if mode == "hold" {
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestExecutionCommandPipeHelper$", "--", commandPipeFixture, "hold") //nolint:gosec // only this test binary, fixed helper arguments
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		os.Exit(2)
	}
	ready, err := net.Dial("unix", os.Args[i+2]) //nolint:gosec // parent test supplies a local abstract Unix socket, never a network endpoint
	if err != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		os.Exit(2)
	}
	_, _ = fmt.Fprintln(ready, child.Process.Pid)
	_ = ready.Close()
	if mode == commandPipeCancel {
		time.Sleep(30 * time.Second)
	}
	os.Exit(0)
}
