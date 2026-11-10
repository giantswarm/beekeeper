package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/state"
)

// Timing belongs to the dedicated CI job, outside the parallel unit matrix.
// Machine and provider I/O are isolated; the real file store, JSON command,
// cold state decode and cross-command activity projection remain in the path.
func TestCapacityLargeSnapshotLatency(t *testing.T) {
	if os.Getenv("BEEKEEPER_TEST_LARGE_HISTORY") != "1" {
		t.Skip("dedicated state performance CI job")
	}
	original := plat
	plat = platform.Stub()
	plat.Machine = roomyMachine{Machine: plat.Machine}
	t.Cleanup(func() { plat = original })
	dir := t.TempDir()
	cfgPath := emptyTestConfig(t, filepath.Join(dir, "config.yaml"))
	t.Setenv("HOME", dir)
	t.Setenv("BEEKEEPER_CONFIG", cfgPath)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("MEMCAP_STATE", filepath.Join(dir, "memcap"))
	t.Setenv("PATH", dir)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenVersion(cfg.StateDir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		for i := range 6500 {
			st.Notes = append(st.Notes, state.Note{ID: i, Text: strings.Repeat("x", 900)})
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	line := "{\"verb\":\"fixture\",\"detail\":\"" + strings.Repeat("x", 900) + "\"}\n"
	if err := os.WriteFile(filepath.Join(store.Dir(), "events.jsonl"), []byte(strings.Repeat(line, 10000)), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		reopened, err := state.OpenVersion(store.Dir(), "dev")
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		a := &app{cfgPath: cfgPath, cfg: cfg, store: reopened, now: time.Now(), out: &out, json: true}
		started := time.Now()
		err = a.capacityCmd().Execute()
		elapsed := time.Since(started)
		if err != nil || !json.Valid(out.Bytes()) {
			t.Fatalf("capacity command: %v %s", err, out.String())
		}
		t.Logf("capacity instance %d: %s", i, elapsed)
		if elapsed >= time.Second {
			t.Fatalf("capacity exceeded one second: %s", elapsed)
		}
	}
}
