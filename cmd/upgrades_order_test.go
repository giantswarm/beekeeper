package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/upgrade"
)

// Pause at Update, after the first watch captured its statuses, and let a
// second watch try a newer observation before the first one reconciles.
type upgradeInterleavingStore struct {
	state.Store
	beforeUpdate func()
	updates      int
}

func (s *upgradeInterleavingStore) Update(fn func(*state.State) ([]state.Event, error)) error {
	s.updates++
	if f := s.beforeUpdate; f != nil {
		s.beforeUpdate = nil
		f()
	}
	return s.Store.Update(fn)
}

func TestUpgradeWatchesCannotReconcileOutOfOrder(t *testing.T) {
	for _, newerRunning := range []bool{false, true} {
		name := "newer quiet"
		if newerRunning {
			name = "newer running"
		}
		t.Run(name, func(t *testing.T) {
			a, _ := stubApp(t)
			a.cfg.Alerts.Installations = []config.Installation{{Name: "prod"}}
			store := &upgradeInterleavingStore{Store: a.store}
			a.store = store
			at := time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)
			reading := func(running bool, when time.Time) upgrade.Readings {
				s := upgrade.Status{Installation: "prod"}
				if running {
					s.Upgrades = []upgrade.Upgrade{{Cluster: "mc", To: "35.2.0"}}
				}
				return upgrade.Readings{"prod": {At: when, Status: s}}
			}
			older, newer := a.newWatcher(false, false), a.newWatcher(false, false)
			older.clock, newer.clock = &stepClock{now: at}, &stepClock{now: at.Add(time.Minute)}
			older.upgrades = reading(!newerRunning, at)
			newer.upgrades = reading(newerRunning, at.Add(time.Minute))
			store.beforeUpdate = func() { newer.upgradeCycle(context.Background()) }
			older.upgradeCycle(context.Background())
			if store.updates != 1 {
				t.Fatalf("a second watch reconciled while an older observation was pending: %d updates", store.updates)
			}
			// The next turn takes the released lock and applies the newer
			// status, in either direction, without a real reader or sleep.
			newer.upgradeCycle(context.Background())
			st, err := store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if _, held := upgrade.Held(st, "prod", at.Add(time.Minute)); held != newerRunning {
				t.Fatalf("held %v after the newer observation, want %v", held, newerRunning)
			}
		})
	}
}

func TestUpgradePublicationKeepsNewerAndDisjointReadings(t *testing.T) {
	a, _ := stubApp(t)
	at := time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)
	older := upgrade.Readings{"prod": {At: at, Status: upgrade.Status{Installation: "prod"}}}
	newer := upgrade.Readings{
		"prod": {At: at.Add(time.Minute), Status: upgrade.Status{Installation: "prod", Upgrades: []upgrade.Upgrade{{Cluster: "mc", To: "35.2.0"}}}},
		"test": {At: at, Status: upgrade.Status{Installation: "test"}},
	}
	// Both maps were captured before publication. The older reader finishes
	// last, after the newer reader has added another installation too.
	a.saveUpgradeReadings(context.Background(), newer)
	a.saveUpgradeReadings(context.Background(), older)
	var saved upgrade.Readings
	if found, err := a.store.ReadFile(upgrade.ReadingsFile, &saved); err != nil || !found {
		t.Fatalf("saved readings: found %v, error %v", found, err)
	}
	for label, rs := range map[string]upgrade.Readings{"saved": saved, "caller": older} {
		if len(rs) != 2 || !rs["prod"].At.Equal(newer["prod"].At) || !rs.Running("prod") {
			t.Errorf("%s lost a newer or disjoint reading: %+v", label, rs)
		}
	}
	// Cancellation before publication leaves the successful reading intact.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.saveUpgradeReadings(ctx, upgrade.Readings{"canceled": {At: at.Add(time.Hour)}})
	if _, err := a.store.ReadFile(upgrade.ReadingsFile, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 2 {
		t.Fatalf("canceled publication changed the cache: %+v", saved)
	}
}
