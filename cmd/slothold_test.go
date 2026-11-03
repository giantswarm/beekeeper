package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/config"
)

// A run holding a build slot past memcap.holdBound is one SLOT HELD LONG
// line naming the slot, its holder, since when and the command, folded
// while it lasts, and one ENDED line once the slot is free.
func TestWatchSaysASlotHeldLong(t *testing.T) {
	t.Setenv("MEMCAP_STATE", "")
	t.Setenv("MEMCAP_SLOTS", "")
	dir := t.TempDir()
	slots := filepath.Join(dir, "slots")
	if err := os.MkdirAll(slots, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("stateDir: "+dir+"\nmemcap: {slotDir: "+slots+", slots: 2}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Memcap.HoldBound.Duration != 30*time.Minute {
		t.Fatalf("memcap.holdBound defaults to %s, want 30m", cfg.Memcap.HoldBound.Duration)
	}
	l := flock.New(filepath.Join(slots, "1.lock"))
	if ok, err := l.TryLock(); err != nil || !ok {
		t.Fatalf("lock: %v", err)
	}
	defer func() { _ = l.Unlock() }()
	rec := `{"pid":4711,"session":"s1","name":"Agent four","cmd":"python3 benchmark_preset.py","max":"12G","since":"2026-10-09T20:27:00Z"}`
	if err := os.WriteFile(filepath.Join(slots, "1.holder"), []byte(rec+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	w := &watcher{app: &app{cfg: cfg, out: &out}, last: map[string]time.Time{}}

	w.now = time.Date(2026, 10, 9, 20, 50, 0, 0, time.UTC)
	w.slotHolds()
	if out.Len() != 0 {
		t.Errorf("a slot held 23m, under the bound, was said:\n%s", out.String())
	}
	w.now = time.Date(2026, 10, 9, 23, 20, 0, 0, time.UTC)
	w.slotHolds()
	w.slotHolds()
	want := "SLOT HELD LONG: slot 1 past memcap.holdBound (30m), held by Agent four (pid 4711) since 20:27Z (2h53m0s): python3 benchmark_preset.py"
	if n := strings.Count(out.String(), want); n != 1 {
		t.Errorf("want %q once, said %d times:\n%s", want, n, out.String())
	}
	if !strings.Contains(out.String(), "--class benchmark|lab") {
		t.Errorf("the line does not name the slotless classes:\n%s", out.String())
	}
	_ = l.Unlock()
	w.slotHolds()
	if !strings.Contains(out.String(), "ENDED SLOT HELD LONG") {
		t.Errorf("a freed slot does not end the condition:\n%s", out.String())
	}
}
