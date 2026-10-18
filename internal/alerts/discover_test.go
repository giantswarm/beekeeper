//go:build linux

package alerts

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeTsh lists the clusters in $FAKE_CLUSTERS (JSON, as tsh kube ls
// --format json) and logs into one by adding its context to the file
// KUBECONFIG names, recording each run with its TELEPORT_HOME in
// $FAKE_CALLS.
const fakeTsh = `#!/bin/sh
echo "tsh $* TELEPORT_HOME=$TELEPORT_HOME KUBECONFIG=$KUBECONFIG" >> "$FAKE_CALLS"
case "$*" in
  "kube ls --format json") echo "$FAKE_CLUSTERS";;
  "kube login "*) echo "teleport.example.io-$3" >> "$KUBECONFIG";;
  *) exit 2;;
esac
`

// fakeDiscoverKubectl takes a kubeconfig for a list of context names: it
// lists those of every KUBECONFIG file, and answers a HelmRelease listing
// for a context with the names in $FAKE_RELEASES/<context>, filtered by
// nothing but the team's label selector; a context without that file does
// not answer.
const fakeDiscoverKubectl = `#!/bin/sh
echo "kubectl $* KUBECONFIG=$KUBECONFIG" >> "$FAKE_CALLS"
case "$*" in *"config get-contexts -o name"*)
  IFS=:; for f in $KUBECONFIG; do cat "$f" 2>/dev/null; done; exit 0;;
esac
ctx=$2
case "$*" in *"get helmreleases.helm.toolkit.fluxcd.io -A -l application.giantswarm.io/team=bumblebee "*) ;; *) exit 2;; esac
if [ ! -e "$FAKE_RELEASES/$ctx" ]; then echo "Unable to connect to the server: dial tcp: i/o timeout" >&2; exit 1; fi
cat "$FAKE_RELEASES/$ctx"
`

// Two of the team's releases.
const (
	muster = "muster"
	kagent = "kagent"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil { //nolint:gosec // test doubles and fixtures
		t.Fatal(err)
	}
}

// The installations found are Teleport's management clusters that carry a
// HelmRelease of the team: one without a context is logged into the
// discovery's own kubeconfig, never the machine's; a workload cluster and
// the proxy are not read; one that does not answer keeps what the last
// discovery found and is named.
func TestDiscoverFindsTheTeamsInstallations(t *testing.T) {
	dir := t.TempDir()
	tsh, kubectl := filepath.Join(dir, "tsh"), filepath.Join(dir, "kubectl")
	write(t, tsh, fakeTsh)
	write(t, kubectl, fakeDiscoverKubectl)
	calls, releases := filepath.Join(dir, "calls"), filepath.Join(dir, "releases")
	if err := os.Mkdir(releases, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CALLS", calls)
	t.Setenv("FAKE_RELEASES", releases)
	t.Setenv("FAKE_CLUSTERS", `[{"kube_cluster_name":"alpha"},{"kube_cluster_name":"alpha-wc1"},{"kube_cluster_name":"beta"},`+
		`{"kube_cluster_name":"gamma"},{"kube_cluster_name":"delta"},{"kube_cluster_name":"epsilon"},{"kube_cluster_name":"teleport.example.io"}]`)
	machine, own := filepath.Join(dir, "config"), filepath.Join(dir, "teleport.yaml")
	write(t, machine, "teleport.example.io-alpha\nkind-lab-1\n")
	write(t, own, "teleport.example.io-gamma\nteleport.example.io-delta\nteleport.example.io-epsilon\n")
	write(t, filepath.Join(releases, "teleport.example.io-alpha"), "agent-platform muster")
	write(t, filepath.Join(releases, "teleport.example.io-beta"), "kagent")
	write(t, filepath.Join(releases, "teleport.example.io-gamma"), "")

	d := Discoverer{Tsh: tsh, TeleportHome: filepath.Join(dir, "tsh-home"), Kubectl: kubectl,
		Kubeconfig: []string{machine, own}, Login: own,
		Template: func(name string) string { return "teleport.example.io-" + name },
		Exclude:  []string{"*-*", "*.*"}, Label: "application.giantswarm.io/team", Team: ourTeam, Timeout: 10 * time.Second}
	prev := &Discovery{Installations: map[string]Found{instD: {Context: "teleport.example.io-delta", Releases: []string{muster}}}}
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	got, unread, err := d.Discover(context.Background(), prev, at)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Found{
		instA: {Context: "teleport.example.io-alpha", Releases: []string{"agent-platform", muster}},
		instB: {Context: "teleport.example.io-beta", Releases: []string{kagent}},
		instD: {Context: "teleport.example.io-delta", Releases: []string{muster}},
	}
	if len(got.Installations) != len(want) || !got.At.Equal(at) {
		t.Errorf("discovery at %s = %+v, want %+v", got.At, got.Installations, want)
	}
	for name, w := range want {
		if g := got.Installations[name]; g.Context != w.Context || !slices.Equal(g.Releases, w.Releases) {
			t.Errorf("%s = %+v, want %+v", name, g, w)
		}
	}
	if want := []string{"delta: Unable to connect to the server: dial tcp: i/o timeout", "epsilon: Unable to connect to the server: dial tcp: i/o timeout"}; !slices.Equal(unread, want) {
		t.Errorf("unread = %q, want %q", unread, want)
	}
	raw, _ := os.ReadFile(calls) //nolint:gosec // the test's own file
	log := string(raw)
	if !strings.Contains(log, "tsh kube login beta TELEPORT_HOME="+d.TeleportHome+" KUBECONFIG="+own+"\n") || strings.Count(log, "kube login") != 1 {
		t.Errorf("logins:\n%s", log)
	}
	for _, never := range []string{"alpha-wc1", "teleport.example.io-teleport.example.io"} {
		if strings.Contains(log, never) {
			t.Errorf("%s read:\n%s", never, log)
		}
	}
	if m, _ := os.ReadFile(machine); string(m) != "teleport.example.io-alpha\nkind-lab-1\n" { //nolint:gosec // the test's own file
		t.Errorf("the machine kubeconfig was written: %q", m)
	}
}

// Without a login file, an installation without a context is not logged
// into: it is named unread.
func TestDiscoverWithoutLoginFile(t *testing.T) {
	dir := t.TempDir()
	tsh, kubectl := filepath.Join(dir, "tsh"), filepath.Join(dir, "kubectl")
	write(t, tsh, fakeTsh)
	write(t, kubectl, fakeDiscoverKubectl)
	t.Setenv("FAKE_CALLS", filepath.Join(dir, "calls"))
	t.Setenv("FAKE_RELEASES", dir)
	t.Setenv("FAKE_CLUSTERS", `[{"kube_cluster_name":"beta"}]`)
	machine := filepath.Join(dir, "config")
	write(t, machine, "")
	d := Discoverer{Tsh: tsh, Kubectl: kubectl, Kubeconfig: []string{machine},
		Template: func(name string) string { return "teleport.example.io-" + name },
		Label:    "application.giantswarm.io/team", Team: ourTeam, Timeout: 10 * time.Second}
	got, unread, err := d.Discover(context.Background(), nil, time.Now())
	if err != nil || len(got.Installations) != 0 || !slices.Equal(unread, []string{"beta: no kube context"}) {
		t.Errorf("discovery = %+v, unread %q, err %v", got, unread, err)
	}
}

// A failed cluster listing is an error, and nothing is discovered.
func TestDiscoverTshFails(t *testing.T) {
	dir := t.TempDir()
	d := Discoverer{Tsh: filepath.Join(dir, "missing"), Kubectl: "kubectl", Team: ourTeam, Label: "l"}
	if _, _, err := d.Discover(context.Background(), nil, time.Now()); err == nil {
		t.Error("no error")
	}
}

// The discovery is kept in the state directory and read back.
func TestDiscoveryStore(t *testing.T) {
	dir := t.TempDir()
	if d, err := LoadDiscovery(dir); err != nil || d.Installations == nil || !d.At.IsZero() {
		t.Fatalf("empty = %+v, %v", d, err)
	}
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	in := &Discovery{At: at, Installations: map[string]Found{instA: {Context: "c", Releases: []string{muster}}}}
	if err := SaveDiscovery(dir, in); err != nil {
		t.Fatal(err)
	}
	back, err := LoadDiscovery(dir)
	if err != nil || !back.At.Equal(at) || back.Installations[instA].Context != "c" {
		t.Errorf("back = %+v, %v", back, err)
	}
}

// Discovered installations join the configured ones with their reason; a
// configured one keeps its own.
func TestTargetsKeepAGivenReason(t *testing.T) {
	got := Targets([]Target{{Name: instA}, {Name: instB, Why: "carries bumblebee's muster"}}, nil, template, []string{ctxA, ctxB})
	want := []Target{{Name: instA, Context: ctxA, Why: WhyConfigured}, {Name: instB, Context: ctxB, Why: "carries bumblebee's muster"}}
	if !slices.Equal(got, want) {
		t.Errorf("targets = %+v, want %+v", got, want)
	}
}

// The targets a discovery adds: every installation found that is not
// configured, sorted, with the releases it carries.
func TestDiscoveryTargets(t *testing.T) {
	d := &Discovery{Installations: map[string]Found{
		instB: {Releases: []string{kagent}},
		instA: {Releases: []string{"agent-platform", muster, "substrate", kagent}},
		instC: {Releases: []string{muster}},
	}}
	got := d.Targets(ourTeam, []string{instC})
	want := []Target{
		{Name: instA, Why: "carries bumblebee's agent-platform, muster, substrate +1"},
		{Name: instB, Why: "carries bumblebee's kagent"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("targets = %+v, want %+v", got, want)
	}
}

// A discovery says the installations beyond the configured ones that
// joined and left.
func TestDiscoveryLines(t *testing.T) {
	prev := &Discovery{Installations: map[string]Found{instA: {Releases: []string{muster}}, instB: {Releases: []string{kagent}}, instC: {Releases: []string{muster}}}}
	next := &Discovery{Installations: map[string]Found{instA: {Releases: []string{muster}}, instD: {Releases: []string{"substrate"}}}}
	got := DiscoveryLines(prev, next, ourTeam, []string{instC})
	want := []string{"ALERTS DISCOVERED delta carries bumblebee's substrate", "ALERTS UNDISCOVERED beta carries no HelmRelease of bumblebee any more"}
	if !slices.Equal(got, want) {
		t.Errorf("lines = %q, want %q", got, want)
	}
}
