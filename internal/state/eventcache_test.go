package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEventsCacheAppendRotateReplaceAndPartialLine(t *testing.T) {
	s, err := OpenVersion(t.TempDir(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Dir(), "events.jsonl")
	write := func(raw string, appendMode bool) {
		t.Helper()
		flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		if appendMode {
			flags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
		}
		f, err := os.OpenFile(path, flags, 0o600) //nolint:gosec // fixture path under t.TempDir
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(raw); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	read := func(want int) []Event {
		t.Helper()
		events, err := s.Events(0, nil)
		if err != nil || len(events) != want {
			t.Fatalf("Events: %+v %v; want %d", events, err, want)
		}
		return events
	}
	write("{\"verb\":\"first\",\"noteSnapshot\":{\"id\":1,\"text\":\"original\"}}\n", false)
	read(1)[0].NoteSnapshot.Text = "mutated"
	cache := s.events[0]
	if read(1)[0].NoteSnapshot.Text != "original" || s.events[0] != cache {
		t.Fatal("cache was mutated or replaced without an append")
	}
	write(`{"verb":"second"}`, true)
	read(2)
	read(2)
	write("\n{\"verb\":", true)
	read(2)
	write("\"third\"}\n", true)
	read(3)
	if err := s.rotateEventsAbove(1); err != nil {
		t.Fatal(err)
	}
	write("{\"verb\":\"fourth\"}\n", false)
	read(4)
	if s.events[0] != cache {
		t.Fatal("rotation replayed sealed history")
	}
	last, err := s.Events(1, func(e Event) bool { return e.Verb != "fourth" })
	if err != nil || len(last) != 1 || last[0].Verb != "third" {
		t.Fatalf("last matching event: %+v %v", last, err)
	}
	write("{\"verb\":\"new\"}\n", false)
	if read(4)[3].Verb != "new" {
		t.Fatal("truncation retained old events")
	}
}

func BenchmarkCachedEvents(b *testing.B) {
	s, err := OpenVersion(b.TempDir(), "dev")
	if err != nil {
		b.Fatal(err)
	}
	line := "{\"verb\":\"fixture\",\"detail\":\"" + strings.Repeat("x", 900) + "\"}\n"
	if err := os.WriteFile(filepath.Join(s.Dir(), "events.jsonl"), []byte(strings.Repeat(line, 10000)), 0o600); err != nil {
		b.Fatal(err)
	}
	keep := func(Event) bool { return false }
	if _, err := s.Events(0, keep); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := s.Events(0, keep); err != nil {
			b.Fatal(err)
		}
	}
}
