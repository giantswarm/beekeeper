//go:build unix

package cmd

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/update"
)

// Recovery must not invoke the workflow fixture (and its loop/GitHub searches).
func TestRecoveryProbeForkBudget(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	binary := filepath.Join(dir, "candidate")
	t.Setenv("RECOVERY_FORK_LOG", log)
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$RECOVERY_FORK_LOG\"\nprintf 'beekeeper v1.0.0\\n'\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil { //nolint:gosec // executable fixture
		t.Fatal(err)
	}
	if err := recoveryProbe(t.Context(), binary, binary+".control", dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(log) //nolint:gosec // the test's own temporary file
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(raw)); got != "version" {
		t.Fatalf("recovery launched %q; only one version process is allowed", got)
	}
}

func TestCanaryProvesWorkflowBeforeInstallation(t *testing.T) {
	old := runCanaryCheck
	t.Cleanup(func() { runCanaryCheck = old })
	workflow := 0
	runCanaryCheck = func(_ context.Context, _ string, args ...string) (string, error) {
		if len(args) == 2 && args[0] == "self-update" && args[1] == "--installation-probe" {
			workflow++
		}
		return "ok", nil
	}
	a := &app{cfgPath: filepath.Join(t.TempDir(), "config.yaml")}
	if err := a.canaryRelease(t.Context(), io.Discard, "candidate", "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if workflow != 1 {
		t.Fatalf("pre-install workflow probes = %d, want 1 without live config", workflow)
	}
	if err := probationHealth(t.Context(), a.cfgPath, "candidate"); err != nil {
		t.Fatal(err)
	}
	if workflow != 1 {
		t.Fatal("probation repeated the workflow fixture")
	}
}

func TestRecoveryControllerDoesNotFollowAnotherInstallation(t *testing.T) {
	c, _ := recoveryRig(t)
	probes := 0
	recoveryProbe = func(context.Context, string, string, string) error { probes++; return nil }
	c.mustTick(t)
	if err := update.Observe(t.Context(), c.target, func(j *update.Journal) error {
		j.InstalledAt = j.InstalledAt.Add(time.Hour)
		j.Observed = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !c.mustTick(t) || probes != 1 {
		t.Fatalf("old controller followed the replacement: %d probes", probes)
	}
}

func TestRecoveryRolledBackCandidateHasNoFurtherProbes(t *testing.T) {
	c, now := recoveryRig(t)
	probes := 0
	recoveryProbe = func(context.Context, string, string, string) error { probes++; return nil }
	c.failMerges(t, 3, *now)
	if !c.mustTick(t) {
		t.Fatal("rollback did not settle")
	}
	before := probes
	for range 60 {
		c.mustTick(t)
	}
	if probes != before {
		t.Fatalf("rolled-back candidate launched %d probes", probes-before)
	}
}

// A rollback no controller observed (Settled already calls it done) is still
// finished: the person's line is filed and the writer fence released, with
// no probe of the restored candidate.
func TestRecoveryFinishesAnUnobservedRollback(t *testing.T) {
	c, _ := recoveryRig(t)
	probes := 0
	recoveryProbe = func(context.Context, string, string, string) error { probes++; return nil }
	if err := update.Rollback(t.Context(), c.target, "manual", func(_, _ string, restore func() error) error { return restore() }); err != nil {
		t.Fatal(err)
	}
	if j := journal(t, c); !j.RolledBack() || j.Observed != nil {
		t.Fatalf("want an unobserved rollback: %+v", j)
	}
	if !c.mustTick(t) {
		t.Fatal("the finished rollback did not settle")
	}
	if lines := c.filedReports(t); len(lines) != 1 || !strings.Contains(lines[0], "RELEASE ROLLED BACK") {
		t.Fatalf("the person's line: %q", lines)
	}
	if j := journal(t, c); j.Observed == nil || !j.Observed.Notified || !j.Observed.Restarted || probes != 0 {
		t.Fatalf("finish skipped or probed: %+v, %d probes", j.Observed, probes)
	}
}
