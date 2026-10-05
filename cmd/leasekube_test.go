package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/sandbox"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

func runLease(a *app, args ...string) (string, error) {
	a.out = &bytes.Buffer{}
	c := a.leaseCmd()
	c.PersistentPreRunE = nil
	usageArgs(c)
	c.SetArgs(args)
	c.SetOut(a.out)
	c.SetErr(a.out)
	c.SilenceUsage, c.SilenceErrors = true, true
	err := c.ExecuteContext(context.Background())
	return a.out.(*bytes.Buffer).String(), err
}

func TestLeaseKubeconfigOnlyForTheHolder(t *testing.T) {
	a, _, _ := secretApp(t)
	a.cfg.LeaseDir = t.TempDir()
	a.cfg.Resources = []string{labOne, labTwo, browser}
	a.cfg.Labs = map[string]string{labOne: labCluster, labTwo: labTwo}
	dir := lease.Dir(a.cfg.LeaseDir)
	if _, err := dir.Claim(labOne, lease.Holder{Env: labOne, Name: a.as}); err != nil {
		t.Fatal(err)
	}
	if _, err := dir.Claim(labTwo, lease.Holder{Env: labTwo, Name: "another session"}); err != nil {
		t.Fatal(err)
	}
	path := labKubeconfig(a.cfg.LeaseDir, labOne)
	out, err := runLease(a, "kubeconfig", labOne)
	if err != nil || out != "export KUBECONFIG="+path+"\n" {
		t.Fatalf("the holder's kubeconfig: %q, %v", out, err)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the test's lease
	if st, _ := os.Stat(path); err != nil || string(raw) != secrettest.Kubeconfig(labCluster) || st.Mode().Perm() != 0o600 {
		t.Errorf("kubeconfig %q, %v", raw, err)
	}
	if _, err := runLease(a, "kubeconfig", labTwo); Code(err) != ExitRefused {
		t.Errorf("another holder's lab: exit %d, want refused", Code(err))
	}
	if _, err := os.Stat(labKubeconfig(a.cfg.LeaseDir, labTwo)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("another holder's lab got a kubeconfig: %v", err)
	}
	if _, err := runLease(a, "kubeconfig", browser); Code(err) != ExitUsage {
		t.Errorf("a resource that is no lab: exit %d, want usage", Code(err))
	}
	if err := dir.Release(labOne); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the kubeconfig outlived its lease: %v", err)
	}
	if _, err := runLease(a, "kubeconfig", labOne); Code(err) != ExitRefused {
		t.Errorf("a released lab: exit %d, want refused", Code(err))
	}
}

func TestLeaseKubeconfigInTheSandboxAsksTheBroker(t *testing.T) {
	a, _, _ := secretApp(t)
	a.cfg.LeaseDir = t.TempDir()
	a.cfg.Resources = []string{labOne}
	a.cfg.Labs = map[string]string{labOne: labCluster}
	t.Setenv(sandbox.Env, "1")
	// no broker serves the scratch state directory
	if _, err := runLease(a, "kubeconfig", labOne); Code(err) != ExitRefused {
		t.Errorf("no broker: exit %d, want refused", Code(err))
	}
	if _, err := os.Stat(filepath.Join(a.cfg.LeaseDir, labOne)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the sandbox wrote a lease: %v", err)
	}
}

func TestBrokeredKubeconfigArgv(t *testing.T) {
	if argv, err := brokeredKubeconfigArgv(sandbox.Request{Op: sandbox.OpKubeconfig, Resource: labOne}); err != nil || len(argv) != 3 || argv[2] != labOne {
		t.Errorf("%q, %v", argv, err)
	}
	for _, bad := range []string{"", "../state", "-h", "a b", "--as=x"} {
		if _, err := brokeredKubeconfigArgv(sandbox.Request{Op: sandbox.OpKubeconfig, Resource: bad}); err == nil {
			t.Errorf("%q: want a refusal", bad)
		}
	}
}
