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
		{statusView{}, "beekeeper\t-\t0\t0\t0"},
		{statusView{Supervisor: "night\twatch", SupervisorLive: true, Leases: []string{"a", "b"}, Holds: []string{"merges"}, Due: 3}, "beekeeper\tnight watch\t2\t1\t3"},
		{statusView{Supervisor: "run 11"}, "beekeeper\t!run 11\t0\t0\t0"},
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
	sup := state.Party{Name: "Supervisor run 17"}
	st := &state.State{Holds: []state.Hold{
		{Target: "upgrade:gazelle/cicddev", Reason: "upgrade gazelle/cicddev ? → 36.0.0", LiftedBy: &sup, LiftedAt: relayNow},
		{Target: "lane:portal", Reason: "stuck drain"},
	}}
	var out bytes.Buffer
	a := &app{out: &out, now: relayNow}
	if active := a.activeHolds(st); len(active) != 1 || active[0].Target != "lane:portal" {
		t.Errorf("active holds %+v", active)
	}
	a.printHolds(liftedHolds(st, relayNow))
	if !strings.Contains(out.String(), `lifted by Supervisor run 17`) {
		t.Errorf("hold list:\n%s", out.String())
	}
	v := statusView{Holds: []string{"lane:portal"}, Lifted: []string{"upgrade:gazelle/cicddev by Supervisor run 17"}}
	if got := v.line(); !strings.Contains(got, "1 hold (lane:portal); 1 lifted (upgrade:gazelle/cicddev by Supervisor run 17)") {
		t.Errorf("status line %q", got)
	}
}
