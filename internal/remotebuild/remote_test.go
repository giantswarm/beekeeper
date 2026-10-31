//nolint:goconst // Test fixtures spell out command and wire-protocol literals for readability.
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
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	testPackages = "./..."
	testMain     = "main.go"
	testDisabled = "disabled"
	testExit     = "exit"
)

func validConfig() Config {
	return Config{Enabled: true, Kubeconfig: "/config/test", Context: "build-test", Namespace: "build-test", Images: map[string]string{"go": "builder:test"}}
}

func TestToolchain(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"go", "test", testPackages, "-run", "TestFoo"}, "go"},
		{[]string{shellBash, "-lc", "go test './...' -run 'TestFoo|TestBar'"}, "go"},
		{[]string{toolCargo, "fmt", "--", "--check"}, toolCargo},
		{[]string{toolCargo, "fmt"}, ""},
		{[]string{"gofmt", "-w", testMain}, ""},
		{[]string{shellBash, "-c", "go test ./... && go build ./..."}, ""},
		{[]string{shellBash, "-c", "go test $(cat args)"}, ""},
		{[]string{shellBash, "-c", "GOFLAGS=x go test ./..."}, ""},
		{[]string{shellBash, "-c", "go test ./... > result"}, ""},
		{[]string{"go", "test", "/host/path"}, ""},
		{[]string{"go", "test", "-exec=host-helper", testPackages}, ""},
		{[]string{toolCargo, "clippy", "--fix"}, ""}, // --local retains edits when requested.
	} {
		if got := Toolchain(tc.argv); got != tc.want {
			t.Errorf("%q = %q, want %q", tc.argv, got, tc.want)
		}
	}
}

func TestJobAndConfig(t *testing.T) {
	c := validConfig()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	j := Job(c, "bkb-test", "builder:test")
	p := j.Spec.Template.Spec
	if *p.AutomountServiceAccountToken || !*p.SecurityContext.RunAsNonRoot || *j.Spec.TTLSecondsAfterFinished != 60 || *j.Spec.BackoffLimit != 0 {
		t.Fatal("unsafe or nonexpiring builder")
	}
	if p.Containers[0].Resources.Limits.Cpu().String() != "2" || p.Containers[0].Resources.Limits.Memory().String() != "4Gi" {
		t.Fatal("resource defaults")
	}
	if !strings.Contains(p.Containers[0].Command[2], "$IDLE") {
		t.Fatal("missing idle watchdog")
	}
	for _, change := range []func(*Config){func(c *Config) { c.Context = "" }, func(c *Config) { c.Kubeconfig = "" }, func(c *Config) { c.Namespace = "bad/name" }, func(c *Config) { c.MaxPods = -1 }, func(c *Config) { c.CPU = "0" }, func(c *Config) { c.Memory = "invalid" }, func(c *Config) { c.IdleTimeout = time.Second }} {
		bad := validConfig()
		change(&bad)
		if bad.Validate() == nil {
			t.Errorf("accepted invalid config: %+v", bad)
		}
	}
	if err := (Config{}).Validate(); err != nil {
		t.Fatal("disabled must work without cluster")
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // fixed executable and typed arguments; test commands use owned fixtures
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git: %s: %v", out, err)
	}
}
func worktree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	for name, value := range map[string]string{testMain: "edited source", ".gitignore": "ignored\n", "ignored": "do not sync", "untracked": "new file", "deleted": "gone"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git(t, dir, "add", testMain, ".gitignore", "deleted")
	if err := os.Remove(filepath.Join(dir, "deleted")); err != nil {
		t.Fatal(err)
	}
	return dir
}
func TestRemoteArchive(t *testing.T) {
	root := worktree(t)
	if err := os.Symlink(testMain, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	f, err := archive(context.Background(), root, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	got := map[string]string{}
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeSymlink {
			got[h.Name] = "->" + h.Linkname
		} else {
			got[h.Name] = string(b)
		}
	}
	want := map[string]string{testMain: "edited source", ".gitignore": "ignored\n", "untracked": "new file", "link": "->main.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot %v, want %v", got, want)
	}
}

func TestRemoteFallbackAndExitRelay(t *testing.T) {
	root := worktree(t)
	// macOS temporary paths have a symlink prefix. Git returns the physical
	// root while Getwd may retain PWD's alias; exercise that mismatch on Linux too.
	alias := filepath.Join(t.TempDir(), "worktree-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	t.Chdir(alias)
	t.Setenv("PWD", alias)
	t.Setenv("GOFLAGS", "")
	t.Setenv("RUSTFLAGS", "")
	t.Setenv("CARGO_ENCODED_RUSTFLAGS", "")
	for _, mode := range []string{testDisabled, "unreachable", "full", "admission", "placement", "upload", testExit, "reconnect", "lost-launch", "lost-journal"} {
		t.Run(mode, func(t *testing.T) {
			c := validConfig()
			if mode == testDisabled {
				c.Enabled = false
			}
			var output, diagnostic bytes.Buffer
			calls, streams, launches := 0, 0, 0
			r := Runner{Config: c, StateDir: t.TempDir(), Stdout: &output, Stderr: &diagnostic}
			r.Command = func(ctx context.Context, in io.Reader, out io.Writer, args ...string) error {
				calls++
				if !slices.Equal(args[:6], []string{"--kubeconfig", c.Kubeconfig, "--context", c.Context, "--namespace", c.Namespace}) {
					t.Fatalf("cluster identity missing: %v", args)
				}
				a := args[7:]
				switch a[0] {
				case "get":
					if mode == "unreachable" {
						return errors.New("offline")
					}
					if a[1] == "job" {
						return nil
					}
					pods := corev1.PodList{}
					if mode == "full" {
						for range 8 {
							pods.Items = append(pods.Items, corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}})
						}
					}
					if slices.Contains(a, "-l") {
						pods.Items = []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "builder-pod"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}}
					}
					return json.NewEncoder(out).Encode(pods)
				case "create":
					if mode == "admission" {
						return errors.New("quota")
					}
					return nil
				case "wait":
					if mode == "placement" {
						return errors.New("deadline")
					}
					return nil
				case "delete":
					return nil
				case "exec":
					if slices.Contains(a, upload) {
						if mode == "upload" {
							return errors.New("stream lost")
						}
						_, err := io.Copy(io.Discard, in)
						return err
					}
					if slices.Contains(a, start) {
						launches++
						if mode == "lost-launch" {
							return errors.New("lost reply")
						}
						return nil
					}
					if slices.Contains(a, follow) {
						streams++
						offset, err := strconvOffset(a[len(a)-1])
						if err != nil {
							t.Fatal(err)
						}
						if mode == "lost-journal" {
							return errors.New("offline")
						}
						if mode == "reconnect" && streams == 1 {
							_, _ = io.WriteString(out, "first")
							return errors.New("dropped stream")
						}
						text := "first second\n"
						if offset > len(text) {
							t.Fatal("bad offset")
						}
						_, err = io.WriteString(out, text[offset:])
						return err
					}
					if slices.Contains(a, "cat") {
						_, err := io.WriteString(out, "37\n")
						return err
					}
					return nil
				}
				t.Fatalf("unexpected call: %v", a)
				return nil
			}
			handled, rc := r.Try(context.Background(), []string{"go", "test", testPackages})
			remote := slices.Contains([]string{testExit, "reconnect", "lost-launch", "lost-journal"}, mode)
			if handled != remote {
				t.Fatalf("handled=%v code=%d: %s", handled, rc, diagnostic.String())
			}
			if mode == testDisabled && calls != 0 {
				t.Fatal("disabled made cluster calls")
			}
			if !remote && mode != testDisabled && strings.Count(diagnostic.String(), "using local slot") != 1 {
				t.Fatal(diagnostic.String())
			}
			if remote {
				if launches != 1 {
					t.Fatalf("launched %d times", launches)
				}
				if mode == "lost-journal" {
					if rc != 125 {
						t.Fatal(rc)
					}
				} else if rc != 37 || output.String() != "first second\n" {
					t.Fatalf("rc=%d output=%q", rc, output.String())
				}
			}
		})
	}
}
func strconvOffset(s string) (int, error) { var n int; _, err := fmt.Sscan(s, &n); return n, err }

func TestRemoteExistingBuilder(t *testing.T) {
	for _, mode := range []string{"warm", "different-image", "different-resources", "foreign", "retiring", "placement"} {
		t.Run(mode, func(t *testing.T) {
			c := validConfig().Defaults()
			deletes, creates := 0, 0
			r := Runner{Config: c}
			r.Command = func(ctx context.Context, in io.Reader, out io.Writer, args ...string) error {
				a := args[7:]
				switch a[0] {
				case "get":
					if a[1] == "job" {
						j := Job(c, "bkb-test", "builder:test")
						switch mode {
						case "different-image":
							j.Spec.Template.Spec.Containers[0].Image = "changed"
						case "different-resources":
							j.Annotations = nil
						case "foreign":
							j.Labels = nil
						case "retiring":
							j.Status.Succeeded = 1
						}
						return json.NewEncoder(out).Encode(j)
					}
					if !slices.Contains(a, "-l") {
						t.Fatal("warm builder should bypass capacity preflight")
					}
					return json.NewEncoder(out).Encode(corev1.PodList{Items: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "warm-pod"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}}})
				case "wait":
					if mode == "placement" {
						return errors.New("not ready")
					}
					return nil
				case "create":
					creates++
					return nil
				case "delete":
					deletes++
					return nil
				}
				t.Fatalf("unexpected %v", a)
				return nil
			}
			pod, err := r.ensurePod(context.Background(), "bkb-test", "builder:test")
			if mode == "warm" {
				if err != nil || pod != "warm-pod" {
					t.Fatalf("%s %v", pod, err)
				}
			} else if err == nil {
				t.Fatal("expected refusal")
			}
			if deletes != 0 || creates != 0 {
				t.Fatal("modified existing builder")
			}
		})
	}
}
