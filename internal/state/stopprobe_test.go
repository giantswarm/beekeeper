package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStopProbeTracksAtomicSnapshotsWithoutLock(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	check := func(want bool) {
		t.Helper()
		got, err := s.StopRequested()
		if err != nil || got != want {
			t.Fatalf("stop = %v, %v; want %v", got, err, want)
		}
	}
	check(false)
	for _, stopped := range []bool{true, false, true} {
		if err := s.Update(func(st *State) ([]Event, error) {
			st.Stop = nil
			if stopped {
				st.Stop = &Stop{At: time.Now()}
			}
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		unlock, err := s.lock(false, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		check(stopped)
		check(stopped) // unchanged snapshot uses the cached intent
		unlock()
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), "state.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StopRequested(); err == nil {
		t.Fatal("malformed snapshot must fail closed")
	}
}
