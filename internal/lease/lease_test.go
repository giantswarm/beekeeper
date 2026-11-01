package lease

import (
	"errors"
	"slices"
	"strings"
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
	loop  = state.Party{Name: "supervisor loop"}
	one   = state.Party{Session: "s1", HostSession: "local_1", Name: agentOne}
	two   = state.Party{Session: "s2", HostSession: "local_2", Name: "Agent two"}
	alex  = state.Party{Name: "alex"}
	grant = func(to state.Party, at time.Time) state.Grant {
		return state.Grant{Resource: lab, To: to, By: loop, At: at}
	}
)

// No supervisor session gates a claim: a free lease is the caller's, even
// with grants of it to others queued.
func TestCheckAdmitsAFreeLease(t *testing.T) {
	idx, err := Check(&state.State{}, Gate{Resource: lab, Caller: one, Now: now, TTL: ttl})
	if err != nil || idx != -1 {
		t.Fatalf("Check = %d, %v", idx, err)
	}
	st := &state.State{Grants: []state.Grant{grant(one, now.Add(-5*time.Minute))}}
	for _, who := range []state.Party{two, alex} {
		if idx, err := Check(st, Gate{Resource: lab, Caller: who, Now: now, TTL: ttl}); err != nil || idx != -1 {
			t.Errorf("%s behind a grant to %s: %d, %v", who.Name, one.Name, idx, err)
		}
	}
}

func TestCheckRefusesDuringUpgrade(t *testing.T) {
	st := &state.State{Holds: []state.Hold{{Target: "upgrade:prod/mc", Reason: "upgrade prod/mc 1.0.0 → 1.1.0", At: now}}}
	for _, who := range []state.Party{alex, loop} {
		var r *Refusal
		if _, err := Check(st, Gate{Resource: "prod", Caller: who, Now: now, TTL: ttl}); !errors.As(err, &r) {
			t.Errorf("%s claims prod while it upgrades: %v", who.Name, err)
		}
	}
	if _, err := Check(st, Gate{Resource: lab, Caller: alex, Now: now, TTL: ttl}); err != nil {
		t.Errorf("an upgrade on prod refuses %s: %v", lab, err)
	}
	st.Holds = nil
	if _, err := Check(st, Gate{Resource: "prod", Caller: alex, Now: now, TTL: ttl}); err != nil {
		t.Errorf("prod stays refused after the upgrade: %v", err)
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

func TestCheckAdmitsUpgradeUnblockGrants(t *testing.T) {
	const prod = "prod"
	unblock := func(to state.Party, at time.Time) state.Grant {
		return state.Grant{Resource: prod, To: to, By: loop, At: at, UpgradeUnblock: "delete the pod a PDB keeps the drain on"}
	}
	st := &state.State{
		Holds:  []state.Hold{{Target: "upgrade:prod/mc", Reason: "upgrade prod/mc 1.0.0 → 1.1.0", At: now}},
		Grants: []state.Grant{{Resource: prod, To: one, By: loop, At: now.Add(-2 * time.Minute)}, unblock(two, now.Add(-time.Minute)), unblock(one, now)},
	}
	gate := func(who state.Party) Gate {
		return Gate{Resource: prod, Caller: who, Now: now, TTL: ttl}
	}

	// The first upgrade-unblock grant carries the claim; a plain grant
	// earlier in the queue does not hold it back.
	if idx, err := Check(st, gate(two)); err != nil || idx != 1 {
		t.Errorf("the first unblock grant: %d, %v", idx, err)
	}
	// Every other claim stays refused: behind it in the unblock queue, the
	// supervisor loop, a person, a session with no grant.
	for _, who := range []state.Party{one, loop, alex, {Session: "s9", Name: "Agent nine"}} {
		var r *Refusal
		if _, err := Check(st, gate(who)); !errors.As(err, &r) {
			t.Errorf("%s claims prod while it upgrades: %v", who.Name, err)
		}
	}
	// A plain grant alone admits nothing during the upgrade.
	st.Grants = st.Grants[:1]
	var r *Refusal
	if _, err := Check(st, gate(one)); !errors.As(err, &r) || !strings.Contains(r.Reason, "--upgrade-unblock") {
		t.Errorf("a plain grant during the upgrade: %v", err)
	}
}

// browserRes is the resource the hand test hands.
const browserRes = "browser"

func TestDirHand(t *testing.T) {
	d := Dir(t.TempDir())
	h := Holder{Env: browserRes, Session: "s1", Name: agentOne, Purpose: "a hand-over", Since: "2026-09-24T20:00:00Z"}
	if cur, err := d.Claim(browserRes, h); err != nil || cur != nil {
		t.Fatalf("Claim = %v, %v", cur, err)
	}
	h.Session = "s2"
	if err := d.Hand(h); err != nil {
		t.Fatal(err)
	}
	cur, err := d.Get(browserRes)
	if err != nil || cur == nil || cur.Session != "s2" || cur.Purpose != "a hand-over" {
		t.Fatalf("Get = %+v, %v", cur, err)
	}
	if list, err := d.List(); err != nil || len(list) != 1 {
		t.Fatalf("List = %+v, %v", list, err)
	}
}

// ToHead moves a grant before the other grants of its resource, keeps their
// order and leaves other resources' grants in place.
func TestToHead(t *testing.T) {
	now := time.Date(2026, 10, 17, 2, 0, 0, 0, time.UTC)
	g := func(res, name string) state.Grant {
		return state.Grant{Resource: res, To: state.Party{Session: name, Name: name}, At: now.Add(-time.Hour)}
	}
	st := &state.State{Grants: []state.Grant{g("x", "o"), g("r", "a"), g("r", "b"), g("x", "p"), g("r", "c")}}
	if j := ToHead(st, 4, now); j != 1 {
		t.Errorf("index %d, want 1", j)
	}
	var got []string
	for _, x := range st.Grants {
		got = append(got, x.Resource+x.To.Name)
	}
	if want := []string{"xo", "rc", "ra", "rb", "xp"}; !slices.Equal(got, want) {
		t.Errorf("grants %q, want %q", got, want)
	}
	if !st.Grants[1].At.Equal(now) {
		t.Errorf("the moved grant's time %v, want %v", st.Grants[1].At, now)
	}
}
