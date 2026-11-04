package state

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStaleWriterCannotChangeSideFilesThroughUpdate(t *testing.T) {
	dir := t.TempDir()
	newer, err := OpenVersion(dir, "v2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := newer.Update(func(st *State) ([]Event, error) {
		st.NextNote = 17
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	older, err := OpenVersion(dir, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	sideFile := filepath.Join(dir, "claimed-lease")
	err = older.Update(func(st *State) ([]Event, error) {
		// Real lease claims and handovers write their holder files here.
		if err := os.WriteFile(sideFile, []byte("new holder"), 0o600); err != nil {
			return nil, err
		}
		st.NextNote++
		return []Event{{Verb: "lease.claim"}}, nil
	})
	var stale *StaleWriterError
	if !errors.As(err, &stale) {
		t.Fatalf("update = %v, want a stale writer error", err)
	}
	if _, err := os.Stat(sideFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused writer changed its side file: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("a refused writer changed the state: %v", err)
	}
	events, err := newer.Events(0, nil)
	if err != nil || len(events) != 1 || events[0].Verb != VerbStaleWriter {
		t.Fatalf("refused update log: %+v, %v", events, err)
	}
}
