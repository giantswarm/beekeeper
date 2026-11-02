package cmd

import (
	"bytes"
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/notify"
	"github.com/giantswarm/beekeeper/internal/state"
)

// desktop records what a watch sends.
type desktop struct {
	sent []notify.Message
	id   uint32
}

func (d *desktop) Send(_ context.Context, m notify.Message) (uint32, error) {
	d.sent = append(d.sent, m)
	d.id++
	return d.id, nil
}

// notifyingWatch is a watch --notify on the state in dir.
func notifyingWatch(t *testing.T, dir string, standby bool) (*watcher, *desktop, *bytes.Buffer) {
	t.Helper()
	cfg, err := config.Load(filepath.Join(dir, "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.StateDir = dir
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	d := &desktop{}
	w := &watcher{app: &app{cfg: cfg, store: store, out: &out, now: relayNow}, standby: standby, last: map[string]time.Time{}, reported: map[string]bool{},
		readHeadroom: func(context.Context, *swapReading) *headroom { return tight() }}
	w.notifier = notify.New(cfg.Notify.Policy(), dir, d, func(l string) { w.emitNow("notify", "%s", l) })
	w.doctoring.Store(true) // background maintenance is not under test in this output fixture
	t.Cleanup(func() {
		w.timerActs.Wait()
		w.stand.inflight.Wait()
		w.bg.Wait()
	})
	return w, d, &out
}

func TestWatchNotifiesEachDueOnceAcrossWatches(t *testing.T) {
	dir := t.TempDir()
	a, da, _ := notifyingWatch(t, dir, false)
	b, db, _ := notifyingWatch(t, dir, false)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) { *st = *pendingState(); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	live := []*claude.Session{{ID: "s4", HostID: hostFour, Name: agentFour}}
	for range 2 {
		b.pending(context.Background(), live)
		a.pending(context.Background(), live)
	}
	if len(da.sent) != 0 || len(db.sent) != 2 {
		t.Fatalf("a sent %d, b sent %d; want the defaulted note and the due timer once, from the watch that fired them", len(da.sent), len(db.sent))
	}
	if s := db.sent[0].Summary + "|" + db.sent[1].Summary; s != "beekeeper: timer #1 due: "+rollout+"|beekeeper: note #1 defaulted" {
		t.Errorf("summaries: %s", s)
	}
	if b := db.sent[1].Body; !strings.Contains(b, "for Timo: pick a threshold") || !strings.HasSuffix(b, "applied: the alert stays as is") {
		t.Errorf("note body: %q", b)
	}
	// A session ending with its record prints a line but needs no person.
	for _, m := range db.sent {
		if strings.Contains(m.Body, issue) {
			t.Errorf("a recorded session's end notified: %+v", m)
		}
	}
}

func TestStatusBar(t *testing.T) {
	for _, c := range []struct {
		v    statusView
		want string
	}{
		{statusView{}, "beekeeper\t!loop\t0\t0\t0\t0/0"},
		{statusView{SupervisorLoop: 42, Leases: []string{"a", "b"}, Holds: []string{"merges"}, Due: 3, Busy: 3, Ceiling: 10}, "beekeeper\tloop\t2\t1\t3\t3/10"},
		{statusView{Ceiling: 5}, "beekeeper\t!loop\t0\t0\t0\t0/5"},
	} {
		if got := c.v.bar(); got != c.want {
			t.Errorf("bar() = %q, want %q", got, c.want)
		}
	}
	st := pendingState()
	if n := dueCount(st, relayNow); n != 2 {
		t.Errorf("due = %d, want the past note and timer", n)
	}
}

func TestLiftedUpgradeHoldShowsWhoLiftedIt(t *testing.T) {
	const portalLane = "lane:portal"
	sup := state.Party{Name: "Supervisor run 17"}
	st := &state.State{Holds: []state.Hold{
		{Target: "upgrade:gazelle/cicddev", Reason: "upgrade gazelle/cicddev ? → 36.0.0", LiftedBy: &sup, LiftedAt: relayNow},
		{Target: portalLane, Reason: "stuck drain"},
	}}
	var out bytes.Buffer
	a := &app{out: &out, now: relayNow}
	if active := a.activeHolds(st); len(active) != 1 || active[0].Target != portalLane {
		t.Errorf("active holds %+v", active)
	}
	a.printHolds(append(a.activeHolds(st), liftedHolds(st, relayNow)...))
	lines := strings.Split(out.String(), "\n")
	if len(lines) < 3 || !strings.Contains(lines[0], "STATE") ||
		!regexp.MustCompile(portalLane+` +active +until lifted `).MatchString(lines[1]) ||
		!regexp.MustCompile(`upgrade:gazelle/cicddev +lifted by Supervisor run 17 \S+ +the upgrade ends `).MatchString(lines[2]) {
		t.Errorf("hold list:\n%s", out.String())
	}
	v := statusView{Holds: []string{portalLane}, Lifted: []string{"upgrade:gazelle/cicddev by Supervisor run 17"}}
	if got := v.line(); !strings.Contains(got, "1 hold ("+portalLane+"); 1 lifted (upgrade:gazelle/cicddev by Supervisor run 17)") {
		t.Errorf("status line %q", got)
	}
}

func TestStaleUpgradeHoldIsMarked(t *testing.T) {
	st := &state.State{Holds: []state.Hold{
		{Target: "upgrade:test/stalled", Reason: "upgrade test/stalled ? → 35.1.1", At: relayNow.Add(-25 * time.Hour)},
		{Target: "upgrade:gazelle/cicddev", Reason: "upgrade gazelle/cicddev 35.0.0 → 36.0.0", At: relayNow.Add(-time.Hour)},
	}}
	var out bytes.Buffer
	a := &app{out: &out, now: relayNow}
	a.printHolds(a.activeHolds(st))
	lines := strings.Split(out.String(), "\n")
	if len(lines) < 3 ||
		!regexp.MustCompile(`upgrade:test/stalled +active, stale +the upgrade ends `).MatchString(lines[1]) ||
		!regexp.MustCompile(`upgrade:gazelle/cicddev +active +the upgrade ends `).MatchString(lines[2]) {
		t.Errorf("hold list:\n%s", out.String())
	}
}

// The machine's lines are the supervisor's: a watch --notify prints them
// and sends none of them to the desktop, an imminent systemd-oomd kill
// among them.
func TestMachineLinesNeverNotify(t *testing.T) {
	needsPlatform(t)
	w, d, out := notifyingWatch(t, t.TempDir(), false)
	th := &w.cfg.Watch
	th.AvailMinMiB, th.LoadMax = 1<<30, 0.001
	w.sample(context.Background())
	for _, l := range []string{"LOW RAM", "HIGH LOAD"} {
		if !strings.Contains(out.String(), l) {
			t.Errorf("no %s line:\n%s", l, out)
		}
	}
	m := machine.Mem{SwapTotalMiB: 16383, SwapUsedMiB: 15000}
	oomd, grow := &machine.OOMDSwap{LimitPercent: 90, Monitored: []string{swapCgroup}}, swapTrend{DiskPerHourMiB: 100, AvailFalling: true, Rated: true}
	w.check("oomd", w.oomdImminent(m, oomd, grow), "OOMD IMMINENT: %s", swapLine(m, oomd, grow))
	if !strings.Contains(out.String(), "OOMD IMMINENT") {
		t.Errorf("no OOMD IMMINENT line:\n%s", out)
	}
	if len(d.sent) != 0 {
		t.Fatalf("machine lines notified: %+v", d.sent)
	}
}

func TestSwapLineSaysDiskZswapAndOomdRule(t *testing.T) {
	m := machine.Mem{SwapTotalMiB: 16383, SwapUsedMiB: 10627, ZswappedMiB: 8000, ZswapPoolMiB: 2500}
	unwatched := &machine.OOMDSwap{LimitPercent: 90}
	if got, want := swapLine(m, unwatched, swapTrend{}), "SWAP: 10627 of 16383 MiB used, disk 2627 MiB + zswap 8000 MiB in a 2500 MiB pool, systemd-oomd watches no cgroup for swap, growth not yet measured"; got != want {
		t.Errorf("unwatched:\n got %q\nwant %q", got, want)
	}
	if got := swapLine(m, unwatched, swapTrend{DiskPerHourMiB: 250, UsedPerHourMiB: 250, AvailFalling: true, Rated: true}); strings.Contains(got, "trigger") {
		t.Errorf("a trigger without a swap-monitored cgroup: %q", got)
	}
	watched := &machine.OOMDSwap{LimitPercent: 90, Monitored: []string{swapCgroup}}
	if got, want := swapLine(m, watched, swapTrend{DiskPerHourMiB: 250, UsedPerHourMiB: 250, AvailFalling: true, Rated: true, Window: 10 * time.Minute}), "SWAP: 10627 of 16383 MiB used, disk 2627 MiB + zswap 8000 MiB in a 2500 MiB pool, 4117 MiB before systemd-oomd's 90 % swap trigger (1 swap-monitored cgroups), disk +250 MiB/h, zswap writeback 0 MiB/h over the last 10m while MemAvailable falls, trigger in 16h28m0s"; got != want {
		t.Errorf("watched, growing:\n got %q\nwant %q", got, want)
	}
	if got := swapLine(m, nil, swapTrend{DiskPerHourMiB: -80, Rated: true, Window: time.Hour}); !strings.HasSuffix(got, "systemd-oomd swap rule unknown, disk -80 MiB/h, zswap writeback 0 MiB/h over the last 1h00m") {
		t.Errorf("shrinking: %q", got)
	}
}

// The swap trend is the rate over watch.swapWindow (10m), rated once the
// readings span five minutes; a burst older than the window has left it.
func TestSwapTrendOverTheWindow(t *testing.T) {
	cfg, err := config.Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	w := &watcher{app: &app{cfg: cfg}}
	at := func(used, zswapped, avail int) machine.Mem {
		return machine.Mem{SwapUsedMiB: used, ZswappedMiB: zswapped, AvailableMiB: avail}
	}
	window := 10 * time.Minute
	if tr := w.swapTrend(relayNow, at(1000, 0, 40000)); tr.Rated || tr.Window != window {
		t.Fatalf("one reading has no rate: %+v", tr)
	}
	if w.swapTrend(relayNow.Add(time.Minute), at(1010, 0, 40000)).Rated {
		t.Fatal("a minute is too short a span")
	}
	// 500 MiB more in use in 6 minutes, 400 of it into zswap.
	if tr := w.swapTrend(relayNow.Add(6*time.Minute), at(1500, 400, 39000)); tr != (swapTrend{DiskPerHourMiB: 1000, UsedPerHourMiB: 5000, AvailFalling: true, Rated: true, Window: window}) {
		t.Fatalf("6 minutes: %+v", tr)
	}
	if tr := w.swapTrend(relayNow.Add(25*time.Minute), at(1500, 400, 41000)); tr.DiskPerHourMiB != 0 || tr.Rated || len(w.swapSamples) != 1 {
		t.Fatalf("readings older than the window are dropped: %+v %d", tr, len(w.swapSamples))
	}
}

// zswap's writeback moves pages already swapped from its pool to disk: with
// swap in use flat it is no disk swap growth, and new swap-outs alongside it
// still are.
func TestSwapTrendCountsZswapWritebackApart(t *testing.T) {
	cfg, err := config.Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	at := func(used, zswapped, writtenBack int) machine.Mem {
		return machine.Mem{SwapUsedMiB: used, ZswappedMiB: zswapped, ZswapWrittenBackMiB: writtenBack, AvailableMiB: 30000}
	}
	w := &watcher{app: &app{cfg: cfg}}
	w.swapTrend(relayNow, at(10000, 4000, 50000))
	// 300 MiB written back in 6 minutes, swap in use flat: disk 6000 → 6300.
	tr := w.swapTrend(relayNow.Add(6*time.Minute), at(10000, 3700, 50300))
	if tr.DiskPerHourMiB != 0 || tr.WritebackPerHourMiB != 3000 || tr.UsedPerHourMiB != 0 || tr.pressing() {
		t.Fatalf("writeback with swap flat is no disk growth: %+v", tr)
	}
	if line := swapLine(at(10000, 3700, 50300), nil, tr); !strings.Contains(line, "disk +0 MiB/h, zswap writeback 3000 MiB/h") {
		t.Errorf("the line does not say the two apart: %s", line)
	}
	h := &headroom{SwapGrowthMaxMiB: 256, Swap: &swapReading{Rated: true, PerHourMiB: tr.DiskPerHourMiB, WritebackPerHourMiB: tr.WritebackPerHourMiB}}
	if line, blocks := h.swapGuard(); line != "" || blocks {
		t.Errorf("the guard reacts to writeback: %q %v", line, blocks)
	}
	// The same writeback and 100 MiB swapped out to disk past zswap.
	w = &watcher{app: &app{cfg: cfg}}
	w.swapTrend(relayNow, at(10000, 4000, 50000))
	tr = w.swapTrend(relayNow.Add(6*time.Minute), at(10100, 3700, 50300))
	if tr.DiskPerHourMiB != 1000 || tr.WritebackPerHourMiB != 3000 {
		t.Fatalf("new swap-outs beside writeback: %+v", tr)
	}
	h.Swap = &swapReading{Rated: true, PerHourMiB: tr.DiskPerHourMiB}
	if line, _ := h.swapGuard(); !strings.Contains(line, "disk swap growing +1000 MiB/h over 256") {
		t.Errorf("the guard misses new swap-outs: %q", line)
	}
}

// Swap in use means pressure only while disk swap grows and MemAvailable
// falls, and oomd's swap kill is imminent only when it watches a cgroup.
func TestOomdImminentNeedsAWatchedCgroupAndDiskGrowth(t *testing.T) {
	cfg, err := config.Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	w := &watcher{app: &app{cfg: cfg}}
	full := machine.Mem{SwapTotalMiB: 16383, SwapUsedMiB: 16383, ZswappedMiB: 14000}
	grow := swapTrend{DiskPerHourMiB: 100, UsedPerHourMiB: 100, AvailFalling: true, Rated: true}
	watched := &machine.OOMDSwap{LimitPercent: 90, Monitored: []string{swapCgroup}}
	for name, c := range map[string]struct {
		oomd *machine.OOMDSwap
		t    swapTrend
		want bool
	}{
		"watched, growing":      {watched, grow, true},
		"unwatched":             {&machine.OOMDSwap{LimitPercent: 90}, grow, false},
		"unknown":               {nil, grow, false},
		"full, flat":            {watched, swapTrend{Rated: true, AvailFalling: true}, false},
		"growing, RAM recovers": {watched, swapTrend{DiskPerHourMiB: 100, Rated: true}, false},
		"growth not yet rated":  {watched, swapTrend{}, false},
	} {
		if got := w.oomdImminent(full, c.oomd, c.t); got != c.want {
			t.Errorf("%s: imminent %v, want %v", name, got, c.want)
		}
	}
}

// The swap check keys on disk swap: zswap's share sits in RAM.
func TestSwapOverIgnoresZswap(t *testing.T) {
	grow := swapTrend{DiskPerHourMiB: 100, AvailFalling: true, Rated: true}
	zswapped := machine.Mem{SwapTotalMiB: 16383, SwapUsedMiB: 16383, ZswappedMiB: 14000}
	if swapOver(zswapped, grow, 10000) {
		t.Error("swap over its max with 2383 MiB on disk")
	}
	onDisk := machine.Mem{SwapTotalMiB: 16383, SwapUsedMiB: 16383, ZswappedMiB: 1000}
	if !swapOver(onDisk, grow, 10000) {
		t.Error("15383 MiB on disk and growing is not over its max")
	}
	if swapOver(onDisk, swapTrend{Rated: true, AvailFalling: true}, 10000) {
		t.Error("disk swap that does not grow is over its max")
	}
}

// swapCgroup is a swap-monitored cgroup of the tests' oomd.
const swapCgroup = "/user.slice"
