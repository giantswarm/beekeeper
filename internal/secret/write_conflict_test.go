package secret_test

import (
	"context"
	"errors"
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

func awaitFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil { //nolint:gosec // a path of this test
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for test process")
}

func TestSOPSWriterProcess(t *testing.T) {
	file := os.Getenv("SOPS_TEST_DEST")
	if file == "" {
		return
	}
	gate := os.Getenv("SOPS_TEST_GATE")
	tools := secrettest.New(map[string]string{"op://Shared/synthetic/value": "synthetic"})
	o := ops(tools)
	o.Run = func(ctx context.Context, dir string, env []string, stdin io.Reader, name string, args ...string) ([]byte, error) {
		if name == "sops" && slices.Contains(args, "encrypt") {
			if err := os.WriteFile(gate+".ready", nil, 0o600); err != nil { //nolint:gosec // a path of this test
				return nil, err
			}
			awaitFile(t, gate+".go")
		}
		return tools.Run(ctx, dir, env, stdin, name, args...)
	}
	dst := secret.Ref{File: file, Path: os.Getenv("SOPS_TEST_KEY")}
	var err error
	if strings.HasSuffix(dst.Path, ".first") {
		_, err = o.CopyValue(context.Background(), secret.Ref{Op: "op://Shared/synthetic/value"}, dst)
	} else {
		err = o.SetSOPSValue(context.Background(), dst, "synthetic")
	}
	if os.Getenv("SOPS_TEST_CONFLICT") == "1" {
		if !errors.Is(err, secret.ErrWriteConflict) {
			t.Fatalf("expected conflict, got %v", err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentSOPSWritesConflictAndRetry(t *testing.T) {
	for _, kind := range []string{"encrypted", "skeleton", "missing"} {
		t.Run(kind, func(t *testing.T) {
			dir, _ := scratch(t)
			t.Setenv("XDG_CACHE_HOME", filepath.Join(t.TempDir(), "cache"))
			file := filepath.Join(dir, "destination.sops.yaml")
			var initial []byte
			switch kind {
			case "encrypted":
				initial = secrettest.Encrypt("stringData: {}\n")
			case "skeleton":
				initial = []byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: synthetic\n  namespace: synthetic\n")
			}
			if initial != nil {
				if err := os.WriteFile(file, initial, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			start := func(key string, conflict bool) (*exec.Cmd, string, *strings.Builder) {
				gate := filepath.Join(dir, key)
				cmd := exec.Command(os.Args[0], "-test.run=^TestSOPSWriterProcess$", "-test.timeout=30s") //nolint:gosec // current test executable
				cmd.Env = append(os.Environ(), "SOPS_TEST_DEST="+file, "SOPS_TEST_GATE="+gate, "SOPS_TEST_KEY=stringData."+key)
				if conflict {
					cmd.Env = append(cmd.Env, "SOPS_TEST_CONFLICT=1")
				}
				out := new(strings.Builder)
				cmd.Stdout = out
				cmd.Stderr = out
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if cmd.ProcessState == nil {
						_ = cmd.Process.Kill()
						_ = cmd.Wait()
					}
				})
				awaitFile(t, gate+".ready")
				return cmd, gate, out
			}
			first, a, ao := start("first", false)
			second, b, bo := start("second", true)
			release := func(cmd *exec.Cmd, gate string, out *strings.Builder) {
				if err := os.WriteFile(gate+".go", nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := cmd.Wait(); err != nil {
					t.Fatalf("writer failed: %v: %s", err, out)
				}
			}
			release(first, a, ao)
			before, err := os.ReadFile(file) //nolint:gosec // a path of this test
			if err != nil {
				t.Fatal(err)
			}
			release(second, b, bo)
			after, err := os.ReadFile(file) //nolint:gosec // a path of this test
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("conflicting writer changed bytes")
			}
			tools := secrettest.New(nil)
			if err := ops(tools).SetSOPSValue(context.Background(), secret.Ref{File: file, Path: "stringData.second"}, "synthetic"); err != nil {
				t.Fatal(err)
			}
			plain := decrypted(t, tools, file)
			for _, key := range []string{"first:", "second:"} {
				if !strings.Contains(plain, key) {
					t.Fatalf("retry lost %s", key)
				}
			}
		})
	}
}
