package cmd

import (
	"errors"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// Count even swallowed audit failures, and refuse before reaching the real
// exclusive lock so a regression fails without a two-minute lock timeout.
type sharedCommandStore struct {
	state.Store
	writes int
}

func (s *sharedCommandStore) Update(func(*state.State) ([]state.Event, error)) error {
	return s.denyWrite()
}

func (s *sharedCommandStore) UpdateWithin(time.Duration, func(*state.State) ([]state.Event, error)) error {
	return s.denyWrite()
}

func (s *sharedCommandStore) Exclusive(func() error) error { return s.denyWrite() }

func (s *sharedCommandStore) Log(...state.Event) error { return s.denyWrite() }

func (s *sharedCommandStore) Record(...state.Event) error { return s.denyWrite() }

func (s *sharedCommandStore) RecordWithin(time.Duration, ...state.Event) error { return s.denyWrite() }

func (s *sharedCommandStore) denyWrite() error {
	s.writes++
	return errors.New("read-only command attempted an exclusive state update")
}

func TestIdleCommandsDoNotRequestExclusiveStateLock(t *testing.T) {
	a, _ := stubApp(t)
	plat.Machine = tableMachine{plat.Machine}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide = &state.Role{WatchSeen: a.now}
		st.GuideReportsSince = a.now
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	store := &sharedCommandStore{Store: a.store}
	a.store = store
	for range 20 {
		if lines, err := a.guideFeed(t.Context()); err != nil || len(lines) != 0 {
			t.Fatalf("idle guide: %q, %v", lines, err)
		}
		if err := a.agentList(true); err != nil {
			t.Fatal(err)
		}
		c := a.capacityCmd()
		c.SetContext(t.Context())
		if err := c.RunE(c, nil); err != nil {
			t.Fatal(err)
		}
		a.now = a.now.Add(100 * time.Millisecond)
	}
	if store.writes != 0 {
		t.Fatalf("idle commands requested %d state writes", store.writes)
	}
	// Heartbeats and newly acknowledged feed entries still require a durable
	// update; only polls with nothing to record stay on the read path.
	a.now = a.now.Add(time.Minute)
	if _, err := a.guideFeed(t.Context()); err == nil || store.writes != 1 {
		t.Fatalf("guide heartbeat lost its durable update: %v, writes=%d", err, store.writes)
	}
}
