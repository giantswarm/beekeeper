package lease

import (
	"errors"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	lab      = "kind-1"
	agentOne = "Agent one"
)

var (
	now   = time.Date(2026, 9, 24, 20, 0, 0, 0, time.UTC)
	ttl   = 30 * time.Minute
	sup   = &state.Supervisor{Party: state.Party{Session: "s0", Name: "Supervisor run 11"}}
	one   = state.Party{Session: "s1", HostSession: "local_1", Name: agentOne}
	two   = state.Party{Session: "s2", HostSession: "local_2", Name: "Agent two"}
	alex  = state.Party{Name: "alex"}
	grant = func(to state.Party, at time.Time) state.Grant {
		return state.Grant{Resource: lab, To: to, By: sup.Party, At: at}
	}
)

func TestCheckWithoutSupervisor(t *testing.T) {
	idx, err := Check(&state.State{}, Gate{Resource: lab, Caller: one, Now: now, TTL: ttl})
	if err != nil || idx != -1 {
		t.Fatalf("Check = %d, %v", idx, err)
	}
}

func TestCheckNeedsGrantUnderSupervisor(t *testing.T) {
	_, err := Check(&state.State{}, Gate{Resource: lab, Caller: one, Supervisor: sup, Now: now, TTL: ttl})
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("want a refusal, got %v", err)
	}
	// The supervisor itself and a person are not gated.
	for _, p := range []state.Party{sup.Party, alex} {
		if _, err := Check(&state.State{}, Gate{Resource: lab, Caller: p, Supervisor: sup, Now: now, TTL: ttl}); err != nil {
			t.Errorf("%s: %v", p.Name, err)
		}
	}
}

func TestCheckQueueOrder(t *testing.T) {
	st := &state.State{Grants: []state.Grant{grant(one, now.Add(-5*time.Minute)), grant(two, now.Add(-4*time.Minute))}}
	g := Gate{Resource: lab, Supervisor: sup, Now: now, TTL: ttl}
	g.Caller = two
	if _, err := Check(st, g); err == nil {
		t.Fatal("second in the queue claimed first")
	}
	g.Caller = one
	idx, err := Check(st, g)
	if err != nil || idx != 0 {
		t.Fatalf("first in the queue: %d, %v", idx, err)
	}
	// A restarted CLI keeps its host session: the grant still matches.
	g.Caller = state.Party{Session: "s1-restarted", HostSession: "local_1"}
	if _, err := Check(st, g); err != nil {
		t.Fatalf("restarted session: %v", err)
	}
}

func TestGrantExpiry(t *testing.T) {
	old := grant(one, now.Add(-time.Hour))
	st := &state.State{Grants: []state.Grant{old}}
	if p := Pending(st, lab, false, now, ttl); len(p) != 0 {
		t.Errorf("an hour-old grant on a free resource is pending: %v", p)
	}
	if p := Pending(st, lab, true, now, ttl); len(p) != 1 {
		t.Errorf("a grant waiting for a held resource expired")
	}
	// The TTL runs from the release when the resource was freed after the grant.
	st.Released = map[string]time.Time{lab: now.Add(-10 * time.Minute)}
	if p := Pending(st, lab, false, now, ttl); len(p) != 1 {
		t.Errorf("a grant freed 10 minutes ago expired")
	}
	Prune(st, map[string]bool{}, now.Add(time.Hour), ttl)
	if len(st.Grants) != 0 {
		t.Errorf("Prune kept %v", st.Grants)
	}
}

func TestDirClaimRelease(t *testing.T) {
	d := Dir(t.TempDir())
	h := Holder{Env: "browser", Session: "s1", Name: agentOne, Purpose: "proof", Since: "2026-09-24T20:00:00Z"}
	if cur, err := d.Claim("browser", h); err != nil || cur != nil {
		t.Fatalf("Claim = %v, %v", cur, err)
	}
	cur, err := d.Claim("browser", Holder{Env: "browser", Session: "s2"})
	if err != nil || cur == nil || cur.Session != "s1" {
		t.Fatalf("second Claim = %+v, %v", cur, err)
	}
	list, err := d.List()
	if err != nil || len(list) != 1 || list[0].Name != agentOne {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if err := d.Release("browser"); err != nil {
		t.Fatal(err)
	}
	if cur, _ := d.Get("browser"); cur != nil {
		t.Fatalf("released lease still held: %+v", cur)
	}
}
