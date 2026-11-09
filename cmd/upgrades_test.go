package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/upgrade"
)

// stepClock is a watch's clock that the test moves; its timers never fire.
type stepClock struct{ now time.Time }

func (c *stepClock) Now() time.Time { return c.now }

func (c *stepClock) Timer(time.Duration) (<-chan time.Time, func()) { return nil, func() {} }

// upgradeKubectl is a kubectl that logs the context of each call and
// answers the installation upgrading with dir/upgrading.json and
// dir/events.json, the context broken with an error, every other one with no
// objects. Its kubeconfig's contexts are dir/contexts' lines; without the
// file, they are unknown.
func upgradeKubectl(t *testing.T, dir, upgrading string) (kubectl, log string) {
	t.Helper()
	upgradeContexts(t, dir, strings.Join([]string{upgrading, "b", "c", "d", "e"}, "\n"))
	kubectl, log = filepath.Join(dir, "kubectl"), filepath.Join(dir, "calls")
	script := `#!/bin/sh
if [ "$1" = config ]; then exec cat "` + dir + `/contexts"; fi
echo "$2 $6" >> "` + log + `"
case "$2,$6" in
` + upgrading + `,events) exec cat "` + dir + `/events.json" ;;
` + upgrading + `,*.*) exec cat "` + dir + `/upgrading.json" ;;
broken,*) echo "error: You must be logged in to the server (Unauthorized)" >&2; exit 1 ;;
esac
echo '{"items":[]}'
`
	if err := os.WriteFile(kubectl, []byte(script), 0o700); err != nil { //nolint:gosec // a test's script
		t.Fatal(err)
	}
	return kubectl, log
}

// Context discovery reads a kubeconfig directly; kubectl still supplies objects.
func upgradeContexts(t *testing.T, dir, names string) {
	t.Helper()
	var data strings.Builder
	data.WriteString("apiVersion: v1\nkind: Config\ncontexts:\n")
	for _, name := range strings.Fields(names) {
		fmt.Fprintf(&data, "- name: %q\n  context: {}\n", name)
	}
	path := filepath.Join(dir, "contexts.yaml")
	if err := os.WriteFile(path, []byte(data.String()), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
}

// phase makes the upgrading installation answer with the objects of the
// recorded upgrade's phase (internal/upgrade/testdata/prod).
func phase(t *testing.T, dir, name string) {
	t.Helper()
	testdata := filepath.Join("..", "internal", "upgrade", "testdata", snapshotInstallation, name)
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
	const upgrading = snapshotInstallation
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
	if !strings.Contains(out.String(), "UPGRADE prod/org-mc/mc 35.0.1 → 35.1.1") {
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
	if !strings.Contains(out.String(), "UPGRADE ENDED prod/org-mc/mc") {
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

// With no supervisor's watch, the standby watch reads the upgrades, holds
// the installation at UPGRADE and lifts the hold at UPGRADE ENDED; once a
// supervisor's watch runs, it reads none of its own until that watch stops.
func TestStandbyWatchHoldsUpgradesWithoutSupervisorWatch(t *testing.T) {
	a, out := stubApp(t)
	dir := t.TempDir()
	const upgrading = snapshotInstallation
	kubectl, log := upgradeKubectl(t, dir, upgrading)
	a.cfg.Alerts.Kubectl = kubectl
	a.cfg.Alerts.Installations = []config.Installation{{Name: upgrading, Context: upgrading}, {Name: "b", Context: "b"}}
	a.cfg.Watch.Interval.Duration = 30 * time.Second
	clk := &stepClock{now: time.Date(2026, 9, 25, 8, 30, 0, 0, time.UTC)}
	standby := a.newWatcher(true, false)
	standby.clock = clk
	held := func() bool {
		t.Helper()
		st, err := a.store.Read()
		if err != nil {
			t.Fatal(err)
		}
		_, ok := upgrade.Held(st, upgrading, clk.now)
		return ok
	}

	// 08:30: no supervisor's watch; the standby watch reads and holds.
	phase(t, dir, "0827-during")
	standby.upgradeCycle(context.Background())
	if got := takeCalls(t, log); got[upgrading] == 0 || got["b"] != 1 {
		t.Fatalf("the standby watch did not read: %v", got)
	}
	if !held() || !strings.Contains(out.String(), "UPGRADE prod/org-mc/mc 35.0.1 → 35.1.1") {
		t.Fatalf("no hold at UPGRADE:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "UPGRADES read by the standby watch") {
		t.Fatalf("the takeover is not said:\n%s", out.String())
	}
	// 08:30:30: the upgrade ended; the held installation is read again
	// within one interval and its hold lifted.
	phase(t, dir, "0843-after")
	clk.now = clk.now.Add(30 * time.Second)
	standby.upgradeCycle(context.Background())
	if got := takeCalls(t, log); got[upgrading] != 1 || got["b"] != 0 {
		t.Fatalf("after the upgrade: %v", got)
	}
	if held() || !strings.Contains(out.String(), "UPGRADE ENDED prod/org-mc/mc") {
		t.Fatalf("the hold outlived UPGRADE ENDED:\n%s", out.String())
	}

	// 08:35 to 08:45: a supervisor's watch runs; the standby watch reads
	// nothing, however stale the readings, and its takeover ends.
	super := a.newWatcher(false, false)
	super.clock = clk
	clk.now = clk.now.Add(4*time.Minute + 30*time.Second)
	for range 20 {
		super.upgradeCycle(context.Background())
		takeCalls(t, log)
		_ = os.Remove(filepath.Join(a.cfg.StateDir, upgrade.ReadingsFile))
		standby.upgradeCycle(context.Background())
		if got := takeCalls(t, log); len(got) != 0 {
			t.Fatalf("the standby watch read beside a supervisor's watch at %s: %v", clk.now.Format("15:04:05"), got)
		}
		clk.now = clk.now.Add(30 * time.Second)
	}
	if !strings.Contains(out.String(), "ENDED UPGRADES read by the standby watch") {
		t.Fatalf("the takeover's end is not said:\n%s", out.String())
	}

	// Five intervals after the supervisor's watch's last cycle, the standby
	// watch reads again.
	clk.now = clk.now.Add(2 * time.Minute)
	standby.upgradeCycle(context.Background())
	if got := takeCalls(t, log); len(got) != 2 {
		t.Fatalf("the standby watch did not take over again: %v", got)
	}
}

// Installations whose context is not in the kubeconfig are not read and
// named in one line per watch, never one line each; one whose context
// resolves and fails to read keeps its own line. The snapshot's upgrades
// name them in one entry.
func TestWatchNamesInstallationsWithoutContextOnce(t *testing.T) {
	a, out := stubApp(t)
	dir := t.TempDir()
	kubectl, log := upgradeKubectl(t, dir, "upgrading")
	upgradeContexts(t, dir, snapshotInstallation+"\nbroken\n")
	a.cfg.Alerts.Kubectl = kubectl
	a.cfg.Alerts.Installations = []config.Installation{
		{Name: snapshotInstallation, Context: snapshotInstallation}, {Name: "broken", Context: "broken"},
		{Name: "lost", Context: "teleport.giantswarm.io-lost"}, {Name: "far", Context: "teleport.giantswarm.io-far"},
	}
	a.cfg.Watch.Interval.Duration = 30 * time.Second
	clk := &stepClock{now: time.Date(2026, 9, 25, 8, 30, 0, 0, time.UTC)}
	w := a.newWatcher(false, false)
	w.clock = clk
	for range 3 {
		w.upgradeCycle(context.Background())
		clk.now = clk.now.Add(a.cfg.Upgrades.Every.Duration)
	}
	if got := takeCalls(t, log); got["lost"] != 0 || got["far"] != 0 || got["teleport.giantswarm.io-lost"] != 0 || got[snapshotInstallation] != 3 || got["broken"] != 6 {
		t.Errorf("kubectl calls: %v", got)
	}
	text := out.String()
	if strings.Contains(text, "lost unreadable") || strings.Contains(text, "far unreadable") {
		t.Errorf("an installation without a kube context has a line of its own:\n%s", text)
	}
	if n := strings.Count(text, "UPGRADES: 2 installations without a kube context, not read: lost, far"); n != 1 {
		t.Errorf("%d summary lines, want 1:\n%s", n, text)
	}
	if !strings.Contains(text, "UPGRADES broken unreadable: error: You must be logged in to the server (Unauthorized)") {
		t.Errorf("the unreadable installation has no line:\n%s", text)
	}

	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	words := strings.Join(upgradeWords(a.readUpgrades(context.Background(), st, clk.now), time.Time{}), " · ")
	if want := "prod none · broken unreadable: error: You must be logged in to the server (Unauthorized) (deadline 2m0s per attempt; up to 2 attempts) · 2 installations without a kube context, not read: lost, far"; words != want {
		t.Errorf("snapshot upgrades:\n%s\nwant\n%s", words, want)
	}
}

// A process whose kubeconfig lacks an installation's context shares a
// reading without a kube context; a process whose kubeconfig has the context
// reads the installation within the same upgrades.every, and one still
// without it does not call kubectl for it.
func TestSharedReadingWithoutContextIsReadWhereTheContextResolves(t *testing.T) {
	a, _ := stubApp(t)
	dir := t.TempDir()
	kubectl, log := upgradeKubectl(t, dir, "upgrading")
	contexts := func(names string) {
		t.Helper()
		upgradeContexts(t, dir, names)
	}
	a.cfg.Alerts.Kubectl = kubectl
	a.cfg.Alerts.Installations = []config.Installation{
		{Name: "home", Context: "home"}, {Name: "away", Context: "teleport.giantswarm.io-away"},
	}
	now := time.Date(2026, 9, 25, 8, 30, 0, 0, time.UTC)
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	words := func(at time.Time) string {
		t.Helper()
		return strings.Join(upgradeWords(a.readUpgrades(context.Background(), st, at), time.Time{}), " · ")
	}

	// The machine kubeconfig alone: away has no context.
	contexts("home\n")
	if got, want := words(now), "home none · 1 installation without a kube context, not read: away"; got != want {
		t.Fatalf("without away's context:\n%s\nwant\n%s", got, want)
	}
	if got := takeCalls(t, log); got["home"] != 1 || len(got) != 1 {
		t.Fatalf("kubectl calls without away's context: %v", got)
	}
	// Still without it a minute later: home's reading is used, away is not read.
	if got := words(now.Add(time.Minute)); !strings.Contains(got, "without a kube context, not read: away") {
		t.Fatalf("still without away's context: %s", got)
	}
	if got := takeCalls(t, log); len(got) != 0 {
		t.Fatalf("kubectl calls still without away's context: %v", got)
	}
	// The kubeconfig list with away's context: away is read at once, home's
	// fresh reading is used.
	contexts("home\nteleport.giantswarm.io-away\n")
	if got, want := words(now.Add(2*time.Minute)), "home none · away none"; got != want {
		t.Fatalf("with away's context:\n%s\nwant\n%s", got, want)
	}
	if got := takeCalls(t, log); got["teleport.giantswarm.io-away"] != 1 || len(got) != 1 {
		t.Fatalf("kubectl calls with away's context: %v", got)
	}
}

func TestUpgradeHeldReaderFailuresReset(t *testing.T) {
	a, out := stubApp(t)
	dir := t.TempDir()
	kubectl, _ := upgradeKubectl(t, dir, snapshotInstallation)
	a.cfg.Alerts.Kubectl = kubectl
	a.cfg.Alerts.Installations = []config.Installation{{Name: snapshotInstallation, Context: snapshotInstallation}}
	clk := &stepClock{now: time.Date(2026, 9, 25, 8, 30, 0, 0, time.UTC)}
	w := a.newWatcher(false, false)
	w.clock = clk
	phase(t, dir, "0827-during")
	w.upgradeCycle(context.Background())
	if err := os.WriteFile(kubectl, []byte("#!/bin/sh\nkill -KILL $$\n"), 0700); err != nil { //nolint:gosec // a path of this test
		t.Fatal(err)
	}
	for range 2 {
		clk.now = clk.now.Add(time.Minute)
		w.upgradeCycle(context.Background())
	}
	if !strings.Contains(out.String(), "reader failed 2 consecutive ticks: signal: killed (deadline 2m0s") {
		t.Fatalf("no failure streak: %s", out.String())
	}
	upgradeKubectl(t, dir, snapshotInstallation)
	clk.now = clk.now.Add(time.Minute)
	w.upgradeCycle(context.Background())
	if w.upgradeFailures[snapshotInstallation] != 0 {
		t.Fatalf("failure streak not reset: %v", w.upgradeFailures)
	}
}

func TestUpgradeRetryDoesNotReportUnreadable(t *testing.T) {
	a, out := stubApp(t)
	upgradeContexts(t, t.TempDir(), snapshotInstallation)
	exe := filepath.Join(t.TempDir(), "kubectl")
	script := `#!/bin/sh
if [ "$1" = config ]; then echo prod; exit; fi
if [ ! -f "$0.once" ]; then touch "$0.once"; kill -KILL $$; fi
echo '{"items":[]}'
`
	if err := os.WriteFile(exe, []byte(script), 0700); err != nil { //nolint:gosec // a path of this test
		t.Fatal(err)
	}
	a.cfg.Alerts.Kubectl = exe
	a.cfg.Alerts.Installations = []config.Installation{{Name: snapshotInstallation, Context: snapshotInstallation}}
	w := a.newWatcher(false, false)
	w.upgradeCycle(context.Background())
	if strings.Contains(out.String(), "unreadable") || w.failed.Load() != 0 {
		t.Fatalf("successful retry reported a failure: %s", out.String())
	}
	if _, err := os.Stat(exe + ".once"); err != nil {
		t.Fatalf("first attempt did not run: %v", err)
	}
}

// Snapshot readers recover versions even when a shared reading skipped events.
func TestSnapshotUpgradeFromHold(t *testing.T) {
	a, out := stubApp(t)
	a.cfg.Alerts.Installations = []config.Installation{{Name: snapshotInstallation, Context: snapshotInstallation}}
	st := &state.State{}
	upgrade.Reconcile(st, snapshotInstallation, []upgrade.Upgrade{{Cluster: "mc", From: "35.0.1", To: "36.1.0"}}, a.now, state.Party{})
	rs := upgrade.Readings{}
	rs.Keep([]upgrade.Status{{Installation: snapshotInstallation, Upgrades: []upgrade.Upgrade{{Cluster: "mc", To: "36.1.0"}}}}, a.now)
	if err := a.store.WriteFile(upgrade.ReadingsFile, rs); err != nil {
		t.Fatal(err)
	}
	statuses := a.readUpgrades(context.Background(), st, a.now)
	a.printSnapshot(&snapshot{Holds: st.Holds, Upgrades: statuses})
	for _, line := range []string{"hold ", "upgrades: "} {
		found := false
		for text := range strings.Lines(out.String()) {
			if strings.HasPrefix(text, line) {
				found = true
				if !strings.Contains(text, "35.0.1 → 36.1.0") {
					t.Errorf("missing version: %s", text)
				}
			}
		}
		if !found {
			t.Errorf("missing %s line", line)
		}
	}
}

const snapshotInstallation = "prod"

func TestUpgradeWatchesSerializeObservationAndReconciliation(t *testing.T) {
	for _, firstRunning := range []bool{false, true} {
		t.Run(fmt.Sprintf("first-running-%t", firstRunning), func(t *testing.T) {
			a, _ := stubApp(t)
			first := a.newWatcher(false, false)
			// Separate stores model separate watch processes sharing a directory.
			other, err := state.OpenVersion(a.store.Dir(), "dev")
			if err != nil {
				t.Fatal(err)
			}
			copyApp := *a
			copyApp.store = other
			// Each watch gets its own output to avoid concurrent buffer writes.
			copyApp.out = io.Discard
			second := copyApp.newWatcher(false, false)
			observed, release := make(chan struct{}), make(chan struct{})
			doneFirst, doneSecond := make(chan struct{}), make(chan struct{})
			status := func(running bool) []upgrade.Status {
				s := upgrade.Status{Installation: snapshotInstallation}
				if running {
					s.Upgrades = []upgrade.Upgrade{{Cluster: "mc", To: "2"}}
				}
				return []upgrade.Status{s}
			}
			go func() {
				defer close(doneFirst)
				first.upgradeCycleRead(context.Background(), func(_ context.Context, _ *state.State, _ time.Time, _ upgrade.Readings, _ func(string) time.Duration) []upgrade.Status {
					close(observed)
					<-release
					return status(firstRunning)
				})
			}()
			<-observed
			seen := make(chan bool, 1)
			go func() {
				defer close(doneSecond)
				second.upgradeCycleRead(context.Background(), func(_ context.Context, st *state.State, now time.Time, _ upgrade.Readings, _ func(string) time.Duration) []upgrade.Status {
					_, held := upgrade.Held(st, snapshotInstallation, now)
					seen <- held
					return status(!firstRunning)
				})
			}()
			// The cycle lock orders watches, while the state lock stays free.
			lock := flock.New(filepath.Join(a.store.Dir(), "state.lock"))
			acquired, err := lock.TryLock()
			if acquired {
				_ = lock.Unlock()
			}
			if err == nil && acquired {
				if err := other.Update(func(st *state.State) ([]state.Event, error) {
					st.Timers = append(st.Timers, state.Timer{ID: 1, Due: a.now.Add(-time.Minute), Run: "true"})
					return nil, nil
				}); err != nil {
					t.Error(err)
				}
				timers, err := copyApp.dispatchTimerPass(t.Context())
				if err != nil {
					t.Error(err)
				} else {
					timers.timerActs.Wait()
					st, readErr := other.Read()
					if readErr != nil || len(st.Timers) != 0 {
						t.Errorf("timer did not settle during upgrade observation: %v", readErr)
					}
				}
			}
			close(release)
			<-doneFirst
			<-doneSecond
			if err != nil || !acquired {
				t.Fatalf("observation retained the state lock: acquired=%t err=%v", acquired, err)
			}
			if held := <-seen; held != firstRunning {
				t.Errorf("second observation saw held=%t, want %t", held, firstRunning)
			}
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			_, held := upgrade.Held(st, snapshotInstallation, time.Now())
			if held != !firstRunning {
				t.Errorf("final held=%t, want %t", held, !firstRunning)
			}
		})
	}
}

func TestDelayedUpgradeCachePublicationMergesLatest(t *testing.T) {
	a, _ := stubApp(t)
	other, err := state.OpenVersion(a.store.Dir(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	b := *a
	b.store = other
	now := time.Now()
	older := upgrade.Readings{}
	older.Keep([]upgrade.Status{{Installation: snapshotInstallation}, {Installation: "old-only"}}, now)
	newer := upgrade.Readings{}
	newer.Keep([]upgrade.Status{{Installation: snapshotInstallation, Upgrades: []upgrade.Upgrade{{Cluster: "mc"}}}, {Installation: "new-only"}}, now.Add(time.Minute))
	if err := b.publishUpgrades(context.Background(), newer); err != nil {
		t.Fatal(err)
	}
	// The older reader finishes after the newer reader has published.
	if err := a.publishUpgrades(context.Background(), older); err != nil {
		t.Fatal(err)
	}
	var disk upgrade.Readings
	if _, err := other.ReadFile(upgrade.ReadingsFile, &disk); err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]upgrade.Readings{"caller": older, loopDiskKind: disk} {
		if len(got) != 3 || !got[snapshotInstallation].At.Equal(now.Add(time.Minute)) || !got.Running(snapshotInstallation) {
			t.Errorf("%s lost newer or disjoint readings: %+v", name, got)
		}
	}
}
