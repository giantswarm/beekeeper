package state

import (
	"context"
	"errors"
	"testing"
)

func TestEventsContextStopsDuringReplayWithoutReturningPartialHistory(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := s.Record(Event{Verb: "cancel-fixture"}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	events, err := s.EventsContext(ctx, 1, func(Event) bool {
		calls++
		cancel()
		return true
	})
	if !errors.Is(err, context.Canceled) || len(events) != 0 || calls != 1 {
		t.Fatalf("replay = %v, %v; decoded %d events", events, err, calls)
	}
	// Cancellation is local to the read; legacy callers still see all events.
	events, err = s.Events(0, nil)
	if err != nil || len(events) != 3 {
		t.Fatalf("subsequent replay = %v, %v", events, err)
	}
	snapshotCtx, snapshotCancel := context.WithCancel(t.Context())
	defer snapshotCancel()
	snapshotCalls := 0
	events, err = s.FollowSnapshotContext(snapshotCtx, "cancel-reader", "v1", func(Event) bool {
		snapshotCalls++
		snapshotCancel()
		return true
	})
	if !errors.Is(err, context.Canceled) || len(events) != 0 || snapshotCalls != 1 {
		t.Fatalf("snapshot replay = %v, %v; decoded %d events", events, err, snapshotCalls)
	}
	events, err = s.FollowSnapshot("cancel-reader", "v1", nil)
	if err != nil || len(events) != 3 {
		t.Fatalf("subsequent snapshot = %v, %v", events, err)
	}
}

func TestEventsContextCanceledBeforeOpeningHistory(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.EventsContext(ctx, 0, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("replay = %v", err)
	}
}
