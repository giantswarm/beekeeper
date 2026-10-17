package cmd

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/platform"
)

// startedUnits records the units it is asked to start, and starts none.
type startedUnits struct {
	platform.Launcher
	started []platform.Unit
}

func (l *startedUnits) Available() bool { return true }

func (l *startedUnits) Start(u platform.Unit) error {
	l.started = append(l.started, u)
	return nil
}

// A unit a development build starts runs that build first on its PATH and
// says so: the build it runs never takes itself for the installed beekeeper,
// so it never syncs the user-wide plugins.
func TestADevBuildsUnitIsNoInstalledBeekeeper(t *testing.T) {
	l := &startedUnits{}
	useLauncher(t, l)
	if err := startUnit(platform.Unit{Name: "u"}, ""); err != nil {
		t.Fatal(err)
	}
	if len(l.started) != 1 || !slices.Contains(l.started[0].Env, devBuildEnv+"=1") {
		t.Fatalf("units %+v", l.started)
	}
	t.Setenv(devBuildEnv, "1")
	if runsInstalled() {
		t.Error("a dev build's unit runs the installed beekeeper")
	}
}

// Only the default configuration syncs the user-wide plugins.
func TestAScratchConfigurationLeavesThePluginsAlone(t *testing.T) {
	if !syncsUnder("") || syncsUnder("/tmp/scratch/config.yaml") {
		t.Error("syncsUnder")
	}
}

// supervisor start under a scratch configuration tells nobody: its roster
// can name live sessions, so it starts no broadcast unit and names the
// command instead.
func TestAScratchConfigurationStartsNoBroadcast(t *testing.T) {
	l := &startedUnits{}
	useLauncher(t, l)
	var out bytes.Buffer
	a := quietApp(t, &out)
	a.cfgPath = "scratch.yaml"
	if got := a.announce("Supervisor run 1"); !strings.HasPrefix(got, "a scratch configuration starts no unit: run `beekeeper --as \"Supervisor run 1\" agents broadcast") {
		t.Errorf("announce: %q", got)
	}
	if len(l.started) != 0 {
		t.Errorf("started %+v", l.started)
	}
}
