package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/giantswarm/beekeeper/internal/sandbox"
)

func TestLeaseKubeconfigBrokerOutput(t *testing.T) {
	a, _, _ := secretApp(t)
	a.cfg.LeaseDir = t.TempDir()
	a.cfg.Resources = []string{labOne}
	a.cfg.Labs = map[string]string{labOne: labCluster}
	path := labKubeconfig(a.cfg.LeaseDir, labOne)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	_, raw := labTLS(t)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(sandbox.Env, "1")
	t.Setenv("ALL_PROXY", "socks5://127.0.0.1:1080")
	hostBroker(t, a, func(_ context.Context, _ int, req sandbox.Request) (sandbox.Reply, error) {
		if req.Op == sandbox.OpPing {
			return sandbox.Reply{}, nil
		}
		if req.Op != sandbox.OpKubeconfig || req.Resource != labOne {
			return sandbox.Reply{Code: 1, Err: "unexpected request"}, nil
		}
		return sandbox.Reply{Out: path + "\n"}, nil
	})
	for _, export := range []bool{false, true} {
		args := []string{"kubeconfig", labOne}
		want := path + "\n"
		if export {
			args = append(args, "--export")
			want = "export KUBECONFIG=" + want
		}
		if out, err := runLease(a, args...); err != nil || out != want {
			t.Fatalf("export %v: %q, %v", export, out, err)
		}
	}
}
