package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
)

// bundleTransport is a host that holds the bundles it was sent.
type bundleTransport struct {
	testExecutionTransport
	installed map[string]bool
	sent      int
	refuse    error
}

func (f *bundleTransport) BundleStatus(_ context.Context, digest string) (execution.BundleStatus, error) {
	return execution.BundleStatus{Digest: digest, Installed: f.installed[digest]}, nil
}

func (f *bundleTransport) SyncBundle(_ context.Context, b execution.CoordinatorBundle) (string, error) {
	f.sent++
	if f.refuse != nil {
		return "", f.refuse
	}
	f.installed[b.Digest()] = true
	return b.Digest(), nil
}

// policyBundleApp configures one approved rule file and answers its path.
func policyBundleApp(t *testing.T, a *app) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rule := filepath.Join(root, "AGENTS.md")
	if err := os.WriteFile(rule, []byte("First rule."), 0o600); err != nil {
		t.Fatal(err)
	}
	a.cfg.Agents.PolicyBundle = &config.PolicyBundle{Root: root, Sources: []execution.BundleSource{{Source: "AGENTS.md", Artifact: "rules/desk/AGENTS.md"}}}
	return rule
}

func startForBundle(a *app, name, brief string) error {
	_, err := runAgents(a, agentStartName, "--name", name, brief, "--host", sshTestSelector, "--repo", sshTestRepository, "--ref", mainBranch, "--model", bgModel)
	return err
}

// A changed rule reaches the next start, and an unchanged one travels once.
func TestRemoteStartCarriesTheCurrentBundle(t *testing.T) {
	a, brief := sshStartTestApp(t)
	rule := policyBundleApp(t, a)
	fake := &bundleTransport{installed: map[string]bool{}}
	var bound []string
	fake.start = func(_ context.Context, r execution.StartRequest) (execution.Handle, error) {
		bound = append(bound, r.BundleDigest)
		return execution.Handle{}, errors.New("host refused the fixture start")
	}
	// The host holds no journal, so each refused start frees its reservation.
	fake.inspect = func(context.Context, execution.Ref) (execution.Observation, error) {
		return execution.Observation{}, execution.ErrNotFound
	}
	a.executionTransport = func(execution.Host) execution.Transport { return fake }
	first, err := a.currentBundle()
	if err != nil {
		t.Fatal(err)
	}
	_ = startForBundle(a, "first-worker", brief)
	_ = startForBundle(a, "second-worker", brief)
	if err := os.WriteFile(rule, []byte("Second rule."), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := a.currentBundle()
	if err != nil {
		t.Fatal(err)
	}
	_ = startForBundle(a, "third-worker", brief)
	if fake.sent != 2 {
		t.Fatalf("sent %d bundles, want one per change", fake.sent)
	}
	if len(bound) < 3 || bound[0] != first.Digest() || bound[len(bound)-1] != second.Digest() || first.Digest() == second.Digest() {
		t.Fatalf("starts bound %v, want %s then %s", bound, first.Digest(), second.Digest())
	}
}

// Any failed synchronization starts nothing, on the host or in the roster.
func TestRemoteStartRefusesWithoutItsBundle(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *app, *bundleTransport)
	}{
		{"failed transfer", func(_ *testing.T, _ *app, f *bundleTransport) { f.refuse = errors.New("connection lost") }},
		{"unreadable source", func(t *testing.T, a *app, _ *bundleTransport) {
			if err := os.Remove(filepath.Join(a.cfg.Agents.PolicyBundle.Root, "AGENTS.md")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, brief := sshStartTestApp(t)
			policyBundleApp(t, a)
			fake := &bundleTransport{installed: map[string]bool{}}
			fake.start = func(context.Context, execution.StartRequest) (execution.Handle, error) {
				t.Fatal("a worker started without its bundle")
				return execution.Handle{}, nil
			}
			a.executionTransport = func(execution.Host) execution.Transport { return fake }
			tc.setup(t, a, fake)
			err := startForBundle(a, "held-worker", brief)
			if err == nil || !strings.Contains(err.Error(), "no remote worker starts") {
				t.Fatalf("start: %v", err)
			}
			st, err := a.store.Read()
			if err != nil || len(st.Starts) != 0 || len(st.Agents) != 0 {
				t.Fatalf("a refused start left a record: %+v %v", st.Starts, err)
			}
		})
	}
}

func TestHostsCheckReportsABundleBehindTheCoordinator(t *testing.T) {
	a, _ := sshStartTestApp(t)
	policyBundleApp(t, a)
	fake := &bundleTransport{installed: map[string]bool{}}
	a.executionTransport = func(execution.Host) execution.Transport { return fake }
	h := a.cfg.Agents.Hosts[sshTestSelector]
	r := a.hostBundleReport(context.Background(), h)
	if r == nil || r.Current || len(r.Differs) != 1 || r.Differs[0] != "rules/desk/AGENTS.md" {
		t.Fatalf("report: %+v", r)
	}
	b, err := a.currentBundle()
	if err != nil {
		t.Fatal(err)
	}
	fake.installed[b.Digest()] = true
	if r := a.hostBundleReport(context.Background(), h); r == nil || !r.Current {
		t.Fatalf("report: %+v", r)
	}
	a.cfg.Agents.PolicyBundle = nil
	if r := a.hostBundleReport(context.Background(), h); r != nil {
		t.Fatalf("report without a bundle: %+v", r)
	}
}
