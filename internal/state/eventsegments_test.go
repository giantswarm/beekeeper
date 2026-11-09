package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const segmentFirstVerb = "segment-first"
const segmentSecondVerb = "segment-second"
const projectionKeepVerb = "projection-keep"
const segmentReplacement = "segment-replacement"

func TestSegmentedHistoryPreservesEventsAndFollowCursor(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Record(Event{Verb: segmentFirstVerb}); err != nil {
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
	if err := s.Record(Event{Verb: segmentSecondVerb}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Follow("segments", keep)
	if err != nil || len(got) != 2 || got[0].Verb != segmentFirstVerb || got[1].Verb != segmentSecondVerb || calls != 2 {
		t.Fatalf("rotation lost cursor or history: %+v calls=%d err=%v", got, calls, err)
	}
	all, err := s.Events(0, nil)
	if err != nil || len(all) != 2 {
		t.Fatalf("archived history: %+v %v", all, err)
	}
	last, err := s.Events(1, nil)
	if err != nil || len(last) != 1 || last[0].Verb != segmentSecondVerb {
		t.Fatalf("last event across segments: %+v %v", last, err)
	}
}

func TestFollowSnapshotResumesAfterRestartAndRotation(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Record(Event{Verb: projectionKeepVerb}, Event{Verb: "discard"}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	keep := func(e Event) bool { calls++; return e.Verb == projectionKeepVerb }
	if got, err := s.FollowSnapshot("reader", "v1", keep); err != nil || len(got) != 1 {
		t.Fatalf("cold projection: %+v %v", got, err)
	}
	if err := s.rotateEventsAbove(1); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(Event{Verb: projectionKeepVerb}); err != nil {
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
		t.Run(map[bool]string{false: segmentReplacement, true: "corruption"}[corrupt], func(t *testing.T) {
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
				raw, err := json.Marshal(Event{Verb: segmentReplacement})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, segmentReplacement), append(raw, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(filepath.Join(dir, segmentReplacement), s.path("events.jsonl")); err != nil {
					t.Fatal(err)
				}
			}
			restarted, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			want := segmentReplacement
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

func TestSlowHistoryReaderDoesNotStallOtherReaders(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Record(Event{Verb: "fixture"}); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	slow := make(chan error, 1)
	go func() {
		_, err := s.Follow("cold-watch", func(Event) bool {
			close(entered)
			<-release
			return false
		})
		slow <- err
	}()
	select {
	case <-entered:
	case err := <-slow:
		t.Fatalf("cold reader did not enter its filter: %v", err)
	}
	fast := make(chan error, 1)
	go func() {
		_, err := s.FollowSnapshot("supervisor", "v1", nil)
		fast <- err
	}()
	fastFinished := false
	t.Cleanup(func() {
		close(release)
		if err := <-slow; err != nil {
			t.Error(err)
		}
		if !fastFinished {
			if err := <-fast; err != nil {
				t.Error(err)
			}
		}
	})
	select {
	case err := <-fast:
		fastFinished = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		// The old store-wide scan mutex stalls the supervisor here without
		// ever invoking its progress callback. Cleanup releases that reader.
		t.Fatal("supervisor queued behind an unrelated cold history reader")
	}
}
