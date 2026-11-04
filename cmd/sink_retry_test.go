package cmd

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/state"
)

// Cancellation during the first dispatch leaves every failed event in the
// claimed batch available to a different watch, including events the
// canceled batch could not send.
func TestTheSinkReleasesFailedClaimsAfterCancellation(t *testing.T) {
	a, _ := sinkApp(t, "sink: {repo: o/site, workflow: refresh.yaml, verbs: [merged]}\n")
	w := a.newWatcher(false, true)
	sinkPoll(w)
	evs := []state.Event{
		about(event(sinkParty, verbMerged, "o/r#7 exit 0, release v1.2.0"), decisionRef),
		about(event(sinkParty, verbMerged, "o/r#8 exit 0, release v1.2.1"), mergedRef),
	}
	if err := a.store.Log(evs...); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prev := sinkGH
	t.Cleanup(func() { sinkGH = prev })
	sinkGH = func(ctx context.Context, _ ...string) ([]byte, error) {
		cancel()
		return nil, ctx.Err()
	}
	w.dispatch(ctx, a.cfg.Sink, evs)
	if ctx.Err() == nil {
		t.Fatal("the dispatch did not cancel its watch")
	}

	// Open the state again as a new process would, so recovery depends on
	// the persisted ledger rather than the old watch's pending events.
	other := *a
	var err error
	other.store, err = state.Open(a.store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	var subjects []string
	sinkGH = func(_ context.Context, args ...string) ([]byte, error) {
		for _, arg := range args {
			if subject, ok := strings.CutPrefix(arg, "subject="); ok {
				subjects = append(subjects, subject)
			}
		}
		return nil, nil
	}
	restarted := other.newWatcher(false, true)
	sinkPoll(restarted)
	if want := []string{decisionRef, mergedRef}; !slices.Equal(subjects, want) {
		t.Fatalf("a restarted watch dispatched %q, want failed events %q", subjects, want)
	}
	sinkPoll(restarted)
	if len(subjects) != len(evs) {
		t.Fatalf("successfully retried events were dispatched again: %q", subjects)
	}
}
