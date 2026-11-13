package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// A paused derivation models slow ownership classification. Writers must finish
// before it resumes, and a conflicting delivery write must force recomputation.
func TestDeliveryObservationDoesNotLockDerivation(t *testing.T) {
	a, _ := supervisorApp(t)
	now := a.timeNow().UTC()
	seed(t, a, func(st *state.State) {
		for i := 0; i < 100; i++ {
			st.Deliveries = append(st.Deliveries, state.Delivery{
				Ref: fmt.Sprintf("o/r#%d", i+1), Step: state.DeliveryImplementing, Started: now, Since: now,
			})
		}
		for i := 0; i < 20; i++ {
			st.Agents = append(st.Agents, state.Agent{Party: state.Party{Name: fmt.Sprintf("worker-%d", i)}})
		}
	})
	entered, resume := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	calls := 0
	go func() {
		done <- loop.CommitDeliveryObservation(context.Background(), a.store, now, true, func(prev []state.Delivery) []state.Delivery {
			calls++
			if calls == 1 {
				close(entered)
				<-resume
			}
			return superloop.Deliveries(superloop.Input{Now: now}, prev)
		})
	}()
	<-entered
	const releaseTag = "v1.0.0"
	writer := make(chan error, 1)
	go func() {
		writer <- a.store.UpdateWithin(time.Second, func(st *state.State) ([]state.Event, error) {
			// Stand in for a start's roster/identity commit, a timer update and
			// a release verdict arriving while classification is paused.
			st.Agents = append(st.Agents, state.Agent{Party: state.Party{Name: "concurrent-start"}})
			st.Timers = append(st.Timers, state.Timer{ID: 1, Due: now})
			st.Deliveries[0].Step = state.DeliveryDone
			st.Deliveries[0].Verdict = VerdictAvailable
			st.Deliveries[0].Release = releaseTag
			st.Deliveries = append(st.Deliveries, state.Delivery{Ref: "o/r#201", Step: state.DeliveryImplementing, Started: now, Since: now})
			return nil, nil
		})
	}()
	writeErr := <-writer
	close(resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if writeErr != nil {
		t.Fatalf("writer blocked by derivation: %v", writeErr)
	}
	if calls != 2 {
		t.Fatalf("derivations = %d, want conflict retry", calls)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Agents) != 21 || len(st.Timers) != 1 || len(st.Deliveries) != 101 {
		t.Fatalf("concurrent state lost: agents=%d timers=%d deliveries=%d", len(st.Agents), len(st.Timers), len(st.Deliveries))
	}
	i := slices.IndexFunc(st.Deliveries, func(d state.Delivery) bool { return d.Ref == refOne })
	if i < 0 || st.Deliveries[i].Step != state.DeliveryDone || st.Deliveries[i].Release != releaseTag {
		t.Fatal("concurrent release verdict lost")
	}
	if !st.DeliveriesAt.Equal(now) {
		t.Fatalf("observation timestamp = %s, want %s", st.DeliveriesAt, now)
	}
}

func TestDeliveryObservationBoundsConflicts(t *testing.T) {
	a, _ := supervisorApp(t)
	calls := 0
	err := loop.CommitDeliveryObservation(context.Background(), a.store, a.timeNow(), true, func(prev []state.Delivery) []state.Delivery {
		calls++
		if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
			st.Deliveries = append(st.Deliveries, state.Delivery{Ref: fmt.Sprintf("o/r#%d", calls)})
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		return prev
	})
	if !errors.Is(err, loop.ErrDeliveryObservationChanged) || calls != 3 {
		t.Fatalf("conflicts: calls=%d err=%v", calls, err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Deliveries) != 3 || !st.DeliveriesAt.IsZero() {
		t.Fatal("failed observation overwrote concurrent writes or marked itself complete")
	}
}

func TestDeliveryObservationCancellationDoesNotCommit(t *testing.T) {
	a, _ := supervisorApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := loop.CommitDeliveryObservation(ctx, a.store, a.timeNow(), true, func([]state.Delivery) []state.Delivery {
		t.Fatal("cancelled observation ran derivation")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled observation: %v", err)
	}
}
