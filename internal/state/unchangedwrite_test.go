package state

import (
	"os"
	"testing"
)

// A save that changes nothing keeps state.json's identity, so snapshot caches
// stay valid and change watchers are not woken; a real change still replaces it.
func TestUpdateWithoutChangeKeepsStateFile(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(st *State) ([]Event, error) { st.Notes = []Note{{ID: 1, Text: "kept"}}; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(s.path("state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(*State) ([]Event, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(s.path("state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !sameSnapshot(before, after) {
		t.Fatal("an update without a change replaced state.json")
	}
	if err := s.Update(func(st *State) ([]Event, error) { st.Notes[0].Text = "changed"; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	changed, err := os.Stat(s.path("state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(after, changed) {
		t.Fatal("a changed update kept the old state.json")
	}
	st, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Notes) != 1 || st.Notes[0].Text != "changed" {
		t.Fatalf("notes = %+v", st.Notes)
	}
}

// Events committed with an unchanged state still reach the log, and the state
// file ends without an outbox.
func TestUpdateWithEventsAndNoChangeAppends(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(*State) ([]Event, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(*State) ([]Event, error) { return []Event{{Verb: outboxAnswerVerb}}, nil }); err != nil {
		t.Fatal(err)
	}
	evs, err := s.Events(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Verb != outboxAnswerVerb {
		t.Fatalf("events = %+v", evs)
	}
	pending, err := s.outboxPending()
	if err != nil || pending {
		t.Fatalf("outbox pending=%v err=%v", pending, err)
	}
}
