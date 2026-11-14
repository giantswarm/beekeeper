package cmd

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/upgrade"
)

// readUpgrades is the running upgrades of alerts.installations: an
// installation's shared reading (upgrade.ReadingsFile) while it is younger
// than upgrades.every, a fresh one of the others.
func (a *app) readUpgrades(ctx context.Context, st *state.State, now time.Time) []upgrade.Status {
	return a.upgradeStatuses(ctx, st, now, upgrade.Readings{}, func(string) time.Duration { return a.cfg.Upgrades.Every.Duration })
}

// upgradeStatuses is the upgrade status of every installation of
// alerts.installations, in their order. rs, merged with the shared readings,
// answers for an installation whose reading began less than fresh(name)
// ago; the others are read, each with two attempts of at least two minutes, in parallel, and
// kept in rs and the shared readings for the next watch, snapshot or ui.
func (a *app) upgradeStatuses(ctx context.Context, st *state.State, now time.Time, rs upgrade.Readings, fresh func(installation string) time.Duration) []upgrade.Status {
	al := a.cfg.Alerts
	var shared upgrade.Readings
	if found, err := a.store.ReadFile(upgrade.ReadingsFile, &shared); found && err == nil {
		rs.Merge(shared)
	}
	var due []config.Installation
	for _, in := range al.Installations {
		if !rs.Fresh(in.Name, now, fresh(in.Name)) {
			due = append(due, in)
		}
	}
	if len(due) > 0 {
		contexts := (&alerts.Reader{Kubectl: al.Kubectl, Kubeconfig: a.alertKubeconfig()}).Contexts(ctx)
		targets := make([]upgrade.Target, 0, len(due))
		for _, in := range due {
			targets = append(targets, upgrade.Target{Name: in.Name, Context: alerts.ResolveContext(in.Name, in.Context, a.cfg.Kube.Context, contexts)})
		}
		r := upgrade.Reader{Kubectl: al.Kubectl, Kubeconfig: a.alertKubeconfig(), Timeout: max(2*time.Minute, al.Timeout.Duration)}
		read := r.Read(ctx, targets, now, upgrade.HeldClusters(st, now))
		if ctx.Err() == nil {
			rs.Keep(read, now)
			_ = a.publishUpgrades(ctx, rs)
		}
	}
	out := make([]upgrade.Status, 0, len(al.Installations))
	for _, in := range al.Installations {
		r, ok := rs[in.Name]
		if !ok {
			r.Status = upgrade.Status{Installation: in.Name, Err: "not read"}
		}
		out = append(out, upgrade.WithHoldVersions(r.Status, st, now))
	}
	return out
}

// publishUpgrades merges against the latest cache while holding a separate
// process-shared lock. It also returns that merged view through rs.
// This cache lock is independent of the upgrade cycle and state locks.
func (a *app) publishUpgrades(ctx context.Context, rs upgrade.Readings) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	l := flock.New(filepath.Join(a.store.Dir(), upgrade.ReadingsFile+".lock"))
	ok, err := l.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("upgrade readings lock unavailable")
	}
	defer func() { _ = l.Unlock() }()
	var latest upgrade.Readings
	if _, err := a.store.ReadFile(upgrade.ReadingsFile, &latest); err != nil {
		return err
	}
	rs.Merge(latest)
	return a.store.WriteFile(upgrade.ReadingsFile, rs)
}
