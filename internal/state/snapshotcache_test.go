package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSnapshotCacheCopiesAndInvalidates(t *testing.T) {
	s, err := OpenVersion(t.TempDir(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Dir(), "state.json")
	seed := func(raw string) {
		t.Helper()
		var st State
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(path, &st); err != nil {
			t.Fatal(err)
		}
	}
	seed(`{"nextNote":4,"notes":[{"id":1,"text":"original","future":{"keep":true}}],"productionNotes":{"1":"2026-01-01T00:00:00Z"},"eventOutbox":{"offset":0,"data":"eA=="},"futureState":true}`)
	first, err := s.Peek()
	if err != nil {
		t.Fatal(err)
	}
	cached := s.snapshot
	first.Notes[0].Text = "changed"
	delete(first.ProductionNotes, 1)
	first.EventOutbox.Data[0] = 'y'
	second, err := s.Peek()
	if err != nil || second.Notes[0].Text != "original" || len(second.ProductionNotes) != 1 || string(second.EventOutbox.Data) != "x" {
		t.Fatalf("cached state escaped: %+v %v", second, err)
	}
	if cached != s.snapshot {
		t.Fatal("unchanged inode was decoded again")
	}
	raw, err := json.Marshal(second)
	if err != nil || !strings.Contains(string(raw), `"futureState":true`) || !strings.Contains(string(raw), `"future":{"keep":true}`) {
		t.Fatalf("unknown fields lost: %s %v", raw, err)
	}
	seed(`{"nextNote":5}`)
	third, err := s.Read()
	if err != nil || third.NextNote != 5 || cached == s.snapshot {
		t.Fatalf("replacement was not observed: %+v %v", third, err)
	}
	if err := os.WriteFile(path, []byte(`{"nextNote":6}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fourth, err := s.Read()
	if err != nil || fourth.NextNote != 6 {
		t.Fatalf("in-place change was not observed: %+v %v", fourth, err)
	}
	if err := os.WriteFile(path, []byte(`broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(); err == nil {
		t.Fatal("served stale cache after corrupt replacement")
	}
}

func TestSnapshotCacheDoesNotRetainFailedTransaction(t *testing.T) {
	s, err := OpenVersion(t.TempDir(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(st *State) ([]Event, error) { st.NextNote = 7; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("abort")
	if err := s.Update(func(st *State) ([]Event, error) { st.NextNote = 8; return nil, failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	st, err := s.Read()
	if err != nil || st.NextNote != 7 {
		t.Fatalf("aborted transaction mutated cache: %+v %v", st, err)
	}
}

func BenchmarkUnchangedSnapshot(b *testing.B) {
	s, err := OpenVersion(b.TempDir(), "dev")
	if err != nil {
		b.Fatal(err)
	}
	st := &State{}
	for i := range 6000 {
		st.Notes = append(st.Notes, Note{ID: i, Text: strings.Repeat("x", 900)})
	}
	if err := writeJSON(filepath.Join(s.Dir(), "state.json"), st); err != nil {
		b.Fatal(err)
	}
	if _, err := s.Peek(); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := s.Peek(); err != nil {
			b.Fatal(err)
		}
	}
}
