//go:build linux

package alerts

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestEnsureContexts(t *testing.T) {
	dir := t.TempDir()
	tsh, kubectl := filepath.Join(dir, "tsh"), filepath.Join(dir, "kubectl")
	write(t, tsh, `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_CALLS"
case "$*" in
 "kube ls --format json") echo '[{"kube_cluster_name":"alpha"},{"kube_cluster_name":"beta"},{"kube_cluster_name":"broken"},{"kube_cluster_name":"empty"}]';;
 "kube login broken") exit 1;;
 "kube login empty") exit 0;;
 "kube login "*) echo "ctx-$3" >> "$KUBECONFIG";;
 *) exit 2;;
esac
`)
	write(t, kubectl, fakeDiscoverKubectl)
	calls := filepath.Join(dir, "calls")
	t.Setenv("FAKE_CALLS", calls)
	machine, own := filepath.Join(dir, "machine"), filepath.Join(dir, "kube", "keeper")
	write(t, machine, "ctx-alpha\nctx-beta\n")
	t.Setenv("KUBECONFIG", machine)
	d := Discoverer{Tsh: tsh, Kubectl: kubectl, Login: own, Kubeconfig: []string{machine, own}, Template: func(s string) string { return "ctx-" + s }, Exclude: []string{"beta"}}
	targets := []Target{{Name: "alpha"}, {Name: "beta"}, {Name: "missing"}, {Name: "broken"}, {Name: "empty"}}
	want := []string{"missing: not listed by Teleport", "broken: tsh kube login failed", "empty: login wrote no matching kube context"}
	if got := d.EnsureContexts(context.Background(), targets); !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	raw, err := os.ReadFile(own)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "ctx-alpha\nctx-beta\n" {
		t.Fatalf("keeper contexts: %q", raw)
	}
	raw, err = os.ReadFile(machine)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "ctx-alpha\nctx-beta\n" {
		t.Fatalf("machine changed: %q", raw)
	}
	if got := d.EnsureContexts(context.Background(), targets[:2]); len(got) != 0 {
		t.Fatal(got)
	}
	raw, err = os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "beta"} {
		if strings.Count(string(raw), "kube login "+name+"\n") != 1 {
			t.Fatalf("login not idempotent: %s", raw)
		}
	}
}

func TestEnsureContextsReportsSetupFailures(t *testing.T) {
	targets := []Target{{Name: "alpha"}, {Name: "beta"}}
	d := Discoverer{}
	if got := d.EnsureContexts(context.Background(), targets); !slices.Equal(got, []string{"alpha: no keeper kubeconfig configured", "beta: no keeper kubeconfig configured"}) {
		t.Fatal(got)
	}
	d.Login = filepath.Join(t.TempDir(), "keeper")
	d.Tsh = filepath.Join(t.TempDir(), "absent")
	if got := d.EnsureContexts(context.Background(), targets); !slices.Equal(got, []string{"alpha: could not list Teleport clusters", "beta: could not list Teleport clusters"}) {
		t.Fatal(got)
	}
}

// A failed context listing must not make an explicit context look present.
func TestEnsureContextsVerifiesExplicitContext(t *testing.T) {
	dir := t.TempDir()
	tsh := filepath.Join(dir, "tsh")
	write(t, tsh, `#!/bin/sh
case "$*" in
 "kube ls --format json") echo '[{"kube_cluster_name":"alpha"}]';;
 "kube login alpha") exit 0;;
 *) exit 2;;
esac
`)
	d := Discoverer{Tsh: tsh, Kubectl: filepath.Join(dir, "absent"), Login: filepath.Join(dir, "keeper"), Template: func(string) string { return "" }}
	got := d.EnsureContexts(context.Background(), []Target{{Name: "alpha", Context: "custom"}})
	if !slices.Equal(got, []string{"alpha: login wrote no matching kube context"}) {
		t.Fatal(got)
	}
}
