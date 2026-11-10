package state

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLockTimingRecordsReleasedHolds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "holds.jsonl")
	t.Setenv("BEEKEEPER_STATE_LOCK_TRACE", path)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, shared := range []bool{false, true} {
		unlock, err := s.lock(shared, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		unlock()
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var holds []lockTiming
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var h lockTiming
		if err := json.Unmarshal(line, &h); err != nil {
			t.Fatal(err)
		}
		holds = append(holds, h)
	}
	if len(holds) != 2 || holds[0].Shared || !holds[1].Shared {
		t.Fatalf("holds: %+v", holds)
	}
	for _, h := range holds {
		if h.PID != os.Getpid() || h.HoldNS <= 0 || h.Released.Before(h.Acquired) {
			t.Fatalf("invalid timing: %+v", h)
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("trace permissions: %v %v", info, err)
	}
}

func TestLockTimingFailureDoesNotKeepLock(t *testing.T) {
	dir := t.TempDir()
	// An absent parent makes the optional trace unwritable.
	t.Setenv("BEEKEEPER_STATE_LOCK_TRACE", filepath.Join(dir, "missing", "trace"))
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := s.lock(false, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	unlock, err = s.lock(false, time.Second)
	if err != nil {
		t.Fatalf("trace failure retained lock: %v", err)
	}
	unlock()
}
