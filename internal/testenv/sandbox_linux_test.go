//go:build linux

package testenv_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/testenv"
)

// The test binary doubles as the program a sandbox runs: --sandbox <root>
// enters the sandbox of root and runs the mode that follows inside it.
func TestMain(m *testing.M) {
	if len(os.Args) > 3 && os.Args[1] == "--sandbox" {
		if err := testenv.EnterSandbox(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		// Nothing of the outer environment follows the program inside.
		if err := syscall.Exec("/work/bin/fixture", append([]string{"fixture"}, os.Args[3:]...), []string{"PATH=/work/bin", "HOME=/work/home"}); err != nil { //nolint:gosec // this test binary
			panic(err)
		}
	}
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "--") {
		var err error
		switch os.Args[1] {
		case "--isolated":
			err = isolated()
		case "--linger":
			c := exec.Command("/work/bin/fixture", "--heartbeat")
			if err = c.Start(); err == nil {
				err = c.Wait()
			}
		case "--heartbeat":
			for {
				if err = os.WriteFile("/work/heartbeat", []byte(time.Now().String()), 0o600); err != nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
		default:
			err = fmt.Errorf("unknown fixture mode %q", os.Args[1])
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// isolated checks the sandbox from inside: no network, no host homes, the
// system read-only, the workspace writable.
func isolated() error {
	if conn, err := net.DialTimeout("tcp", "192.0.2.1:443", 100*time.Millisecond); err == nil {
		_ = conn.Close()
		return errors.New("outbound network is available")
	}
	if _, err := os.Stat("/home"); !os.IsNotExist(err) {
		return fmt.Errorf("host homes exposed: %v", err)
	}
	if err := os.WriteFile("/usr/sandbox-probe", nil, 0o600); err == nil {
		return errors.New("/usr is writable")
	}
	if err := os.WriteFile("/work/probe", []byte("ok"), 0o600); err != nil {
		return fmt.Errorf("the workspace is not writable: %w", err)
	}
	if wd, err := os.Getwd(); err != nil || wd != "/work/repo" {
		return fmt.Errorf("the sandbox starts in %q (%v), not /work/repo", wd, err)
	}
	return nil
}

// TestSandbox runs the sandbox the scenario harness enters (internal/
// scenario) for itself: what a program inside sees, and that nothing it
// started survives an interrupted run while another run stays untouched.
// It was the interrupted case and the isolation checks of the retired
// TestSupervisorWorkflow.
func TestSandbox(t *testing.T) {
	if testing.Short() {
		t.Skip("enters Linux namespaces")
	}
	if out, err := exec.Command("unshare", "-Urmpnf", "--kill-child", "true").CombinedOutput(); err != nil {
		t.Skipf("Linux user/mount/PID/network namespaces required: %v: %s", err, out)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self) //nolint:gosec // this test executable
	if err != nil {
		t.Fatal(err)
	}
	// newRoot is a run's root: /work with this test binary as its program.
	newRoot := func(t *testing.T) string {
		t.Helper()
		root := t.TempDir()
		for _, dir := range []string{"bin", "repo", "home"} {
			if err := os.MkdirAll(filepath.Join(root, "work", dir), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(root, "work", "bin", "fixture"), data, 0o700); err != nil { //nolint:gosec // the sandbox's program
			t.Fatal(err)
		}
		return root
	}
	t.Run("isolated", func(t *testing.T) {
		t.Parallel()
		root := newRoot(t)
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		c := exec.CommandContext(ctx, "unshare", "-Urmpnf", "--kill-child", self, "--sandbox", root, "--isolated") //nolint:gosec // this test binary in its own sandbox
		c.Env = []string{"PATH=" + os.Getenv("PATH"), "GH_TOKEN=unavailable-fixture-credential"}
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("inside the sandbox: %v\n%s", err, out)
		}
		if b, err := os.ReadFile(filepath.Join(root, "work", "probe")); err != nil || string(b) != "ok" { //nolint:gosec // the run's own workspace
			t.Fatalf("the workspace write did not reach the run's root: %q, %v", b, err)
		}
	})
	t.Run("interrupted", func(t *testing.T) {
		t.Parallel()
		root := newRoot(t)
		sentinel := filepath.Join(t.TempDir(), "other-run")
		if err := os.WriteFile(sentinel, []byte("untouched"), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		c := exec.CommandContext(ctx, "unshare", "-Urmpnf", "--kill-child", self, "--sandbox", root, "--linger") //nolint:gosec // this test binary in its own sandbox
		c.Env = []string{"PATH=" + os.Getenv("PATH")}
		var output bytes.Buffer
		c.Stdout, c.Stderr = &output, &output
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cancel(); _ = c.Wait() })
		heartbeat := filepath.Join(root, "work", "heartbeat")
		deadline := time.Now().Add(15 * time.Second)
		for {
			if b, _ := os.ReadFile(heartbeat); len(b) > 0 { //nolint:gosec // the run's own workspace
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("no heartbeat from inside the sandbox: %s", output.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
		if err := c.Wait(); err == nil {
			t.Fatal("the interrupted run succeeded")
		}
		before, err := os.ReadFile(heartbeat) //nolint:gosec // the run's own workspace
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		after, err := os.ReadFile(heartbeat) //nolint:gosec // the run's own workspace
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("a descendant survived the namespace's teardown")
		}
		if b, err := os.ReadFile(sentinel); err != nil || string(b) != "untouched" { //nolint:gosec // the test's own file
			t.Fatal("another run was touched")
		}
	})
}
