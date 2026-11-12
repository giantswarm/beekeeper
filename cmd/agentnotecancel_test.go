package cmd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

type cancelingNoteStore struct {
	state.Store
	cancel context.CancelFunc
}

func (s cancelingNoteStore) EventsContext(ctx context.Context, n int, keep func(state.Event) bool) ([]state.Event, error) {
	s.cancel()
	return nil, ctx.Err()
}

func TestHandoverNoteLookupPropagatesShutdown(t *testing.T) {
	r := newLoopRig(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r.a.store = cancelingNoteStore{Store: r.a.store, cancel: cancel}
	_, found, err := r.a.lastNoteContext(ctx, state.Party{Name: holderParked}, time.Time{})
	if !errors.Is(err, context.Canceled) || found {
		t.Fatalf("note lookup = %t, %v", found, err)
	}
}

func TestContinuationShutdownDuringHistoryKeepsTaskAndDoesNotStartWorker(t *testing.T) {
	r, _ := fillRig(t)
	ag := state.Agent{Party: state.Party{Name: holderParked, Session: continuationOldSession}, Task: recoveryTask,
		Park: &state.Park{At: r.a.now, On: "#3", Continuation: []string{parkAnswer}}}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{ag}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r.a.store = cancelingNoteStore{Store: r.a.store, cancel: cancel}
	if err := l.ContinueParks(ctx, st); !errors.Is(err, context.Canceled) {
		t.Fatalf("continuation = %v", err)
	}
	for _, call := range r.calls {
		if len(call) > 1 && call[0] == agentsName && call[1] == agentStartName {
			t.Fatalf("started worker after shutdown: %v", call)
		}
	}
	after, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Agents) != 1 || after.Agents[0].Park == nil || after.Agents[0].Park.Continuing || len(after.Agents[0].Park.Continuation) != 1 {
		t.Fatalf("task changed during shutdown: %+v", after.Agents)
	}
}
