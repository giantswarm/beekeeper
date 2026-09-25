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
	// The supervisor's session ends: after two polls, one line per watch and
	// one notification, then the standby watch reports the pending itself.
	for range 3 {
		unit.pending(context.Background(), nil)
		sup.pending(context.Background(), nil)
	}
	gone := 0
	for _, m := range append(du.sent, ds.sent...) {
		if m.Summary == "beekeeper: no supervisor" {
			gone++
		}
	}
	if gone != 1 || strings.Count(out.String(), "SUPERVISOR GONE") != 1 {
		t.Fatalf("no-supervisor: %d notifications, lines:\n%s", gone, out)
	}
	if !strings.Contains(out.String(), "TIMER: #1") {
		t.Errorf("the standby watch leaves the timer to a supervisor that is gone:\n%s", out)
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
