package sandbox

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
)

func TestBrokerDir(t *testing.T) {
	home, run := testHome(t), t.TempDir()
	t.Setenv("XDG_STATE_HOME", "")
	live := filepath.Join(home, ".local", "state", "beekeeper")
	if got := BrokerDir(run, live, home); got != filepath.Join(run, "beekeeper") {
		t.Fatalf("default: %s", got)
	}
	first := filepath.Join(home, "a", "scratch")
	second := filepath.Join(home, "b", "scratch")
	a, b := BrokerDir(run, first, home), BrokerDir(run, second, home)
	if a == b || !strings.HasPrefix(a, filepath.Join(run, "beekeeper", "brokers", "scratch-")) {
		t.Fatalf("scratch paths: %s, %s", a, b)
	}
	if got := BrokerDir("", first, home); got != "" {
		t.Fatalf("without runtime: %s", got)
	}
	p := New(config.Sandbox{}, Paths{Home: home, StateDir: first, RuntimeDir: run})
	if p.Egress != filepath.Join(a, "egress") {
		t.Fatalf("policy egress: %s", p.Egress)
	}
	if !p.Readable(filepath.Join(p.Egress, EgressCA), home) || p.Readable(filepath.Join(b, "egress", EgressCA), home) {
		t.Fatal("policy does not isolate broker files")
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "custom-state"))
	if got := BrokerDir(run, filepath.Join(home, "custom-state", "beekeeper"), home); got != filepath.Join(run, "beekeeper") {
		t.Fatalf("XDG default: %s", got)
	}
}
