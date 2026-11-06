package cmd

import (
	"context"

	"github.com/giantswarm/beekeeper/internal/execution"
)

// currentBundle reads the coordinator's approved rules and skills anew; nil
// when agents.policyBundle names none.
func (a *app) currentBundle() (*execution.CoordinatorBundle, error) {
	c := a.cfg.Agents.PolicyBundle
	if c == nil {
		return nil, nil
	}
	secrets := append(append([]string(nil), a.cfg.Secret.Files...), a.cfg.Secret.TokenFile)
	b, err := execution.BuildCoordinatorBundle(c.Root, c.Sources, c.Settings, c.Capabilities, secrets)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// syncHostBundle puts the current bundle on h and answers the digest the start
// binds, empty when none is configured. A failure refuses the start: a worker
// never runs on rules older than the coordinator's, here or elsewhere.
func (a *app) syncHostBundle(ctx context.Context, h execution.Host) (string, error) {
	b, err := a.currentBundle()
	if err != nil {
		return "", refused("the coordinator policy bundle cannot be read, so no remote worker starts: %v", err)
	}
	if b == nil {
		return "", nil
	}
	c, ok := a.executionClient(h).(execution.BundleSyncer)
	if !ok {
		return "", refused("execution host %s has no policy bundle route, so no remote worker starts", h.Identity)
	}
	digest, err := execution.SyncCurrentBundle(ctx, c, *b)
	if err != nil {
		return "", refused("execution host %s did not take the coordinator policy bundle, so no remote worker starts: %v", h.Identity, err)
	}
	return digest, nil
}

// hostBundleReport is hosts check's comparison of the host with the current
// bundle, nil when none is configured. An unreadable side is a Problem.
func (a *app) hostBundleReport(ctx context.Context, h execution.Host) *execution.BundleReport {
	b, err := a.currentBundle()
	if err != nil {
		return &execution.BundleReport{Problem: "the coordinator policy bundle cannot be read: " + err.Error()}
	}
	if b == nil {
		return nil
	}
	c, ok := a.executionClient(h).(execution.BundleSyncer)
	if !ok {
		return &execution.BundleReport{Expected: b.Digest(), Problem: "execution host has no policy bundle route"}
	}
	st, err := c.BundleStatus(ctx, b.Digest())
	if err != nil {
		return &execution.BundleReport{Expected: b.Digest(), Problem: "host bundle status unavailable: " + err.Error()}
	}
	report := b.Compare(st)
	return &report
}
