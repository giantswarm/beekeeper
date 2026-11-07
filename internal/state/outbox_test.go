package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

const outboxAnswerVerb = "answer.recorded"

type failedOutboxFile struct {
	*os.File
	stage string
}

func (f failedOutboxFile) Write(b []byte) (int, error) {
	if f.stage == "write" {
		return 0, errors.New("injected append failure")
	}
	if f.stage == "partial" {
		n, e := f.File.Write(b[:len(b)/2])
		if e != nil {
			return n, e
		}
		return n, errors.New("injected short append")
	}
	return f.File.Write(b)
}
func (f failedOutboxFile) Sync() error {
	if f.stage == "sync" {
		return errors.New("injected sync failure")
	}
	return f.File.Sync()
}
func (f failedOutboxFile) Close() error {
	err := f.File.Close()
	if f.stage == "close" {
		return errors.New("injected close failure")
	}
	return err
}

func TestEventOutboxRecoversEveryAppendBoundary(t *testing.T) {
	for _, stage := range []string{"write", "partial", "sync", "close", "crash-before-append", "crash-after-append"} {
		t.Run(stage, func(t *testing.T) {
			s, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Update(func(st *State) ([]Event, error) { st.Notes = []Note{{ID: 1, Text: "answer me"}}; return nil, nil }); err != nil {
				t.Fatal(err)
			}
			// Persist exactly the state and append that a closure commits. Reopening
			// must finish this outbox, even if a previous process wrote only a prefix.
			data := []byte("{\"verb\":\"answer.recorded\",\"detail\":\"the person's words\"}\n")
			st, _ := s.Read()
			st.Notes = nil
			st.EventOutbox = &EventOutbox{Data: data}
			if err := writeJSON(s.path("state.json"), st); err != nil {
				t.Fatal(err)
			}
			f, err := s.openEventLog()
			if err != nil {
				t.Fatal(err)
			}
			switch stage {
			case "crash-before-append":
				_ = f.Close()
			case "crash-after-append":
				if _, err := f.Write(data); err != nil {
					t.Fatal(err)
				}
				_ = f.Close()
			default:
				if err := s.flushEvents(st, failedOutboxFile{f, stage}); err == nil {
					t.Fatal("fault did not fail")
				}
				_ = f.Close()
			}
			reopened, err := Open(s.Dir())
			if err != nil {
				t.Fatal(err)
			}
			got, err := reopened.Read()
			if err != nil || len(got.Notes) != 0 || got.EventOutbox != nil {
				t.Fatalf("recovered state %+v: %v", got, err)
			}
			evs, err := reopened.Events(0, nil)
			if err != nil || len(evs) != 1 || evs[0].Detail != "the person's words" {
				t.Fatalf("answer lost or duplicated: %+v %v", evs, err)
			}
			if err := reopened.Update(func(*State) ([]Event, error) { return nil, nil }); err != nil {
				t.Fatal(err)
			}
			evs, _ = reopened.Events(0, nil)
			if len(evs) != 1 {
				t.Fatal("replayed twice")
			}
		})
	}
}

func TestEventOutboxRetainsEvidenceOnLogMismatch(t *testing.T) {
	s, _ := Open(t.TempDir())
	st := &State{EventOutbox: &EventOutbox{Data: []byte("expected\n")}}
	if err := writeJSON(s.path("state.json"), st); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path("events.jsonl"), []byte("other\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(); err == nil {
		t.Fatal("overwrote unrelated history")
	}
	raw, err := os.ReadFile(s.path("events.jsonl"))
	if err != nil || string(raw) != "other\n" {
		t.Fatal("changed history")
	}
}

func TestUpdateCommitsAndClearsEventOutbox(t *testing.T) {
	s, _ := Open(t.TempDir())
	if err := s.Update(func(st *State) ([]Event, error) {
		st.NextNote = 17
		return []Event{{At: time.Now(), Verb: outboxAnswerVerb, Detail: "yes"}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	st, err := s.Read()
	if err != nil || st.NextNote != 17 || st.EventOutbox != nil {
		t.Fatalf("state %+v %v", st, err)
	}
	evs, err := s.Events(0, nil)
	if err != nil || len(evs) != 1 {
		t.Fatalf("events %+v %v", evs, err)
	}
}

func TestUpdateRetainsAnswerAfterEventWriteFailure(t *testing.T) {
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("requires /dev/full")
	}
	s, _ := Open(t.TempDir())
	if err := s.Update(func(st *State) ([]Event, error) { st.Notes = []Note{{ID: 1, Text: "question"}}; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/full", s.path("events.jsonl")); err != nil {
		t.Fatal(err)
	}
	err := s.Update(func(st *State) ([]Event, error) {
		st.Notes = nil
		return []Event{{Verb: outboxAnswerVerb, Detail: "verbatim answer"}}, nil
	})
	if err == nil {
		t.Fatal("event write unexpectedly succeeded")
	}
	pending, err := s.Peek()
	if err != nil || pending.EventOutbox == nil || !bytes.Contains(pending.EventOutbox.Data, []byte("verbatim answer")) {
		t.Fatalf("committed closure lost its answer: %+v %v", pending, err)
	}
	if err := os.Remove(s.path("events.jsonl")); err != nil {
		t.Fatal(err)
	}
	reopened, _ := Open(s.Dir())
	st, err := reopened.Read()
	if err != nil || len(st.Notes) != 0 || st.EventOutbox != nil {
		t.Fatalf("reopen: %+v %v", st, err)
	}
	evs, err := reopened.Events(0, nil)
	if err != nil || len(evs) != 1 || evs[0].Detail != "verbatim answer" {
		t.Fatalf("recovered answer: %+v %v", evs, err)
	}
}

func TestPendingEventsRecoverBeforeAnotherWriterAppends(t *testing.T) {
	for _, method := range []string{"update", "log", "record"} {
		t.Run(method, func(t *testing.T) {
			s, _ := Open(t.TempDir())
			st := &State{NextNote: 11, EventOutbox: &EventOutbox{Data: []byte("{\"verb\":\"answer.recorded\",\"detail\":\"first\"}\n")}}
			if err := writeJSON(s.path("state.json"), st); err != nil {
				t.Fatal(err)
			}
			next := Event{Verb: outboxAnswerVerb, Detail: "second"}
			var err error
			switch method {
			case "update":
				err = s.Update(func(st *State) ([]Event, error) {
					if st.NextNote != 11 {
						t.Fatal("lost committed state")
					}
					return []Event{next}, nil
				})
			case "log":
				err = s.Log(next)
			case "record":
				err = s.Record(next)
			}
			if err != nil {
				t.Fatal(err)
			}
			evs, err := s.Events(0, nil)
			if err != nil || len(evs) != 2 || evs[0].Detail != "first" || evs[1].Detail != "second" {
				t.Fatalf("out of order: %+v %v", evs, err)
			}
		})
	}
}

func TestGuideWorkPreservesFutureFields(t *testing.T) {
	var st State
	raw := []byte(`{"guideWork":[{"id":1,"kind":"goal","futureAction":{"keep":true}}]}`)
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	st.GuideWork[0].Text = "next action"
	after, err := json.Marshal(st)
	if err != nil || !bytes.Contains(after, []byte(`"futureAction":{"keep":true}`)) {
		t.Fatalf("future work lost: %s %v", after, err)
	}
}
