package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// FollowLast answers as Events(n, keep) does, reading each line once: a
// verb that was never logged costs only what was appended since.
func TestFollowLastReadsOnlyAppendedHistory(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Record(Event{Verb: "other"}, Event{Verb: "other"}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	keep := func(e Event) bool { calls++; return e.Verb == "wanted" }
	if got, err := FollowLast(s, "reader", 1, keep); err != nil || len(got) != 0 || calls != 2 {
		t.Fatalf("cold: %+v calls=%d err=%v", got, calls, err)
	}
	calls = 0
	if got, err := FollowLast(s, "reader", 1, keep); err != nil || len(got) != 0 || calls != 0 {
		t.Fatalf("unchanged log read again: %+v calls=%d err=%v", got, calls, err)
	}
	if err := s.Record(Event{Verb: "wanted", Detail: "1"}, Event{Verb: "other"}, Event{Verb: "wanted", Detail: "2"}); err != nil {
		t.Fatal(err)
	}
	calls = 0
	got, err := FollowLast(s, "reader", 1, keep)
	if err != nil || len(got) != 1 || got[0].Detail != "2" || calls != 3 {
		t.Fatalf("appended: %+v calls=%d err=%v", got, calls, err)
	}
	want, err := s.Events(1, keep)
	if err != nil || len(want) != 1 || want[0].Detail != got[0].Detail {
		t.Fatalf("FollowLast %+v, Events %+v (%v)", got, want, err)
	}
}

// A reader does not rewrite its checkpoint for every appended event; a
// restart from the older checkpoint still returns the whole projection.
func TestFollowSnapshotThrottlesCheckpoint(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Record(Event{Verb: projectionKeepVerb}); err != nil {
		t.Fatal(err)
	}
	keep := func(e Event) bool { return e.Verb == projectionKeepVerb }
	if _, err := s.FollowSnapshot("reader", "v1", keep); err != nil {
		t.Fatal(err)
	}
	checkpoint := func() eventCheckpoint {
		raw, err := os.ReadFile(filepath.Join(dir, "event-checkpoints", "reader.json"))
		if err != nil {
			t.Fatal(err)
		}
		var c eventCheckpoint
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	first := checkpoint()
	if err := s.Record(Event{Verb: projectionKeepVerb}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.FollowSnapshot("reader", "v1", keep); err != nil || len(got) != 2 {
		t.Fatalf("follow: %+v %v", got, err)
	}
	if c := checkpoint(); c.Offset != first.Offset || len(c.Kept) != len(first.Kept) {
		t.Fatalf("checkpoint republished for one event: %+v, was %+v", c, first)
	}
	restarted, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := restarted.FollowSnapshot("reader", "v1", keep); err != nil || len(got) != 2 {
		t.Fatalf("restart from the older checkpoint: %+v %v", got, err)
	}
}
