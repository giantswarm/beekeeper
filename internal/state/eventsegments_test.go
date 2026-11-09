package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSegmentedHistoryPreservesEventsAndFollowCursor(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Record(Event{Verb: "first"}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	keep := func(Event) bool { calls++; return true }
	if _, err := s.Follow("segments", keep); err != nil {
		t.Fatal(err)
	}
	if err := s.rotateEventsAbove(1); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(Event{Verb: "second"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Follow("segments", keep)
	if err != nil || len(got) != 2 || got[0].Verb != "first" || got[1].Verb != "second" || calls != 2 {
		t.Fatalf("rotation lost cursor or history: %+v calls=%d err=%v", got, calls, err)
	}
	all, err := s.Events(0, nil)
	if err != nil || len(all) != 2 {
		t.Fatalf("archived history: %+v %v", all, err)
	}
	last, err := s.Events(1, nil)
	if err != nil || len(last) != 1 || last[0].Verb != "second" {
		t.Fatalf("last event across segments: %+v %v", last, err)
	}
}

func TestFollowSnapshotResumesAfterRestartAndRotation(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Record(Event{Verb: "keep"}, Event{Verb: "discard"}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	keep := func(e Event) bool { calls++; return e.Verb == "keep" }
	if got, err := s.FollowSnapshot("reader", "v1", keep); err != nil || len(got) != 1 {
		t.Fatalf("cold projection: %+v %v", got, err)
	}
	if err := s.rotateEventsAbove(1); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(Event{Verb: "keep"}); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	calls = 0
	got, err := restarted.FollowSnapshot("reader", "v1", keep)
	if err != nil || len(got) != 2 || calls != 1 {
		t.Fatalf("replayed old segment after restart: %+v calls=%d err=%v", got, calls, err)
	}
	// A changed predicate revision must replay the source, not silently reuse
	// a projection that discarded events the new reader needs.
	calls = 0
	got, err = restarted.FollowSnapshot("reader", "v2", func(Event) bool { calls++; return true })
	if err != nil || len(got) != 3 || calls != 3 {
		t.Fatalf("revision reused incompatible cache: %+v calls=%d err=%v", got, calls, err)
	}
}

func TestFollowSnapshotRejectsReplacedLogAndCorruptCache(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "replacement", true: "corruption"}[corrupt], func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Record(Event{Verb: "old"}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.FollowSnapshot("reader", "v1", nil); err != nil {
				t.Fatal(err)
			}
			if corrupt {
				if err := os.WriteFile(filepath.Join(dir, "event-checkpoints", "reader.json"), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				raw, err := json.Marshal(Event{Verb: "replacement"})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "replacement"), append(raw, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(filepath.Join(dir, "replacement"), s.path("events.jsonl")); err != nil {
					t.Fatal(err)
				}
			}
			restarted, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			want := "replacement"
			if corrupt {
				want = "old"
			}
			got, err := restarted.FollowSnapshot("reader", "v1", nil)
			if err != nil || len(got) != 1 || got[0].Verb != want {
				t.Fatalf("cache hid source of truth: %+v %v", got, err)
			}
		})
	}
}
