package cmd

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	unreadDeliveryPull   = "o/never#65"
	readableDeliveryPull = "o/r#66"
)

// An active delivery whose pull request lies in a repository the desk never
// reads is not tracked: the watcher never observes it, so it used to leave
// every pass, and every self-update --dry-run comparison, incomplete. A pull
// request of a readable repository is still observed.
func TestDeliveryPullsOutsideDeskAreNotTracked(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Supervisor.Ownership.Never = []string{"o/never"}
	r.pulls[readableDeliveryPull] = superloop.Pull{Ref: readableDeliveryPull, URL: "https://github.com/o/r/pull/66", State: superloop.Open, Head: loopHead, Green: true, Author: loopDesk}
	since := r.a.now.Add(-time.Hour)
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Deliveries = append(st.Deliveries,
			state.Delivery{Ref: "o/never#40", Pulls: []string{unreadDeliveryPull}, Step: state.DeliveryReviewQueued, Since: since, OutsideDesk: true},
			state.Delivery{Ref: "o/r#41", Pulls: []string{readableDeliveryPull}, Step: state.DeliveryReleasing, Since: since},
		)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var observed []string
	fixture := loop.ReadObservedLoopPull
	loop.ReadObservedLoopPull = func(l *loop.Loop, ctx context.Context, ref string, changed time.Time, step string) (loop.LoopPullObservation, error) {
		mu.Lock()
		observed = append(observed, ref)
		mu.Unlock()
		return fixture(l, ctx, ref, changed, step)
	}
	r.pass(t)
	mu.Lock()
	defer mu.Unlock()
	if slices.Contains(observed, unreadDeliveryPull) {
		t.Errorf("pass observed %s of a repository the desk never reads: %v", unreadDeliveryPull, observed)
	}
	if !slices.Contains(observed, readableDeliveryPull) {
		t.Errorf("pass did not observe the readable delivery pull %s: %v", readableDeliveryPull, observed)
	}
	if strings.Contains(r.out.String(), "cannot read "+unreadDeliveryPull) {
		t.Errorf("pass reported an unread observation for %s:\n%s", unreadDeliveryPull, r.out)
	}
}
