package cmd

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A relay due said at one relayAt is said again once relayAt changed, a
// record from before relayAt was kept among them.
func TestRelayDueAgainAtAChangedRelayAt(t *testing.T) {
	sup := &state.Supervisor{Party: agentC, Since: relayNow.Add(-time.Hour)}
	for _, said := range []*state.RelayDue{
		{Supervisor: agentC, Since: sup.Since, Reported: relayNow.Add(-4 * 24 * time.Hour), RelayAt: 150_000},
		{Supervisor: agentC, Since: sup.Since, Reported: relayNow.Add(-4 * 24 * time.Hour)}, // before relayAt was kept
	} {
		st := &state.State{}
		guideRole.set(st, state.Role{Holder: sup, RelayDue: said})
		q := quietness{checked: true, context: 537_000, relayAt: 400_000}
		lines, _ := guideRole.fireRelayDue(st, q, nil, relayNow)
		if len(lines) != 1 || !strings.HasPrefix(lines[0], `GUIDE RELAY DUE: "Agent three" is at 537k`) {
			t.Fatalf("relay due at the new relayAt: %q", lines)
		}
		if d := guideRole.get(st).RelayDue; !d.At(sup, 400_000) || !d.Reported.Equal(relayNow) {
			t.Fatalf("record at the new relayAt: %+v", d)
		}
		if lines, _ := guideRole.fireRelayDue(st, q, nil, relayNow.Add(time.Minute)); lines != nil {
			t.Fatalf("relay due twice at the same relayAt: %q", lines)
		}
	}
}

// A watch process that did not say a standing relay due says it once, in
// both of its poll's passes, and keeps when it was first reported.
func TestRelayDueOncePerWatchProcess(t *testing.T) {
	const relayAt = 150_000
	since, first := relayNow.Add(-3*time.Hour), relayNow.Add(-2*time.Hour)
	st := &state.State{
		Supervisor: &state.Supervisor{Party: supA, Since: since},
		RelayDue:   &state.RelayDue{Supervisor: supA, Since: since, Reported: first, Context: 151_000, RelayAt: relayAt},
	}
	sessions := []*claude.Session{{ID: supA.Session, HostID: supA.HostSession, Name: supA.Name, Transcript: supervisorTranscript}}
	q := func(said relayDues, now time.Time) quietness {
		c := relayContext(st, sessions, now, relayAt, said)
		return quietness{checked: c > 0, context: c, relayAt: relayAt}
	}
	if c := relayContext(st, sessions, relayNow, relayAt, nil); c != 0 {
		t.Fatalf("a watch --once reads the context of a said relay due: %d", c)
	}
	if lines, _ := fireRelayDue(st, q(nil, relayNow), nil, relayNow); lines != nil {
		t.Fatalf("a watch --once said a standing relay due: %q", lines)
	}
	for i, said := range []relayDues{{}, {}} {
		now := relayNow.Add(time.Duration(i) * time.Hour)
		for pass := range 2 { // the poll's read, then its write under the lock
			if lines, _ := fireRelayDue(st, q(said, now), said, now); len(lines) != 1 {
				t.Fatalf("watch %d, pass %d: %q", i, pass, lines)
			}
		}
		later := now.Add(time.Minute)
		if lines, _ := fireRelayDue(st, q(said, later), said, later); lines != nil {
			t.Fatalf("watch %d said it on its next poll: %q", i, lines)
		}
	}
	if !st.RelayDue.Reported.Equal(first) {
		t.Fatalf("a repeat moved the report: %v", st.RelayDue.Reported)
	}
}

func TestPastRelayGrace(t *testing.T) {
	cfg := config.Role{RelayAt: 150_000, RelayGrace: config.Duration{Duration: 30 * time.Minute}}
	sup := &state.Supervisor{Party: supA, Since: relayNow.Add(-time.Hour)}
	due := func(ago time.Duration, at int64) *state.RelayDue {
		return &state.RelayDue{Supervisor: supA, Since: sup.Since, Reported: relayNow.Add(-ago), RelayAt: at}
	}
	open := &state.Relay{From: supA, To: supB, At: relayNow, Expires: relayNow.Add(15 * time.Minute)}
	for _, c := range []struct {
		name string
		r    state.Role
		want bool
	}{
		{"past the grace", state.Role{Holder: sup, RelayDue: due(30*time.Minute, 150_000)}, true},
		{"within the grace", state.Role{Holder: sup, RelayDue: due(29*time.Minute, 150_000)}, false},
		{"said at another relayAt", state.Role{Holder: sup, RelayDue: due(time.Hour, 400_000)}, false},
		{"not said", state.Role{Holder: sup}, false},
		{"a relay open", state.Role{Holder: sup, RelayDue: due(time.Hour, 150_000), Relay: open}, false},
		{"no holder", state.Role{RelayDue: due(time.Hour, 150_000)}, false},
	} {
		if got := pastRelayGrace(c.r, cfg, relayNow); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// The standby watch relays a live supervisor or guide that stays over its
// relayAt past relayGrace after its relay due; the supervisor only at a
// quiet moment, and a role once until the backoff passed.
func TestStandbyRelaysAHolderOverItsRelayAt(t *testing.T) {
	for _, rl := range roles {
		t.Run(rl.name, func(t *testing.T) {
			w, _, out := notifyingWatch(t, t.TempDir(), true)
			w.cfg.LeaseDir = t.TempDir()
			for _, c := range []*config.Role{&w.cfg.Supervisor.Role, &w.cfg.Guide.Role} {
				c.RelayAt = 150_000
			}
			var from atomic.Value
			var started atomic.Int32
			w.stand = standbyWatch{succeed: func(_ context.Context, got role, holder state.Party) (state.Party, error) {
				if got.name != rl.name {
					t.Errorf("relayed the %s", got.name)
				}
				from.Store(holder.Name)
				started.Add(1)
				return supB, nil
			}}
			t.Cleanup(w.stand.inflight.Wait)
			sup := &state.Supervisor{Party: supA, Since: relayNow.Add(-3 * time.Hour)}
			st := &state.State{}
			rl.set(st, state.Role{Holder: sup,
				RelayDue: &state.RelayDue{Supervisor: supA, Since: sup.Since, Reported: relayNow.Add(-31 * time.Minute), RelayAt: 150_000}})
			sessions := []*claude.Session{{ID: supA.Session, HostID: supA.HostSession, Name: supA.Name, Transcript: supervisorTranscript}}

			if rl.grants {
				st.Merges = []state.Merge{{Repo: modelManager, PR: 172, Phase: state.Running, PID: 1}}
				w.relayOverdue(context.Background(), rl, st, sessions)
				if started.Load() != 0 {
					t.Fatal("relayed the supervisor while a merge runs")
				}
				st.Merges = nil
			}
			w.relayOverdue(context.Background(), rl, st, sessions)
			w.stand.inflight.Wait()
			if started.Load() != 1 || from.Load() != supA.Name {
				t.Fatalf("%d relays from %v", started.Load(), from.Load())
			}
			if l := out.String(); !strings.Contains(l, rl.tag+`RELAYED: "Supervisor run 11" is at 163k tokens of context, over 150k`) || !strings.Contains(l, `started "Supervisor run 12"`) {
				t.Fatalf("watch lines:\n%s", l)
			}
			w.relayOverdue(context.Background(), rl, st, sessions)
			w.now = w.now.Add(successorBackoff)
			w.relayOverdue(context.Background(), rl, st, sessions)
			w.stand.inflight.Wait()
			if n := started.Load(); n != 2 {
				t.Fatalf("%d relays, want a second only past the backoff", n)
			}
		})
	}
}

// Under its relayAt, the holder is not relayed.
func TestStandbyLeavesAHolderUnderItsRelayAt(t *testing.T) {
	w, _, _ := notifyingWatch(t, t.TempDir(), true)
	w.cfg.LeaseDir = t.TempDir()
	w.cfg.Guide.RelayAt = 400_000
	w.stand = standbyWatch{succeed: func(context.Context, role, state.Party) (state.Party, error) {
		t.Error("relayed a guide under its relayAt")
		return state.Party{}, nil
	}}
	t.Cleanup(w.stand.inflight.Wait)
	sup := &state.Supervisor{Party: supA, Since: relayNow.Add(-3 * time.Hour)}
	st := &state.State{}
	guideRole.set(st, state.Role{Holder: sup,
		RelayDue: &state.RelayDue{Supervisor: supA, Since: sup.Since, Reported: relayNow.Add(-time.Hour), RelayAt: 400_000}})
	w.relayOverdue(context.Background(), guideRole, st, []*claude.Session{{ID: supA.Session, Name: supA.Name, Transcript: supervisorTranscript}})
}
