package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/fatal"
	"github.com/giantswarm/beekeeper/internal/loop"
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
	oldTimeout, oldRun, oldStart := fatalObservationTimeout, fatalRun, loop.BeekeeperRun
	fatalObservationTimeout = 20 * time.Millisecond
	defer func() { fatalObservationTimeout, fatalRun, loop.BeekeeperRun = oldTimeout, oldRun, oldStart }()
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
	loop.BeekeeperRun = func(context.Context, ...string) (string, int, error) {
		starts++
		if !verified {
			t.Error("repair launched without completing harness health probe")
		}
		return "", 0, nil
	}
	// Advance the observation deadline only when the slow probe blocks. Real
	// configuration I/O and CI scheduling must not consume this test budget.
	synctest.Test(t, func(t *testing.T) {
		if err := a.fatalScan(t.Context(), dir); err != nil {
			t.Fatal(err)
		}
	})
	if starts != 1 {
		t.Fatalf("verified harness did not receive recovery: %d starts", starts)
	}
}

func TestRemoteReviewMissingEnvConfigurationRetainsRepairTarget(t *testing.T) {
	for _, defaultState := range []bool{false, true} {
		t.Run(fmt.Sprintf("default-state-%t", defaultState), func(t *testing.T) {
			testMissingEnvConfiguration(t, defaultState)
		})
	}
}

func testMissingEnvConfiguration(t *testing.T, defaultState bool) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("BEEKEEPER_CONFIG", "")
	a, _ := stubApp(t)
	dir := a.store.Dir()
	if defaultState {
		cfg, err := config.Load(filepath.Join(t.TempDir(), "missing.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		dir = cfg.StateDir
	}
	source := filepath.Join(t.TempDir(), "main.yaml")
	raw := fmt.Sprintf("stateDir: %q\nwatch: {interval: 13s}\nfatal: {repositories: []}\n", dir)
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
	old := loop.BeekeeperRun
	defer func() { loop.BeekeeperRun = old }()
	starts := 0
	loop.BeekeeperRun = func(_ context.Context, args ...string) (string, int, error) {
		starts++
		brief, err := readStateFixture(dir, filepath.Join(dir, fatalRepairBriefFile))
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
	a.cfgPath = ""
	t.Setenv("BEEKEEPER_CONFIG", config.RecoveryPath(dir))
	if err := a.fatalScan(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	q, err := fatal.Read(dir)
	if err != nil || q.Barrier("ordinary") == nil || starts != 1 {
		t.Fatal("snapshot scan resolved the original outage or duplicated its owner", err)
	}
	trusted, err := config.LoadRecovery(dir)
	if err != nil || trusted.Watch.Interval.String() != "13s" {
		t.Fatal("missing source overwrote trusted admission", err)
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
	testFatalConfiguredHarnessAdmission(t, true, false)
}

func TestFatalFailedConfiguredHarnessesDeferWithoutRepairAttempt(t *testing.T) {
	testFatalConfiguredHarnessAdmission(t, false, false)
}

func TestFatalHealthyAlternateConfiguredHarnessReceivesRepair(t *testing.T) {
	testFatalConfiguredHarnessAdmission(t, false, true)
}

func testFatalConfiguredHarnessAdmission(t *testing.T, unknown bool, healthyAlternate bool) {
	t.Helper()
	a, _ := stubApp(t)
	dir := a.store.Dir()
	source := filepath.Join(t.TempDir(), "config.yaml")
	a.cfgPath = source
	if err := os.WriteFile(source, []byte(fmt.Sprintf("stateDir: %q\ncapacity:\n  providers: {claude: {}, agy: {}}\n  choices: [{harness: claude, model: test}, {harness: agy, model: test}]\nfatal:\n  repositories: []\n  checks: [{name: failed, probe: [unavailable-probe]}]\n", dir)), 0600); err != nil {
		t.Fatal(err)
	}
	q := fatal.Queue{}
	q.Observe(fatal.Observation{ID: "check:failed", Failed: true}, time.Now().Add(-time.Hour))
	if err := q.Save(dir); err != nil {
		t.Fatal(err)
	}
	oldTimeout, oldRun, oldStart := fatalObservationTimeout, fatalRun, loop.BeekeeperRun
	fatalObservationTimeout = 20 * time.Millisecond
	defer func() { fatalObservationTimeout, fatalRun, loop.BeekeeperRun = oldTimeout, oldRun, oldStart }()
	fatalRun = func(ctx context.Context, _ string, args []string) (string, string, int, error) {
		if slices.Contains(args, "is-enabled") {
			return fatalDisabledState, "", 1, nil
		}
		if healthyAlternate && slices.Contains(args, "agy") {
			return "", "", 0, nil
		}
		if !unknown {
			return "", "", 1, nil
		}
		<-ctx.Done()
		return "", "", -1, ctx.Err()
	}
	starts := 0
	loop.BeekeeperRun = func(context.Context, ...string) (string, int, error) { starts++; return "", 1, nil }
	if err := a.fatalScan(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	q, err := fatal.Read(dir)
	wantStarts := 0
	if healthyAlternate {
		wantStarts = 1
	}
	if err != nil || starts != wantStarts || (!healthyAlternate && (q.Incidents[0].Attempts != 0 || q.Incidents[0].Owner != "")) {
		t.Fatalf("unknown harness consumed a repair attempt: starts=%d queue=%+v error=%v", starts, q, err)
	}
}
