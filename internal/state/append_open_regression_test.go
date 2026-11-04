package state

import (
	"bytes"
	"os"
	"testing"
)

func TestUpdateKeepsNoteWhenEventLogCannotOpen(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(st *State) ([]Event, error) {
		st.Notes = []Note{{ID: 1, Text: "retain this question"}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.path("state.json"))
	if err != nil {
		t.Fatal(err)
	}
	// A directory fails OpenFile even when the test runs as root.
	if err := os.Mkdir(s.path("events.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	answer := func(st *State) ([]Event, error) {
		st.Notes = nil
		return []Event{{Verb: "answer.recorded", Detail: "retain this answer"}}, nil
	}
	if err := s.Update(answer); err == nil {
		t.Fatal("update accepted an unavailable event log")
	}
	after, err := os.ReadFile(s.path("state.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("failed log open changed the state: %v", err)
	}
	st, err := s.Read()
	if err != nil || len(st.Notes) != 1 || st.Notes[0].ID != 1 {
		t.Fatalf("the note is no longer answerable: %+v, %v", st, err)
	}
	if err := os.Remove(s.path("events.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(answer); err != nil {
		t.Fatal(err)
	}
	st, err = s.Read()
	if err != nil || len(st.Notes) != 0 {
		t.Fatalf("retry did not close the note: %+v, %v", st, err)
	}
	events, err := s.Events(0, nil)
	if err != nil || len(events) != 1 || events[0].Detail != "retain this answer" {
		t.Fatalf("retry did not retain the answer exactly once: %+v, %v", events, err)
	}
}
