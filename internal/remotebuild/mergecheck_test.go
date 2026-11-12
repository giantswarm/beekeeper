//nolint:goconst // Keep failure scenarios and wire-protocol fixtures readable.
package remotebuild

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMergeCheckUsesExplicitWorktreeAndRemoteLint(t *testing.T) {
	root := worktree(t)
	// The coordinator's cwd is deliberately a different repository.
	t.Chdir(worktree(t))
	if err := os.WriteFile(filepath.Join(root, testMain), []byte("merged source"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"GOFLAGS", "RUSTFLAGS", "CARGO_ENCODED_RUSTFLAGS"} {
		t.Setenv(key, "")
	}
	for _, offline := range []bool{false, true} {
		t.Run(fmt.Sprint(offline), func(t *testing.T) {
			var diagnostic bytes.Buffer
			var snap, command string
			r := Runner{Dir: root, Config: validConfig(), StateDir: t.TempDir(), Stdout: io.Discard, Stderr: &diagnostic}
			r.Command = func(_ context.Context, in io.Reader, out io.Writer, args ...string) error {
				if offline {
					return fmt.Errorf("offline")
				}
				a := args[7:]
				switch a[0] {
				case "get":
					if a[1] == "job" {
						return nil
					}
					pods := corev1.PodList{}
					if slices.Contains(a, "-l") {
						pods.Items = []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "merge-builder-pod"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}}
					}
					return json.NewEncoder(out).Encode(pods)
				case "create", "wait", "delete":
					return nil
				case "exec":
					switch {
					case slices.Contains(a, upload):
						tr := tar.NewReader(in)
						for {
							h, err := tr.Next()
							if err == io.EOF {
								break
							}
							if err != nil {
								return err
							}
							if h.Name == testMain {
								b, err := io.ReadAll(tr)
								if err != nil {
									return err
								}
								snap = string(b)
							}
						}
					case slices.Contains(a, start):
						command = a[len(a)-3]
					case slices.Contains(a, "cat"):
						_, err := io.WriteString(out, "0\n")
						return err
					}
					return nil
				}
				t.Fatalf("unexpected call: %v", a)
				return nil
			}
			handled, rc := r.TryMergeCheck(t.Context(), []string{"golangci-lint", "run", "--config", ".golangci.yaml", testPackages})
			if !handled {
				t.Fatal("merge check allowed local fallback")
			}
			if offline {
				if rc != 125 || strings.Contains(diagnostic.String(), "using local slot") {
					t.Fatalf("%d: %s", rc, &diagnostic)
				}
				return
			}
			if rc != 0 || snap != "merged source" || !strings.HasPrefix(command, shellCommand([]string{shellBash, "-c", lintShim, "golangci-lint", GolangCILintModule})+" 'run' '--config' '.golangci.yaml'") {
				t.Fatalf("rc=%d snapshot=%q command=%q", rc, snap, command)
			}
		})
	}
}

func TestBuilderReservesConfiguredScratchSpace(t *testing.T) {
	c := validConfig()
	c.Storage = "48Gi"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	p := Job(c, "merge-builder", "builder:test").Spec.Template.Spec
	resources := p.Containers[0].Resources
	if resources.Requests.StorageEphemeral().String() != c.Storage || resources.Limits.StorageEphemeral().String() != c.Storage || p.Volumes[0].EmptyDir.SizeLimit.String() != c.Storage {
		t.Fatal("scratch space not reserved and bounded")
	}
	for _, storage := range []string{"-1Gi", "bad"} {
		c.Storage = storage
		if c.Validate() == nil {
			t.Fatalf("accepted storage %q", storage)
		}
	}
}

// The shim prefers the image's golangci-lint, installs the pinned one into the
// pod's workspace otherwise, and reports a failed install as exit 125.
func TestLintShimInstallsPinnedLinterWhenImageLacksIt(t *testing.T) {
	bash, err := exec.LookPath(shellBash)
	if err != nil {
		t.Skip("bash unavailable")
	}
	tools := t.TempDir()
	shim := strings.ReplaceAll(lintShim, "/work/tools", tools)
	_, version, ok := strings.Cut(GolangCILintModule, "@")
	if !ok || version == "" {
		t.Fatal("golangci-lint module must pin a version")
	}
	installed := filepath.Join(tools, version, "golangci-lint")
	lint := "#!/bin/sh\necho \"linted by $0 $* cache=$GOLANGCI_LINT_CACHE\"\n"
	// The install must not touch the image's GOPATH, which the builder's
	// non-root user cannot write.
	install := "#!/bin/sh\n[ \"$*\" = 'install " + GolangCILintModule + "' ] && [ \"$GOPATH\" = '" + filepath.Join(tools, "gopath") + "' ] && [ -z \"$GOFLAGS\" ] || exit 1\nmkdir -p \"$GOBIN\" && printf '%s' '" + lint + "' > \"$GOBIN/golangci-lint\" && chmod 700 \"$GOBIN/golangci-lint\"\n"
	for _, tc := range []struct {
		name, image, goScript, want string
		rc                          int
	}{
		{name: "image linter", image: lint, want: "linted by IMAGE/golangci-lint run ./... cache=" + filepath.Join(tools, "lint-cache")},
		{name: "install", goScript: install, want: "linted by " + installed + " run ./... cache=" + filepath.Join(tools, "lint-cache")},
		{name: "already installed", want: "linted by " + installed + " run ./... cache=" + filepath.Join(tools, "lint-cache")},
		{name: "install fails", goScript: "#!/bin/sh\nexit 1\n", want: "installing " + GolangCILintModule + " failed", rc: 125},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.rc != 0 {
				if err := os.RemoveAll(tools); err != nil {
					t.Fatal(err)
				}
			}
			image := t.TempDir()
			for name, script := range map[string]string{"golangci-lint": tc.image, "go": tc.goScript} {
				if script == "" {
					continue
				}
				if err := os.WriteFile(filepath.Join(image, name), []byte(script), 0o700); err != nil { //nolint:gosec // executable fixture in the test-owned directory
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			c := exec.CommandContext(t.Context(), bash, "-c", shim, "golangci-lint", GolangCILintModule, "run", "./...") //nolint:gosec // test-owned shim and fixtures
			c.Env = []string{"PATH=" + image + ":/usr/bin:/bin", "GOPATH=/read-only-image-gopath", "GOFLAGS=-mod=mod"}
			c.Stdout, c.Stderr = &out, &out
			rc := 0
			var exit *exec.ExitError
			if err := c.Run(); errors.As(err, &exit) {
				rc = exit.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if rc != tc.rc || !strings.Contains(out.String(), strings.ReplaceAll(tc.want, "IMAGE", image)) {
				t.Fatalf("rc=%d output=%q", rc, &out)
			}
		})
	}
}

func TestBuilderGoMemoryBudget(t *testing.T) {
	for _, test := range []struct{ memory, limit string }{
		{"4Gi", "2147483648B"},
		{"1Gi", "536870912B"},
		{"6000M", "3000000000B"},
	} {
		t.Run(test.memory, func(t *testing.T) {
			c := validConfig()
			c.Memory = test.memory
			job := Job(c, "memory-builder", "builder:test")
			for _, env := range job.Spec.Template.Spec.Containers[0].Env {
				if env.Name == "GOMEMLIMIT" {
					if env.Value != test.limit {
						t.Fatalf("Go memory limit = %s, want %s", env.Value, test.limit)
					}
					return
				}
			}
			t.Fatal("builder has no Go memory budget")
		})
	}
}
