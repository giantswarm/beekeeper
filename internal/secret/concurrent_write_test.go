package secret_test

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

var (
	writerFile = flag.String("secret-writer-file", "", "synthetic child destination")
	writerKey  = flag.String("secret-writer-key", "", "synthetic child key")
)

func TestConcurrentSOPSWriterProcess(t *testing.T) {
	if *writerFile == "" {
		t.Skip("subprocess helper")
	}
	tools := secrettest.New(map[string]string{vaultRef: password})
	o := ops(tools)
	run := o.Run
	o.Run = func(ctx context.Context, dir string, env []string, in io.Reader, name string, args ...string) ([]byte, error) {
		if name == "sops" && slices.Contains(args, "encrypt") {
			fmt.Println("ready")
			if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
				return nil, err
			}
		}
		return run(ctx, dir, env, in, name, args...)
	}
	_, err := o.CopyValue(context.Background(), secret.Ref{Op: vaultRef}, secret.Ref{File: *writerFile, Path: *writerKey})
	if errors.Is(err, secret.ErrSOPSConflict) {
		fmt.Println("conflict")
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("written")
}

func TestConcurrentSOPSWritesReportConflict(t *testing.T) {
	for _, initial := range []string{"encrypted", "skeleton", "absent"} {
		t.Run(initial, func(t *testing.T) {
			cache := t.TempDir()
			t.Setenv("XDG_CACHE_HOME", cache)
			t.Setenv("LOCALAPPDATA", cache)
			t.Setenv("HOME", cache)
			dir := t.TempDir()
			file := filepath.Join(dir, "target.sops.yaml")
			if err := os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte("creation_rules:\n  - path_regex: '.*'\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if initial != "absent" {
				raw := secrettest.Encrypt("original: keep\n")
				if initial == "skeleton" {
					raw = []byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: example\nstringData: {}\n")
				}
				if err := os.WriteFile(file, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			type child struct {
				cmd *exec.Cmd
				in  io.WriteCloser
				out *bufio.Reader
			}
			start := func(key string) child {
				cmd := exec.CommandContext(ctx, exe, "-test.run=^TestConcurrentSOPSWriterProcess$", "-test.timeout=15s", "-secret-writer-file="+file, "-secret-writer-key="+key) //nolint:gosec // this test binary, synthetic flags
				cmd.Stderr = os.Stderr
				input, err := cmd.StdinPipe()
				if err != nil {
					t.Fatal(err)
				}
				output, err := cmd.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				reader := bufio.NewReader(output)
				if line, err := reader.ReadString('\n'); err != nil || line != "ready\n" {
					t.Fatalf("writer did not reach encryption: %q, %v", line, err)
				}
				return child{cmd: cmd, in: input, out: reader}
			}
			first, second := start("first"), start("second")
			finish := func(c child, want string) {
				if _, err := io.WriteString(c.in, "continue\n"); err != nil {
					t.Fatal(err)
				}
				if err := c.in.Close(); err != nil {
					t.Fatal(err)
				}
				output, err := io.ReadAll(c.out)
				if err != nil {
					t.Fatal(err)
				}
				if err := c.cmd.Wait(); err != nil || !strings.Contains(string(output), want+"\n") {
					t.Fatalf("writer result: %s, %v", output, err)
				}
			}
			finish(first, "written")
			finish(second, "conflict")
			tools := secrettest.New(map[string]string{vaultRef: password})
			plain := decrypted(t, tools, file)
			if !strings.Contains(plain, "first: "+password) || strings.Contains(plain, "second:") {
				t.Fatal("failed writer silently changed the winner")
			}
			if initial == "encrypted" && !strings.Contains(plain, "original: keep") {
				t.Fatal("original key was lost")
			}
			if _, err := ops(tools).CopyValue(context.Background(), secret.Ref{Op: vaultRef}, secret.Ref{File: file, Path: "second"}); err != nil {
				t.Fatal(err)
			}
			plain = decrypted(t, tools, file)
			if !strings.Contains(plain, "first: "+password) || !strings.Contains(plain, "second: "+password) {
				t.Fatal("reread/retry lost a successful edit")
			}
		})
	}
}
