package guard

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

func TestRunKubeconfigWriter(t *testing.T) {
	if os.Getenv("BEEKEEPER_TEST_KUBECONFIG_WRITER") != "1" {
		return
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg := api.NewConfig()
	cfg.Contexts["test-context"] = &api.Context{}
	cfg.CurrentContext = "test-context"
	if err := clientcmd.ModifyConfig(rules, *cfg, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("BEEKEEPER_TEST_KUBECONFIG_REPORT"), []byte(rules.GetDefaultFilename()), 0o600); err != nil { //nolint:gosec // a path of this test
		t.Fatal(err)
	}
}

const (
	kubeconfigAssignment = "assignment"
	kubeconfigInherited  = "inherited"
)

type alreadyCappedKubeconfigCapper struct{ fakeCapper }

func (*alreadyCappedKubeconfigCapper) Capped() bool { return true }

func TestRunProtectsDefaultKubeconfig(t *testing.T) {
	for _, execution := range []string{"capped", "uncapped", "already-capped"} {
		for _, mode := range []string{"unset", "blank", kubeconfigInherited, kubeconfigAssignment} {
			t.Run(mode+"/"+execution, func(t *testing.T) {
				saved := plat.Capper
				plat.Capper = &fakeCapper{down: execution == "uncapped"}
				if execution == "already-capped" {
					plat.Capper = &alreadyCappedKubeconfigCapper{}
				}
				t.Cleanup(func() { plat.Capper = saved })
				home := t.TempDir()
				machine := filepath.Join(home, ".kube", "config")
				if err := os.MkdirAll(filepath.Dir(machine), 0o700); err != nil {
					t.Fatal(err)
				}
				original := []byte("apiVersion: v1\nkind: Config\ncurrent-context: machine\ncontexts:\n- name: machine\n  context: {}\n")
				if err := os.WriteFile(machine, original, 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("HOME", home)
				t.Setenv("KUBECONFIG", "")
				if mode == "unset" {
					if err := os.Unsetenv("KUBECONFIG"); err != nil {
						t.Fatal(err)
					}
				}
				explicit := filepath.Join(home, "explicit")
				if mode == kubeconfigInherited {
					t.Setenv("KUBECONFIG", explicit)
				}
				report := filepath.Join(home, "report")
				t.Setenv("BEEKEEPER_TEST_KUBECONFIG_WRITER", "1")
				t.Setenv("BEEKEEPER_TEST_KUBECONFIG_REPORT", report)
				binary, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				argv := []string{binary, "-test.run=^TestRunKubeconfigWriter$"}
				if mode == kubeconfigAssignment {
					argv = []string{"sh", "-c", `KUBECONFIG="$1" "$2" -test.run='^TestRunKubeconfigWriter$'`, "sh", explicit, binary}
				}
				o := Options{Max: "1M", Swap: "0", SlotDir: t.TempDir(), Slots: 1, Stderr: io.Discard}
				if rc := Run(o, argv); rc != 0 {
					t.Fatalf("Run exit %d", rc)
				}
				got, err := os.ReadFile(machine) //nolint:gosec // a path of this test
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, original) {
					t.Fatalf("machine kubeconfig changed: %s", got)
				}
				raw, err := os.ReadFile(report) //nolint:gosec // a path of this test
				if err != nil {
					t.Fatal(err)
				}
				used := string(raw)
				if mode == kubeconfigInherited || mode == kubeconfigAssignment {
					if used != explicit {
						t.Fatalf("used %q, want explicit path", used)
					}
					cfg, err := clientcmd.LoadFromFile(explicit)
					if err != nil {
						t.Fatal(err)
					}
					if cfg.CurrentContext != "test-context" {
						t.Fatal("explicit config not updated")
					}
				} else {
					if !strings.Contains(used, "beekeeper-kubeconfig-") {
						t.Fatalf("used %q, want scratch", used)
					}
					if _, err := os.Stat(filepath.Dir(used)); !os.IsNotExist(err) { //nolint:gosec // a path of this test
						t.Fatalf("scratch not cleaned up: %v", err)
					}
				}
			})
		}
	}
}
