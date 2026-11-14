package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/state"
)

// Regression for https://github.com/giantswarm/beekeeper/issues/1980 (fails
// on the base): a fill-rate forecast with free space above the floor held
// every start and filed a decision note for the person. The watch says
// DISK FILLING from the write rate; only free space under the floor is the
// emergency. On the base the loop reads the forecast as Low: starts stay
// held, a note for the person appears.
func TestLoopWatchFillForecastAloneNeverHoldsStarts(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Watch.DiskFillWithin = config.Duration{Duration: 2 * time.Hour}
	w := r.a.newWatcher(false, false)
	total := 1900 * gibMiB // floor 95 GiB; free stays above it while falling fast
	for i := range 5 {
		now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC).Add(time.Duration(i) * 30 * time.Second)
		w.SampleDisk(now, machine.Disk{FreeMiB: 100*gibMiB - i*1229, TotalMiB: total}, nil, nil)
	}
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.Once = true // These checks inspect completed pass effects.
	for range 3 {
		if err := l.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
		if l.StartsHeld {
			t.Fatal("a fill forecast over the floor held starts")
		}
	}
	if r.called(loop.LoopDiskKind, "prune", "--apply") != nil {
		t.Fatal("a fill forecast over the floor pruned")
	}
	st, _ := r.a.store.Read()
	if len(st.Notes) != 0 {
		t.Fatalf("a fill forecast noted the person: %+v", st.Notes)
	}
	evs, err := r.a.store.Events(0, func(e state.Event) bool { return e.Verb == loop.LoopFileVerb })
	if err != nil || len(evs) != 0 {
		t.Fatalf("a fill forecast filed a finding: %+v %v", evs, err)
	}
}
