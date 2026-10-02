package cmd

import (
	"bytes"
	"context"
	"path/filepath"
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
	w := &watcher{app: &app{cfg: cfg, store: store, out: &out, now: relayNow}, standby: standby, last: map[string]time.Time{}, reported: map[string]bool{}}
	w.notifier = notify.New(cfg.Notify.Policy(), dir, d, func(l string) { w.emitNow("notify", "%s", l) })
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
		t.Fatalf("a sent %d, b sent %d; want the due note and timer once, from the watch that fired them", len(da.sent), len(db.sent))
	}
	if s := db.sent[0].Summary + "|" + db.sent[1].Summary; s != "beekeeper: note #1 due|beekeeper: timer #1 due: "+rollout {
		t.Errorf("summaries: %s", s)
	}
	if b := db.sent[0].Body; !strings.Contains(b, "for Timo: pick a threshold") || !strings.Contains(b, "if unanswered: the alert stays as is") || !strings.HasSuffix(b, "beekeeper note list") {
		t.Errorf("note body: %q", b)
	}
	// A session ending with its record prints a line but needs no person.
	for _, m := range db.sent {
		if strings.Contains(m.Body, issue) {
			t.Errorf("a recorded session's end notified: %+v", m)
		}
	}
}

func TestSupervisorGoneNotifiesOnceAndEndsStandby(t *testing.T) {
	dir := t.TempDir()
	unit, du, out := notifyingWatch(t, dir, true)
	sup, ds, _ := notifyingWatch(t, dir, false)
	st := pendingState()
	st.Supervisor = &state.Supervisor{Party: four, Since: relayNow.Add(-time.Hour)}
	if err := unit.store.Update(func(s *state.State) ([]state.Event, error) { *s = *st; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	live := []*claude.Session{{ID: "s4", HostID: hostFour, Name: agentFour}}
	unit.pending(context.Background(), live)
	if len(du.sent) != 0 || strings.Contains(out.String(), "TIMER") {
		t.Fatalf("a standby watch fired the supervisor's timer: %s %+v", out, du.sent)
	}
	// The supervisor's session ends: once the restart grace has passed, one
	// line per watch and one critical notification; the standby watch
	// reports the pending itself from the first poll that misses the
	// supervisor.
	poll := func(at time.Duration, sessions []*claude.Session) {
		unit.now, sup.now = relayNow.Add(at), relayNow.Add(at)
		unit.pending(context.Background(), sessions)
		sup.pending(context.Background(), sessions)
	}
	gone := func() (n int) {
		for _, m := range append(du.sent, ds.sent...) {
			if m.Summary == "beekeeper: no supervisor, claims gated" && m.Urgency == notify.Critical && strings.Contains(m.Body, `"Agent four"`) {
				n++
			}
		}
		return n
	}
	poll(0, nil)
	poll(20*time.Second, nil)
	// A CLI back within the grace (30s) is a restart: nothing to say.
	poll(22*time.Second, live)
	if strings.Contains(out.String(), "SUPERVISOR") || gone() != 0 {
		t.Fatalf("a restart within the grace was said:\n%s", out)
	}
	poll(25*time.Second, nil)
	poll(45*time.Second, nil)
	if strings.Contains(out.String(), "SUPERVISOR GONE") {
		t.Fatalf("gone within the grace:\n%s", out)
	}
	poll(2*time.Minute, nil)
	poll(3*time.Minute, nil)
	poll(10*time.Minute, nil)
	if gone() != 1 || strings.Count(out.String(), "SUPERVISOR GONE") != 1 || !strings.Contains(out.String(), "claims stay gated") {
		t.Fatalf("no-supervisor: %d notifications, lines:\n%s", gone(), out)
	}
	if !strings.Contains(out.String(), "TIMER: #1") {
		t.Errorf("the standby watch leaves the timer to a supervisor that is gone:\n%s", out)
	}
	// While it lasts, again after notify.repeat, still one line.
	poll(2*time.Minute+30*time.Minute, nil)
	poll(3*time.Minute+30*time.Minute, nil)
	if gone() != 2 || strings.Count(out.String(), "SUPERVISOR GONE") != 1 {
		t.Fatalf("repeat: %d notifications, lines:\n%s", gone(), out)
	}
	// Its CLI back: one line, and no more notifications.
	poll(40*time.Minute, live)
	poll(41*time.Minute, live)
	poll(80*time.Minute, live)
	if strings.Count(out.String(), "SUPERVISOR BACK: \"Agent four\"") != 1 || gone() != 2 {
		t.Fatalf("back: %d notifications, lines:\n%s", gone(), out)
	}
	// Supervision ended on purpose: nothing to say.
	if err := unit.store.Update(func(s *state.State) ([]state.Event, error) { s.Supervisor = nil; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	poll(90*time.Minute, nil)
	poll(95*time.Minute, nil)
	if strings.Count(out.String(), "SUPERVISOR") != 2 || gone() != 2 {
		t.Fatalf("after supervisor stop: %d notifications, lines:\n%s", gone(), out)
	}
}

func TestStatusBar(t *testing.T) {
	for _, c := range []struct {
		v    statusView
		want string
	}{
		{statusView{}, "beekeeper\t-\t0\t0\t0\t0/0"},
		{statusView{Supervisor: "night\twatch", SupervisorLive: true, Leases: []string{"a", "b"}, Holds: []string{"merges"}, Due: 3, Busy: 3, Floor: 5, Ceiling: 10}, "beekeeper\tnight watch\t2\t1\t3\t3/5"},
		{statusView{Supervisor: "run 11", Floor: 5}, "beekeeper\t!run 11\t0\t0\t0\t0/5"},
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
	a.printHolds(liftedHolds(st, relayNow))
	if !strings.Contains(out.String(), `lifted by Supervisor run 17`) {
		t.Errorf("hold list:\n%s", out.String())
	}
	v := statusView{Holds: []string{portalLane}, Lifted: []string{"upgrade:gazelle/cicddev by Supervisor run 17"}}
	if got := v.line(); !strings.Contains(got, "1 hold ("+portalLane+"); 1 lifted (upgrade:gazelle/cicddev by Supervisor run 17)") {
		t.Errorf("status line %q", got)
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
	if got, want := swapLine(m, watched, swapTrend{DiskPerHourMiB: 250, UsedPerHourMiB: 250, AvailFalling: true, Rated: true}), "SWAP: 10627 of 16383 MiB used, disk 2627 MiB + zswap 8000 MiB in a 2500 MiB pool, 4117 MiB before systemd-oomd's 90 % swap trigger (1 swap-monitored cgroups), disk +250 MiB/h over the last hour while MemAvailable falls, trigger in 16h28m0s"; got != want {
		t.Errorf("watched, growing:\n got %q\nwant %q", got, want)
	}
	if got := swapLine(m, nil, swapTrend{DiskPerHourMiB: -80, Rated: true}); !strings.HasSuffix(got, "systemd-oomd swap rule unknown, disk -80 MiB/h over the last hour") {
		t.Errorf("shrinking: %q", got)
	}
}

func TestSwapTrendOverTheLastHour(t *testing.T) {
	w := &watcher{}
	at := func(used, zswapped, avail int) machine.Mem {
		return machine.Mem{SwapUsedMiB: used, ZswappedMiB: zswapped, AvailableMiB: avail}
	}
	if w.swapTrend(relayNow, at(1000, 0, 40000)).Rated {
		t.Fatal("one reading has no rate")
	}
	if w.swapTrend(relayNow.Add(time.Minute), at(1010, 0, 40000)).Rated {
		t.Fatal("a minute is too short a span")
	}
	// 500 MiB more in use in 30 minutes, 400 of it into zswap.
	if tr := w.swapTrend(relayNow.Add(30*time.Minute), at(1500, 400, 39000)); tr != (swapTrend{DiskPerHourMiB: 200, UsedPerHourMiB: 1000, AvailFalling: true, Rated: true}) {
		t.Fatalf("30 minutes: %+v", tr)
	}
	if tr := w.swapTrend(relayNow.Add(150*time.Minute), at(1500, 400, 41000)); tr.DiskPerHourMiB != 0 || len(w.swapSamples) != 1 {
		t.Fatalf("readings older than an hour are dropped: %+v %d", tr, len(w.swapSamples))
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
