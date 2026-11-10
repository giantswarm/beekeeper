package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestDeliveryAwakeElapsed(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	records := []machine.SuspendRecord{
		{At: start.Add(10 * time.Minute), Entry: true},
		{At: start.Add(6*time.Hour + 10*time.Minute)},
		{At: start.Add(6*time.Hour + 20*time.Minute), Entry: true},
		{At: start.Add(7 * time.Hour)},
	}
	for _, tc := range []struct {
		name string
		from time.Time
		want time.Duration
	}{
		{"whole step", start, 30 * time.Minute},
		{"step during sleep", start.Add(3 * time.Hour), 20 * time.Minute},
		{"step after resume", start.Add(7 * time.Hour), 10 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := deliveryAwakeElapsed(tc.from, start.Add(7*time.Hour+10*time.Minute), records); got != tc.want {
				t.Fatalf("awake = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestDeliveryHealthReadsSuspendBeforeResumeObserver(t *testing.T) {
	a, _ := supervisorApp(t)
	now := a.timeNow()
	original := plat.Machine
	plat.Machine = &suspendFixture{Machine: original, records: []machine.SuspendRecord{
		{At: now.Add(-6*time.Hour - 10*time.Minute), Entry: true},
		{At: now.Add(-10 * time.Minute)},
	}}
	t.Cleanup(func() { plat.Machine = original })
	a.cfg.Delivery.StallAfter.Duration = time.Hour
	a.cfg.Delivery.WIPLimit = 1
	seed(t, a, func(st *state.State) {
		st.Agents = []state.Agent{{Party: state.Party{Name: "awake-worker"}, Task: deliveryTestTask}}
		st.Deliveries = []state.Delivery{{Ref: deliveryControlPendingRef, Agent: "awake-worker", Step: state.DeliveryCI, Since: now.Add(-6*time.Hour - 20*time.Minute), Started: now.Add(-7 * time.Hour), Waits: deliveryControlChecks}}
	})
	// No standby suspend observer ran. The supervisor's first pass must still
	// avoid filing either a delivery stall or a full-WIP alert for sleep.
	if line, err := a.checkDeliveries(now); err != nil || line != "" {
		t.Fatalf("post-resume: %q, %v", line, err)
	}
	// Reconstructing health in another command must retain the same result.
	copy := *a
	if line, err := copy.checkDeliveries(now.Add(time.Minute)); err != nil || line != "" {
		t.Fatalf("restarted observer: %q, %v", line, err)
	}
	if line, err := copy.checkDeliveries(now.Add(time.Hour)); err != nil || !strings.Contains(line, "DELIVERY STALLED") || !strings.Contains(line, "WIP FULL") {
		t.Fatalf("real awake stall: %q, %v", line, err)
	}
}
