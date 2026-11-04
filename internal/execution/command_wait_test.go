package execution

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Use only the test binary: no SSH connection, shell, or installed harness.
func TestExecutionCommandPipeHelper(t *testing.T) {
	args := os.Args
	for len(args) > 0 && args[0] != "--execution-pipe-helper" {
		args = args[1:]
	}
	if len(args) == 0 {
		return
	}
	args = args[1:]
	if args[0] == "descendant" {
		conn, err := net.Dial("unix", args[1])
		if err != nil {
			os.Exit(2)
		}
		_, _ = fmt.Fprintln(conn, os.Getpid())
		_ = conn.Close()
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestExecutionCommandPipeHelper$", "--", "--execution-pipe-helper", "descendant", args[1])
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		os.Exit(2)
	}
	if args[0] == "cancel" {
		time.Sleep(time.Minute)
	}
	os.Exit(0)
}

func TestExecutionCommandsBoundInheritedPipes(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runners := map[string]func(context.Context, []string) ([]byte, error){
		"SSHCommand": func(ctx context.Context, args []string) ([]byte, error) {
			return SSHCommand(ctx, append([]string{bin}, args...), nil)
		},
		"hostCommand": func(ctx context.Context, args []string) ([]byte, error) {
			return hostCommand(ctx, "", os.Environ(), bin, args...)
		},
		"hostCombined": func(ctx context.Context, args []string) ([]byte, error) {
			return hostCombined(ctx, "", os.Environ(), bin, args...)
		},
	}
	for name, run := range runners {
		for _, mode := range []string{"exit", "cancel"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				socket := filepath.Join(t.TempDir(), "ready.sock")
				listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				if err := listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				type result struct {
					out []byte
					err error
				}
				done := make(chan result, 1)
				go func() {
					out, err := run(ctx, []string{"-test.run=^TestExecutionCommandPipeHelper$", "--", "--execution-pipe-helper", mode, socket})
					done <- result{out, err}
				}()
				conn, err := listener.Accept()
				if err != nil {
					t.Fatal(err)
				}
				var pid int
				_, err = fmt.Fscan(conn, &pid)
				_ = conn.Close()
				if err != nil {
					t.Fatal(err)
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
				case got := <-done:
					if got.err == nil {
						t.Fatal("incomplete output reported as success")
					}
					if len(got.out) != 0 {
						t.Fatalf("returned incomplete output: %s", strconv.Quote(string(got.out)))
					}
					if mode == "exit" && !errors.Is(got.err, exec.ErrWaitDelay) {
						t.Fatalf("expected ErrWaitDelay, got %v", got.err)
					}
				case <-time.After(3 * time.Second):
					cancel()
					_ = descendant.Kill()
					<-done
					t.Fatal("command blocked on inherited output pipes")
				}
			})
		}
	}
}
