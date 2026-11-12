package cmd

import (
	"context"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestUsageRefreshSharedByCommandsAndWatch(t *testing.T) {
	const probeProvider = "probe-provider"
	store, err := state.OpenVersion(t.TempDir(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	file := usageProviderFile(probeProvider)
	u := config.Usage{Command: []string{"fixture-usage"}}
	old := capacity.UsageReading{At: now.Add(-6 * time.Minute), FiveHour: &capacity.UsageWindow{UsedPercent: 99, ResetsAt: now.Add(time.Hour)}}
	if err := store.WriteFile(file, old); err != nil {
		t.Fatal(err)
	}
	entered, finish := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	probe := func(ctx context.Context, _ []string, _ time.Duration) (capacity.UsageReading, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-ctx.Done():
			return capacity.UsageReading{}, ctx.Err()
		case <-finish:
			return capacity.UsageReading{FiveHour: &capacity.UsageWindow{UsedPercent: 20, ResetsAt: now.Add(time.Hour)}}, nil
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	owner := &app{store: store, now: now, readUsage: probe}
	done := make(chan error, 1)
	go func() { _, err := owner.resolveReading(ctx, file, u); done <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("owner did not enter probe")
	}
	for i := range 16 {
		// Separate stores model separate short-lived command instances.
		other, err := state.OpenVersion(store.Dir(), "dev")
		if err != nil {
			t.Fatal(err)
		}
		caller := &app{store: other, now: now, readUsage: probe}
		var r capacity.UsageReading
		if i%2 == 0 {
			r, err = caller.resolveReading(ctx, file, u)
		} else {
			w := &watcher{app: caller}
			r, err = w.execUsageNow(ctx, file, u)
		}
		if err != nil || r.FiveHour == nil || r.FiveHour.UsedPercent != 99 || r.Fallback == "" {
			t.Fatalf("contender lost existing quota limit: %+v %v", r, err)
		}
	}
	w := &watcher{app: &app{store: store, now: now, readUsage: probe}, chores: true}
	w.refreshUsage(ctx, config.Capacity{Providers: map[string]config.Provider{probeProvider: {Usage: u}}})
	if w.providerUsageErr[probeProvider] != "" {
		t.Fatalf("contended probe recorded as provider usage failure: %q", w.providerUsageErr[probeProvider])
	}
	close(finish)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	reader := &app{store: store, now: time.Now(), readUsage: probe}
	r, err := reader.resolveReading(ctx, file, u)
	if err != nil || r.FiveHour.UsedPercent != 20 || calls.Load() != 1 {
		t.Fatalf("probe was not shared for its lifetime: %+v calls=%d %v", r, calls.Load(), err)
	}
}

func TestProbeLockAcrossProcesses(t *testing.T) {
	const key = "BEEKEEPER_TEST_PROBE_LOCK_DIR"
	const lockName = "capacity-host-fixture.lock"
	if dir := os.Getenv(key); dir != "" {
		store, err := state.OpenVersion(dir, "dev")
		if err != nil {
			t.Fatal(err)
		}
		a := &app{store: store}
		if unlock, err := a.probeLock(lockName); err == nil {
			unlock()
			t.Fatal("second process admitted for the same host")
		}
		unlock, err := a.probeLock("capacity-host-another.lock")
		if err != nil {
			t.Fatal("unrelated host blocked:", err)
		}
		unlock()
		return
	}
	store, err := state.OpenVersion(t.TempDir(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	a := &app{store: store}
	unlock, err := a.probeLock(lockName)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(t.Context(), bin, "-test.run=^TestProbeLockAcrossProcesses$") //nolint:gosec // reruns this test binary as a lock contender
	child.Env = append(os.Environ(), key+"="+store.Dir())
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("cross-process lock: %v\n%s", err, out)
	}
}
