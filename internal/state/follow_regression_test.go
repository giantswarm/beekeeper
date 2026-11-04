package state

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const followReport = "agents.report"

const followAfter = "after"

func TestFollowReadsLargeEventsAndContinuesPastThem(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const readerName = "large-events"
	keep := func(e Event) bool { return e.Verb != followReport }
	if err := s.Record(Event{Verb: "before"}); err != nil {
		t.Fatal(err)
	}
	if events, err := s.Follow(readerName, keep); err != nil || len(events) != 1 {
		t.Fatalf("before the report: %d events, %v", len(events), err)
	}
	report := Event{Verb: followReport, Detail: strings.Repeat("large report\n", 100000)}
	if err := s.Record(report, Event{Verb: followAfter}); err != nil {
		t.Fatal(err)
	}
	// Even a reader whose filter rejects the report must get past it.
	for range 2 {
		events, err := s.Follow(readerName, keep)
		if err != nil || len(events) != 2 || events[0].Verb != "before" || events[1].Verb != followAfter {
			t.Fatalf("past the report: %d events, %v", len(events), err)
		}
	}
	events, err := s.Follow("full-large-events", nil)
	if err != nil || len(events) != 3 || events[1].Detail != report.Detail {
		t.Fatalf("full report: %d events, %v", len(events), err)
	}
}

func TestFollowLeavesLargePartialEventsForTheNextRead(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	event := Event{Verb: followReport, Detail: strings.Repeat("x", 2<<20)}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(s.path("events.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(raw); err != nil {
		t.Fatal(err)
	}
	const readerName = "partial-large-event"
	if events, err := s.Follow(readerName, nil); err != nil || len(events) != 0 {
		t.Fatalf("unterminated report: %d events, %v", len(events), err)
	}
	if _, err := f.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(Event{Verb: followAfter}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		events, err := s.Follow(readerName, nil)
		if err != nil || len(events) != 2 || events[0].Detail != event.Detail || events[1].Verb != followAfter {
			t.Fatalf("completed report: %d events, %v", len(events), err)
		}
	}
}
