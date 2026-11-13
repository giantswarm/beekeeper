//nolint:goconst // Keep failure scenarios and wire-protocol fixtures readable.
package remotebuild

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestMergeCheckUsesExplicitWorktreeAndRemoteLint runs the merge result's
// lint in the builder, in the checker environment.
//
// control: none: the checker environment's command is new, so the base cannot build the test
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
			if rc != 0 || snap != "merged source" || command != shellCommand(checkerCommand([]string{"golangci-lint", "run", "--config", ".golangci.yaml", testPackages}, nil)) {
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
