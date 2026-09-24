package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
)

const browser = "browser"

var (
	supA   = state.Party{Session: "sA", HostSession: "local_A", Name: "Supervisor run 11"}
	supB   = state.Party{Session: "sB", HostSession: "local_B", Name: "Supervisor run 12"}
	agentC = state.Party{Session: "sC", Name: "Agent three"}
	agentD = state.Party{Session: "sD", Name: "Agent five"}
)

func handOverState() *state.State {
	return &state.State{
		Supervisor: &state.Supervisor{Party: supA, Since: relayNow.Add(-8 * time.Hour)},
		Grants: []state.Grant{
			{Resource: browser, To: four, By: supA, At: relayNow.Add(-time.Minute)},
			{Resource: browser, To: agentD, By: supA, At: relayNow},
		},
	}
}

// claimGated checks a claim of the browser by who against st's supervisor.
func claimGated(st *state.State, who state.Party) error {
	_, err := lease.Check(st, lease.Gate{Resource: browser, Caller: who, Supervisor: st.Supervisor, Held: true, Now: relayNow, TTL: 30 * time.Minute})
	return err
}

func TestRelayMovesTheRoleAndTheGrants(t *testing.T) {
	st := handOverState()
	var log []state.Event
	if _, _, err := relayRole(st, supB, agentC, relayNow, 15*time.Minute); Code(err) != ExitRefused {
		t.Fatalf("a session that does not supervise relayed: %v", err)
	}
	msg, evs, err := relayRole(st, supA, supB, relayNow, 15*time.Minute)
	if err != nil || !strings.Contains(msg, `relayed to "Supervisor run 12"`) {
		t.Fatalf("relay: %q, %v", msg, err)
	}
	log = append(log, evs...)
	if claimGated(st, agentC) == nil || claimGated(st, agentD) == nil {
		t.Fatal("a claim went ungated during the hand-over")
	}
	if _, _, err := startRole(st, agentC, true, false, relayNow); Code(err) != ExitRefused || !strings.Contains(err.Error(), `its relay names "Supervisor run 12", not you`) {
		t.Fatalf("an unnamed start was not refused: %v", err)
	}
	if relievedBy(st, supA) != nil {
		t.Fatal("relieved before the successor started")
	}
	msg, evs, err = startRole(st, supB, true, false, relayNow.Add(time.Minute))
	if err != nil || !strings.Contains(msg, `relieving "Supervisor run 11"`) || !strings.Contains(msg, "2 grant(s) move over") {
		t.Fatalf("start: %q, %v", msg, err)
	}
	log = append(log, evs...)
	if !st.Supervisor.Is(supB) {
		t.Fatalf("supervisor is %q", st.Supervisor.Name)
	}
	for _, g := range st.Grants {
		if !g.By.Is(supB) {
			t.Errorf("grant of %s to %q is still by %q", g.Resource, g.To.Name, g.By.Name)
		}
	}
	if claimGated(st, agentC) == nil || claimGated(st, agentD) == nil || claimGated(st, supA) == nil {
		t.Fatal("a claim went ungated after the hand-over")
	}
	if relievedBy(st, supA) == nil || relievedBy(st, supB) != nil {
		t.Fatal("status does not tell the outgoing supervisor it has been relieved")
	}
	if len(log) != 2 || log[0].Verb != "supervisor.relay" || log[1].Verb != "supervisor.start" {
		t.Fatalf("the event log lacks the two steps: %+v", log)
	}
	lines, _ := fireRelay(st, relayNow.Add(time.Minute))
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "RELAY TAKEN:") {
		t.Fatalf("taken: %q", lines)
	}
	if lines, _ := fireRelay(st, relayNow.Add(2*time.Minute)); len(lines) != 0 {
		t.Fatalf("taken reported twice: %q", lines)
	}
}

func TestRelayExpiresAndCancels(t *testing.T) {
	st := handOverState()
	if _, _, err := relayRole(st, supA, supB, relayNow, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	later := relayNow.Add(16 * time.Minute)
	if lines, _ := fireRelay(st, relayNow.Add(time.Minute)); len(lines) != 0 {
		t.Fatalf("open relay reported: %q", lines)
	}
	if _, _, err := startRole(st, supB, true, false, later); Code(err) != ExitRefused || !strings.Contains(err.Error(), "its relay to you expired") {
		t.Fatalf("an expired relay was taken: %v", err)
	}
	if !st.Supervisor.Is(supA) {
		t.Fatal("the outgoing supervisor lost the role to an expired relay")
	}
	lines, evs := fireRelay(st, later)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "RELAY EXPIRED:") || len(evs) != 1 {
		t.Fatalf("expired: %q", lines)
	}
	if lines, _ := fireRelay(st, later); len(lines) != 0 {
		t.Fatalf("expiry reported twice: %q", lines)
	}

	if _, _, err := relayRole(st, supA, supB, later, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	msg, evs, err := cancelRelay(st, supA, later)
	if err != nil || st.Relay != nil || len(evs) != 1 || !strings.Contains(msg, "cancelled") {
		t.Fatalf("cancel: %q, %v", msg, err)
	}
	if _, _, err := startRole(st, supB, true, false, later); Code(err) != ExitRefused {
		t.Fatalf("a cancelled relay was taken: %v", err)
	}
	if _, _, err := startRole(st, supB, false, false, later); err != nil || !st.Supervisor.Is(supB) {
		t.Fatalf("a start over a gone supervisor: %v", err)
	}
}

func TestRelayDueOnlyAtAQuietMoment(t *testing.T) {
	cfg := &config.Config{
		GrantTTL: config.Duration{Duration: 30 * time.Minute},
		Lanes:    []config.Lane{{Name: serving, Installation: gazelle, Repositories: []string{modelManager}}},
		Merge:    config.Merge{Settle: config.Duration{Duration: 5 * time.Minute}, SettleTimeout: config.Duration{Duration: 30 * time.Minute}},
	}
	shift := 8 * time.Hour
	sessions := []*claude.Session{{ID: supA.Session, HostID: supA.HostSession, Name: supA.Name}}
	st := &state.State{Supervisor: &state.Supervisor{Party: supA, Since: relayNow.Add(-shift + time.Minute)}}
	if shiftOver(st, sessions, relayNow, shift) {
		t.Fatal("shift over a minute early")
	}
	now := relayNow.Add(time.Minute)
	if !shiftOver(st, sessions, now, shift) || shiftOver(st, nil, now, shift) || shiftOver(st, sessions, now, 0) {
		t.Fatal("shiftOver")
	}
	never := func(int) bool { return false }
	notRolled := func(state.Merge, config.Lane) bool { return false }
	busy := func() string { return busyWith(st, cfg, map[string]bool{}, now, never, notRolled) }

	st.Merges = []state.Merge{{Repo: modelManager, PR: 172, Lane: serving, Phase: state.Settling, Finished: now.Add(-time.Minute)}}
	if b := busy(); b != "giantswarm/model-manager#172 settles" {
		t.Fatalf("settling merge: %q", b)
	}
	lines, _, _ := fireShift(st, quietness{checked: true, busy: busy()}, now, shift)
	if len(lines) != 0 {
		t.Fatalf("relay due while a merge settles: %q", lines)
	}
	st.Merges = nil
	st.Grants = []state.Grant{{Resource: browser, To: four, By: supA, At: now}}
	if b := busy(); b != `browser is granted to "Agent four" and not claimed yet` {
		t.Fatalf("waiting grant: %q", b)
	}
	st.Grants = nil
	lines, evs, changed := fireShift(st, quietness{checked: true}, now, shift)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "RELAY DUE:") || len(evs) != 1 || !changed {
		t.Fatalf("relay due at a quiet moment: %q", lines)
	}
	if lines, _, changed := fireShift(st, quietness{checked: true}, now.Add(time.Minute), shift); len(lines) != 0 || changed {
		t.Fatalf("relay due twice in one quiet period: %q", lines)
	}
	if lines, _, changed := fireShift(st, quietness{checked: true, busy: "x merges"}, now.Add(2*time.Minute), shift); len(lines) != 0 || !changed {
		t.Fatalf("busy moment: %q, changed %v", lines, changed)
	}
	if lines, _, _ := fireShift(st, quietness{checked: true}, now.Add(3*time.Minute), shift); len(lines) != 1 {
		t.Fatalf("relay due not repeated after a busy moment: %q", lines)
	}
	st.Shift.Quiet = false
	st.Relay = &state.Relay{From: supA, To: supB, Expires: now.Add(time.Hour)}
	if shiftOver(st, sessions, now, shift) {
		t.Fatal("relay due while a relay is open")
	}
}

func TestSettlesByTheGateRule(t *testing.T) {
	cfg := &config.Config{
		Lanes: []config.Lane{{Name: serving, Installation: gazelle, Repositories: []string{modelManager}}},
		Merge: config.Merge{Settle: config.Duration{Duration: 5 * time.Minute}, SettleTimeout: config.Duration{Duration: 30 * time.Minute}},
	}
	rolled := func(state.Merge, config.Lane) bool { return true }
	notRolled := func(state.Merge, config.Lane) bool { return false }
	m := state.Merge{Repo: modelManager, PR: 1, Lane: serving, Phase: state.Settling, Finished: relayNow.Add(-time.Minute)}
	own := state.Merge{Repo: devctlRepo, PR: 2, Lane: devctlRepo, Phase: state.Settling, Finished: relayNow}
	known := m
	known.Release = "v1.2.3"
	stuck := known
	stuck.Finished = relayNow.Add(-time.Hour)
	for _, c := range []struct {
		name   string
		m      state.Merge
		rolled func(state.Merge, config.Lane) bool
		want   bool
	}{
		{"no installation to wait for", own, notRolled, false},
		{"unknown release within settle", m, notRolled, true},
		{"known release rolling", known, notRolled, true},
		{"known release rolled", known, rolled, false},
		{"past the settle timeout", stuck, notRolled, false},
	} {
		if got := settles(c.m, cfg, relayNow, c.rolled); got != c.want {
			t.Errorf("%s: settles %v, want %v", c.name, got, c.want)
		}
	}
	m.Finished = relayNow.Add(-6 * time.Minute)
	if settles(m, cfg, relayNow, notRolled) {
		t.Error("an unknown release settles past merge.settle")
	}
}
