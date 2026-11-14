package cmd

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/watch"
)

// failedLauncher has failed units and records the ones it reset.
type failedLauncher struct {
	platform.Launcher
	failed, patterns, reset []string
	running, stopped        []string
}

func (l *failedLauncher) Failed(_ context.Context, patterns ...string) []string {
	l.patterns = patterns
	return l.failed
}

func (l *failedLauncher) ResetFailed(_ context.Context, units ...string) error {
	l.reset = append(l.reset, units...)
	return nil
}

func (l *failedLauncher) Running(_ context.Context, _ bool, patterns ...string) []string {
	l.patterns = patterns
	return l.running
}

func (l *failedLauncher) Stop(_ context.Context, unit string) error {
	l.stopped = append(l.stopped, unit)
	return nil
}

// The doctor clears the failed state of beekeeper's own units and says
// which; a dry run only says them.
func TestDoctorResetsOwnFailedUnits(t *testing.T) {
	orig := plat
	t.Cleanup(func() { plat = orig })
	units := []string{"beekeeper-merge-a.service", "beekeeper-wake-b.service"}
	for name, tc := range map[string]struct {
		dryRun bool
		reset  []string
		line   string
	}{
		"a pass":    {false, units, "reset the failed units beekeeper-merge-a.service, beekeeper-wake-b.service"},
		"a dry run": {true, nil, "would reset the failed units beekeeper-merge-a.service, beekeeper-wake-b.service"},
	} {
		l := &failedLauncher{failed: units}
		plat.Launcher = l
		line := resetOwnUnits(context.Background(), tc.dryRun)
		if !strings.HasPrefix(line, tc.line) || !slices.Equal(l.reset, tc.reset) || !slices.Equal(l.patterns, []string{ownUnits}) {
			t.Errorf("%s: %q, reset %v of %v", name, line, l.reset, l.patterns)
		}
	}
	plat.Launcher = &failedLauncher{}
	if line := resetOwnUnits(context.Background(), false); line != "" {
		t.Errorf("no failed unit: %q", line)
	}
}

// The doctor reports and stops runner units whose session parked, reported
// done or left the roster, while keeping units of live unparked sessions and
// busy turns running.
func TestDoctorStopsRunnerUnitsWithoutLiveSession(t *testing.T) {
	orig := plat
	t.Cleanup(func() { plat = orig })

	const (
		workingID   = "11111111-1111-4111-8111-111111111111"
		idleID      = "22222222-2222-4222-8222-222222222222"
		parkedID    = "33333333-3333-4333-8333-333333333333"
		doneID      = "44444444-4444-4444-8444-444444444444"
		offRosterID = "55555555-5555-4555-8555-555555555555"
		busyOffID   = "66666666-6666-4666-8666-666666666666"
	)
	st := &state.State{
		Agents: []state.Agent{
			{Party: state.Party{Session: workingID, HostSession: codex.HostPrefix + workingID, Name: "working-codex"}, Task: "working-unit-task"},
			{Party: state.Party{HostSession: agy.HostPrefix + idleID, Name: "idle-agy"}},
			{Party: state.Party{Session: parkedID, HostSession: codex.HostPrefix + parkedID, Name: "parked-codex"}, Task: "parked-unit-task", Park: &state.Park{On: "giantswarm/beekeeper#3675"}},
			{Party: state.Party{HostSession: agy.HostPrefix + doneID, Name: "done-agy"}, Done: true},
		},
		Starts: []state.Start{
			{Party: state.Party{Session: busyOffID, HostSession: codex.HostPrefix + busyOffID}, Harness: codex.Harness},
		},
	}
	running := []string{
		codexUnit(workingID),
		agyUnit(idleID),
		codexUnit(parkedID),
		agyUnit(doneID),
		codexUnit(offRosterID),
		codexUnit(busyOffID),
	}
	wantStale := []string{
		codexUnit(parkedID),
		codexUnit(offRosterID),
		agyUnit(doneID),
	}
	slices.Sort(wantStale)

	busy := func(p state.Party) bool {
		return p.Session == busyOffID || p.HostSession == codex.HostPrefix+busyOffID
	}

	for name, tc := range map[string]struct {
		dryRun  bool
		stopped []string
		prefix  string
	}{
		"a pass":    {false, wantStale, "stopped the runner units without a live session " + strings.Join(wantStale, ", ")},
		"a dry run": {true, nil, "would stop the runner units without a live session " + strings.Join(wantStale, ", ")},
	} {
		l := &failedLauncher{running: running}
		plat.Launcher = l
		var a *app
		line := a.doctorRunnerUnits(context.Background(), st, busy, watch.WatchParty, tc.dryRun)
		if line != tc.prefix || !slices.Equal(l.stopped, tc.stopped) {
			t.Errorf("%s: line = %q (want %q), stopped = %v (want %v)", name, line, tc.prefix, l.stopped, tc.stopped)
		}
	}
}
