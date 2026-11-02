package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
)

// executionGuardFixture is one execution host installation whose digest a
// test varies only through the host's start-guard configuration: the root,
// rules file and host config stay identical across every digest it takes.
type executionGuardFixture struct {
	root      string
	rulesPath string
}

func executionGuardFixtureOf(t *testing.T) *executionGuardFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rules := filepath.Join(root, "host-rules.md")
	if err := os.WriteFile(rules, []byte("fixture host rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &executionGuardFixture{root: root, rulesPath: rules}
}

// digest builds the host runtime of an app whose start-guard configuration
// tweak applies, and returns the policy digest `hosts bundle` prints and the
// coordinator pins.
func (f *executionGuardFixture) digest(t *testing.T, tweak func(*config.Config)) string {
	t.Helper()
	key, err := execution.SigningKey(filepath.Join(f.root, "coordinator"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		StateDir: f.root,
		Agents: config.Agents{Execution: &execution.HostConfig{
			Identity:      execution.HostID(sshTestSelector),
			RulesFile:     f.rulesPath,
			WorkspaceRoot: filepath.Join(f.root, "workspaces"),
			Repositories:  map[string]string{sshTestRepository: filepath.Join(f.root, "source")},
			Harnesses:     map[string]execution.HarnessConfig{claudeComm: {Executable: "/opt/claude/bin/claude", Version: "2.1.291"}},
			Coordinators: map[string]execution.Coordinator{sshTestCoordinator: {
				PublicKey: execution.PublicKey(key),
				Callback:  execution.Endpoint{SSH: "guard-coordinator.example", Executable: sshTestExecutable, Config: sshTestConfig},
			}},
			MaxBusy: 2,
		}},
	}
	if tweak != nil {
		tweak(cfg)
	}
	a := &app{cfg: cfg, loadedCfg: filepath.Join(f.root, "config.yaml")}
	s, err := a.hostRuntime()
	if err != nil {
		t.Fatal(err)
	}
	return s.Policy().Digest()
}

// TestExecutionHostPolicyDigestCoversTheStartGuards is the regression of
// https://github.com/giantswarm/beekeeper/issues/1945: the pinned policy
// digest must change when a host setting that gates what a remote worker may
// start under or how much it may consume changes, so a weakened guard shows
// up as a digest mismatch in `agents hosts check` instead of passing.
func TestExecutionHostPolicyDigestCoversTheStartGuards(t *testing.T) {
	f := executionGuardFixtureOf(t)
	base := f.digest(t, nil)
	if again := f.digest(t, nil); again != base {
		t.Fatal("the same configuration produced different digests")
	}
	for _, tc := range []struct {
		name  string
		tweak func(*config.Config)
	}{
		{"capacity.availMinMiB", func(c *config.Config) { c.Capacity.AvailMinMiB = 20480 }},
		{"capacity.swapGrowthMaxMiB", func(c *config.Config) { c.Capacity.SwapGrowthMaxMiB = 256 }},
		{"capacity.swapPSIMax", func(c *config.Config) { c.Capacity.SwapPSIMax = 5 }},
		{"watch.swapWindow", func(c *config.Config) { c.Watch.SwapWindow = config.Duration{Duration: 15 * time.Minute} }},
		{"watch.loadMax", func(c *config.Config) { c.Watch.LoadMax = 8 }},
		{"watch.loadPerCoreMax", func(c *config.Config) { c.Watch.LoadPerCoreMax = 1.5 }},
		{"watch.diskMinMiB", func(c *config.Config) { c.Watch.DiskMinMiB = 10240 }},
		{"watch.diskFillWithin", func(c *config.Config) { c.Watch.DiskFillWithin = config.Duration{Duration: 2 * time.Minute} }},
		{"memcap.slots", func(c *config.Config) { c.Memcap.Slots = 4 }},
		{"memcap.max", func(c *config.Config) { c.Memcap.Max = "12G" }},
		{"memcap.cpuQuota", func(c *config.Config) { c.Memcap.CPUQuota = "900%" }},
		{"memcap.cpuWeight", func(c *config.Config) { c.Memcap.CPUWeight = 60 }},
		{"maxKindClusters", func(c *config.Config) { c.MaxKindClusters = 2 }},
		// Positive control from the issue: execution.maxBusy is a HostConfig
		// field and was already covered before the fix.
		{"execution.maxBusy", func(c *config.Config) { c.Agents.Execution.MaxBusy = 3 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := f.digest(t, tc.tweak); got == base {
				t.Errorf("changing %s left the policy digest %s unchanged", tc.name, base)
			}
		})
	}
}

// TestExecutionHostPolicyDigestStaysHostLocalForWatchCadence proves the other
// half of the contract: fields that shape only this machine's own watch and
// desk, not what a pinned remote worker runs or consumes, do not invalidate
// the pinned policy.
func TestExecutionHostPolicyDigestStaysHostLocalForWatchCadence(t *testing.T) {
	f := executionGuardFixtureOf(t)
	base := f.digest(t, nil)
	for _, tc := range []struct {
		name  string
		tweak func(*config.Config)
	}{
		{"capacity.ceiling", func(c *config.Config) { c.Capacity.Ceiling = 10 }},
		{"watch.interval", func(c *config.Config) { c.Watch.Interval = config.Duration{Duration: time.Minute} }},
		{"watch.availMinMiB", func(c *config.Config) { c.Watch.AvailMinMiB = 8192 }},
		{"watch.diskCriticalMiB", func(c *config.Config) { c.Watch.DiskCriticalMiB = 1024 }},
		{"memcap.slotDir", func(c *config.Config) { c.Memcap.SlotDir = "/srv/beekeeper/slots" }},
		{"memcap.holdBound", func(c *config.Config) { c.Memcap.HoldBound = config.Duration{Duration: 30 * time.Minute} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := f.digest(t, tc.tweak); got != base {
				t.Errorf("changing host-local %s moved the policy digest: host-local settings must not invalidate the pinned policy", tc.name)
			}
		})
	}
}
