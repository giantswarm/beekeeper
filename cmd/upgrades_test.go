package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/upgrade"
)

// stepClock is a watch's clock that the test moves; its timers never fire.
type stepClock struct{ now time.Time }

func (c *stepClock) Now() time.Time { return c.now }

func (c *stepClock) Timer(time.Duration) (<-chan time.Time, func()) { return nil, func() {} }

// upgradeKubectl is a kubectl that logs the context of each call and
// answers the installation upgrading with dir/upgrading.json and
// dir/events.json, every other one with no objects.
func upgradeKubectl(t *testing.T, dir, upgrading string) (kubectl, log string) {
	t.Helper()
	kubectl, log = filepath.Join(dir, "kubectl"), filepath.Join(dir, "calls")
	script := `#!/bin/sh
echo "$2 $6" >> "` + log + `"
case "$2,$6" in
` + upgrading + `,events) exec cat "` + dir + `/events.json" ;;
` + upgrading + `,*.*) exec cat "` + dir + `/upgrading.json" ;;
esac
echo '{"items":[]}'
`
	if err := os.WriteFile(kubectl, []byte(script), 0o700); err != nil { //nolint:gosec // a test's script
		t.Fatal(err)
	}
	return kubectl, log
}

// phase makes the upgrading installation answer with the objects of the
// recorded upgrade's phase (internal/upgrade/testdata/prod).
func phase(t *testing.T, dir, name string) {
	t.Helper()
	testdata := filepath.Join("..", "internal", "upgrade", "testdata", "prod", name)
	list := struct {
		Items []json.RawMessage `json:"items"`
	}{}
	for _, k := range upgrade.Kinds {
		kind, _, _ := strings.Cut(k, ".")
		raw, err := os.ReadFile(filepath.Join(testdata, kind+".json")) //nolint:gosec // testdata
		if err != nil {
			t.Fatal(err)
		}
		var l struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(raw, &l); err != nil {
			t.Fatal(err)
		}
		list.Items = append(list.Items, l.Items...)
	}
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "upgrading.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if raw, err = os.ReadFile(filepath.Join(testdata, "events.json")); err != nil { //nolint:gosec // testdata
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.json"), raw, 0o600); err != nil { //nolint:gosec // a test directory
		t.Fatal(err)
	}
}

// takeCalls returns the kubectl calls since the last take, by context.
func takeCalls(t *testing.T, log string) map[string]int {
	t.Helper()
	raw, err := os.ReadFile(log) //nolint:gosec // a test's log
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	_ = os.Remove(log)
	n := map[string]int{}
	for l := range strings.Lines(string(raw)) {
		ctx, _, _ := strings.Cut(l, " ")
		n[ctx]++
	}
	return n
}

// Five quiet installations are read once each per upgrades.every; the one
// whose upgrade begins is read every watch.interval until UPGRADE ENDED, and
// a second watch, the snapshot and the ui use the shared readings.
func TestWatchReadsUpgradesOncePerEvery(t *testing.T) {
	a, out := stubApp(t)
	dir := t.TempDir()
	const upgrading = "prod"
	kubectl, log := upgradeKubectl(t, dir, upgrading)
	a.cfg.Alerts.Kubectl = kubectl
	a.cfg.Alerts.Installations = nil
	for _, name := range []string{upgrading, "b", "c", "d", "e"} {
		a.cfg.Alerts.Installations = append(a.cfg.Alerts.Installations, config.Installation{Name: name, Context: name})
	}
	a.cfg.Watch.Interval.Duration = 30 * time.Second
	if a.cfg.Upgrades.Every.Duration != 5*time.Minute {
		t.Fatalf("upgrades.every defaults to %s", a.cfg.Upgrades.Every.Duration)
	}
	clk := &stepClock{now: time.Date(2026, 9, 25, 8, 30, 0, 0, time.UTC)}
	w := a.newWatcher(false, false)
	w.clock = clk
	ticks := func(n int) map[string]int {
		t.Helper()
		for range n {
			clk.now = clk.now.Add(30 * time.Second)
			w.upgradeCycle(context.Background())
		}
		return takeCalls(t, log)
	}

	// 08:30: every installation is read once; prod's upgrade begins.
	phase(t, dir, "0827-during")
	w.upgradeCycle(context.Background())
	got := takeCalls(t, log)
	for _, in := range a.cfg.Alerts.Installations {
		if want := 1 + map[bool]int{true: 1}[in.Name == upgrading]; got[in.Name] != want {
			t.Fatalf("first reading: %s read %d times, want %d (the upgrade's events once): %v", in.Name, got[in.Name], want, got)
		}
	}
	if !strings.Contains(out.String(), "UPGRADE prod/mc 35.0.1 → 35.1.1") {
		t.Fatalf("no UPGRADE line:\n%s", out.String())
	}
	// 08:30:30 to 08:34:30: the upgrading installation every interval, the
	// others not at all.
	if got = ticks(9); len(got) != 1 || got[upgrading] != 9 {
		t.Fatalf("while prod upgrades: %v", got)
	}
	// 08:35: the upgrade has ended; all are due, prod's end is said.
	phase(t, dir, "0843-after")
	if got = ticks(1); len(got) != 5 {
		t.Fatalf("at upgrades.every: %v", got)
	}
	if !strings.Contains(out.String(), "UPGRADE ENDED prod/mc") {
		t.Fatalf("no UPGRADE ENDED line:\n%s", out.String())
	}
	// 08:35:30 to 08:40: quiet, one read of each installation.
	got = ticks(10)
	total := 0
	for _, in := range a.cfg.Alerts.Installations {
		if got[in.Name] != 1 {
			t.Errorf("quiet: %s read %d times in upgrades.every, want 1: %v", in.Name, got[in.Name], got)
		}
		total += got[in.Name]
	}
	if total != len(a.cfg.Alerts.Installations) {
		t.Errorf("quiet: %d kubectl in upgrades.every for 5 installations: %v", total, got)
	}

	// A second watch, the snapshot and the ui use the fresh shared reading.
	clk.now = clk.now.Add(time.Minute)
	second := a.newWatcher(false, false)
	second.clock = clk
	second.upgradeCycle(context.Background())
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	ss := a.readUpgrades(context.Background(), st, clk.now)
	if got = takeCalls(t, log); len(got) != 0 {
		t.Errorf("a second watch or the snapshot read again: %v", got)
	}
	if len(ss) != 5 || ss[0].Installation != upgrading || ss[0].Err != "" || len(ss[0].Upgrades) != 0 {
		t.Errorf("the snapshot's statuses: %+v", ss)
	}
	// Past upgrades.every, the snapshot reads afresh.
	a.readUpgrades(context.Background(), st, clk.now.Add(5*time.Minute))
	if got = takeCalls(t, log); len(got) != 5 {
		t.Errorf("a stale reading was not read again: %v", got)
	}
}
