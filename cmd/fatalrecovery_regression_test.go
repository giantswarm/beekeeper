package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/fatal"
)

func TestFatalHarnessHealthPrecedesSlowOptionalProbes(t *testing.T) {
	a, _ := stubApp(t)
	dir := a.store.Dir()
	source := filepath.Join(t.TempDir(), "main.yaml")
	a.cfgPath = source
	raw := fmt.Sprintf("stateDir: %q\nfatal:\n  repositories: []\n  checks:\n    - name: failed\n      probe: [broken-probe]\n    - name: slow\n      probe: [slow-probe]\n", dir)
	if err := os.WriteFile(source, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	oldTimeout, oldRun, oldStart := fatalObservationTimeout, fatalRun, beekeeperRun
	fatalObservationTimeout = 20 * time.Millisecond
	defer func() { fatalObservationTimeout, fatalRun, beekeeperRun = oldTimeout, oldRun, oldStart }()
	verified := false
	fatalRun = func(ctx context.Context, _ string, args []string) (string, string, int, error) {
		if slices.Contains(args, "is-enabled") {
			return fatalDisabledState, "", 1, nil
		}
		switch args[0] {
		case "broken-probe":
			return "", "", 1, nil
		case "slow-probe":
			<-ctx.Done()
			return "", "", -1, ctx.Err()
		}
		if ctx.Err() != nil {
			return "", "", -1, ctx.Err()
		}
		if slices.Contains(args, "--version") {
			verified = true
		}
		return "", "", 0, nil
	}
	starts := 0
	beekeeperRun = func(context.Context, ...string) (string, int, error) {
		starts++
		if !verified {
			t.Error("repair launched without completing harness health probe")
		}
		return "", 0, nil
	}
	if err := a.fatalScan(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	if starts != 1 {
		t.Fatalf("verified harness did not receive recovery: %d starts", starts)
	}
}

func TestRemoteReviewMissingEnvConfigurationRetainsRepairTarget(t *testing.T) {
	a, _ := stubApp(t)
	dir := a.store.Dir()
	source := filepath.Join(t.TempDir(), "main.yaml")
	raw := fmt.Sprintf("stateDir: %q\nfatal: {repositories: []}\n", dir)
	if err := os.WriteFile(source, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveRecovery(dir, source); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(source, source+".moved"); err != nil {
		t.Fatal(err)
	}
	a.cfgPath = ""
	t.Setenv("BEEKEEPER_CONFIG", source)
	fakeFatalRun(t, func(args []string) (string, int) {
		if slices.Contains(args, "is-enabled") {
			return fatalDisabledState, 1
		}
		return "", 0
	})
	old := beekeeperRun
	defer func() { beekeeperRun = old }()
	starts := 0
	beekeeperRun = func(_ context.Context, args ...string) (string, int, error) {
		starts++
		brief, err := os.ReadFile(filepath.Clean(filepath.Join(dir, "fatal-repair-brief.txt")))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(brief), source) {
			t.Error("repair brief lost the missing main configuration path")
		}
		return "", 0, nil
	}
	if err := a.fatalScan(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	if starts != 1 {
		t.Fatalf("expected one config repair, got %d", starts)
	}
	// A repair inherits the snapshot path. Scanning it must still observe
	// the broken original, rather than resolve the incident from the fallback.
	a.cfgPath = config.RecoveryPath(dir)
	if err := a.fatalScan(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	q, err := fatal.Read(dir)
	if err != nil || q.Barrier("ordinary") == nil || starts != 1 {
		t.Fatal("snapshot scan resolved the original outage or duplicated its owner", err)
	}
	if err := os.WriteFile(source, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.fatalScan(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	q, err = fatal.Read(dir)
	if err != nil || q.Barrier("ordinary") != nil {
		t.Fatal("original configuration recovery was not verified", err)
	}
}

func TestFatalUnknownHarnessDefersWithoutRepairAttempt(t *testing.T) {
	a, _ := stubApp(t)
	dir := a.store.Dir()
	source := filepath.Join(t.TempDir(), "config.yaml")
	a.cfgPath = source
	if err := os.WriteFile(source, []byte(fmt.Sprintf("stateDir: %q\nfatal:\n  repositories: []\n  checks: [{name: failed, probe: [unavailable-probe]}]\n", dir)), 0600); err != nil {
		t.Fatal(err)
	}
	q := fatal.Queue{}
	q.Observe(fatal.Observation{ID: "check:failed", Failed: true}, time.Now().Add(-time.Hour))
	if err := q.Save(dir); err != nil {
		t.Fatal(err)
	}
	oldTimeout, oldRun, oldStart := fatalObservationTimeout, fatalRun, beekeeperRun
	fatalObservationTimeout = 20 * time.Millisecond
	defer func() { fatalObservationTimeout, fatalRun, beekeeperRun = oldTimeout, oldRun, oldStart }()
	fatalRun = func(ctx context.Context, _ string, args []string) (string, string, int, error) {
		if slices.Contains(args, "is-enabled") {
			return fatalDisabledState, "", 1, nil
		}
		<-ctx.Done()
		return "", "", -1, ctx.Err()
	}
	starts := 0
	beekeeperRun = func(context.Context, ...string) (string, int, error) { starts++; return "", 1, nil }
	if err := a.fatalScan(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	q, err := fatal.Read(dir)
	if err != nil || starts != 0 || q.Incidents[0].Attempts != 0 {
		t.Fatalf("unknown harness consumed a repair attempt: starts=%d queue=%+v error=%v", starts, q, err)
	}
}
