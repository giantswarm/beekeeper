//nolint:goconst // Test fixtures spell out command and wire-protocol literals for readability.
package remotebuild

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRemoteWrapperAndFollow(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("builder image shell contract requires Linux GNU coreutils")
	}
	for _, test := range []struct{ name, command, rc, output string }{
		{testExit, "printf 'before\\n'; sleep 0.1; printf 'after\\n'; exit 37", "37\n", "before\nafter\n"},
		{"subdirectory", "pwd | sed 's|.*/src/||'; exit 0", "0\n", "nested\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			script := func(s string) string { return strings.ReplaceAll(s, "/work", dir) }
			for _, d := range []string{"busy", "src", "incoming/nested", "runs/test"} {
				if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "busy/id"), []byte("test"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "runs/test/log"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, shellBash, "-c", script(wrapper), "--", "test", "nested", test.command) //nolint:gosec // fixed executable and typed arguments; test commands use owned fixtures
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("wrapper: %v %s", err, out)
			}
			rc, err := os.ReadFile(filepath.Join(dir, "runs/test/rc")) //nolint:gosec // owned test fixture
			if err != nil || string(rc) != test.rc {
				t.Fatalf("rc=%q err=%v", rc, err)
			}
			for _, offset := range []string{"0", "3"} {
				cmd = exec.CommandContext(ctx, shellBash, "-c", script(follow), "--", "test", offset) //nolint:gosec // fixed executable and typed arguments; test commands use owned fixtures
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("follow: %v %s", err, out)
				}
				want := test.output
				if offset == "3" {
					want = want[3:]
				}
				if string(out) != want {
					t.Fatalf("offset %s: %q want %q", offset, out, want)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "busy")); !os.IsNotExist(err) {
				t.Fatal("run did not release reservation")
			}
		})
	}
}

func TestRemoteWrapperInterrupt(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("builder image shell contract requires Linux GNU coreutils")
	}
	dir := t.TempDir()
	script := func(s string) string { return strings.ReplaceAll(s, "/work", dir) }
	for _, d := range []string{"busy", "src", "incoming", "runs/test"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "busy/id"), []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "runs/test/log"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// A FIFO gives deterministic child-start evidence, without a polling wait.
	fifo := filepath.Join(dir, "ready")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	ready, err := os.OpenFile(fifo, os.O_RDWR, 0o600) //nolint:gosec // owned test fixture
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ready.Close() }()
	cmd := exec.CommandContext(ctx, shellBash, "-c", script(wrapper), "--", "test", ".", "printf ready > "+quote(fifo)+"; sleep 300") //nolint:gosec // fixed executable and typed arguments; test commands use owned fixtures
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var diagnostics bytes.Buffer
	cmd.Stderr = &diagnostics
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }()
	b := make([]byte, 5)
	if _, err := ready.Read(b); err != nil {
		t.Fatal(err)
	}
	// The remote cancellation path kills the recorded process groups.
	stop := exec.CommandContext(ctx, shellBash, "-c", script(interrupt), "--", "test") //nolint:gosec // fixed executable and typed arguments; test commands use owned fixtures
	if out, err := stop.CombinedOutput(); err != nil {
		t.Fatalf("interrupt: %v %s", err, out)
	}
	_ = cmd.Wait()
	rc, err := os.ReadFile(filepath.Join(dir, "runs/test/rc")) //nolint:gosec // owned test fixture
	if err != nil || string(rc) != "143\n" {
		t.Fatalf("rc=%q err=%v diagnostics=%s", rc, err, diagnostics.String())
	}
}

func TestRemoteRunThroughShellTransport(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux builder shell contract")
	}
	root := worktree(t)
	t.Chdir(root)
	dir := t.TempDir()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\nprintf 'remote output\\n'\nexit 37\n"), 0o750); err != nil { //nolint:gosec // executable test fixture
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GOFLAGS", "")
	t.Setenv("RUSTFLAGS", "")
	t.Setenv("CARGO_ENCODED_RUSTFLAGS", "")
	for _, d := range []string{"src", "runs"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var output, diagnostics bytes.Buffer
	r := Runner{Config: validConfig(), StateDir: t.TempDir(), Stdout: &output, Stderr: &diagnostics}
	r.Command = func(ctx context.Context, in io.Reader, out io.Writer, args ...string) error {
		a := args[7:]
		switch a[0] {
		case "get":
			if a[1] == "job" {
				return nil
			}
			pods := corev1.PodList{}
			if slices.Contains(a, "-l") {
				pods.Items = []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "test-pod"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}}
			}
			return json.NewEncoder(out).Encode(pods)
		case "create", "wait", "delete":
			return nil
		case "exec":
			at := slices.Index(a, "--")
			argv := append([]string(nil), a[at+1:]...)
			for i := range argv {
				argv[i] = strings.ReplaceAll(argv[i], "/work", dir)
			}
			cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // owned transport fixture, production scripts with a temporary workspace
			cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, &diagnostics
			return cmd.Run()
		}
		t.Fatalf("unexpected call: %v", a)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	handled, rc := r.Try(ctx, []string{"go", "test", "./..."})
	if !handled || rc != 37 || output.String() != "remote output\n" {
		t.Fatalf("handled=%v rc=%d output=%q diagnostics=%s", handled, rc, output.String(), diagnostics.String())
	}
	source, err := os.ReadFile(filepath.Join(dir, "src", testMain)) //nolint:gosec // owned test fixture
	if err != nil || string(source) != "edited source" {
		t.Fatalf("source=%q err=%v", source, err)
	}
}
