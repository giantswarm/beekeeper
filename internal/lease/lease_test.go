package lease

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	lab      = "kind-1"
	agentOne = "Agent one"
	// since is the holders' claim time, matching now.
	since = "2026-09-24T20:00:00Z"
	// damagedOne is a resource whose holder record a test damages.
	damagedOne = "damaged"
)

var (
	now  = time.Date(2026, 9, 24, 20, 0, 0, 0, time.UTC)
	ttl  = 30 * time.Minute
	loop = state.Party{Name: "supervisor loop"}
	// damagedRes are the resources whose holder records a test damages.
	damagedRes = []string{damagedOne, "also-damaged"}
	one        = state.Party{Session: "s1", HostSession: "local_1", Name: agentOne}
	two        = state.Party{Session: "s2", HostSession: "local_2", Name: "Agent two"}
	alex       = state.Party{Name: "alex"}
	grant      = func(to state.Party, at time.Time) state.Grant {
		return state.Grant{Resource: lab, To: to, By: loop, At: at}
	}
)

func TestCheckAdmitsOnlyQueueHead(t *testing.T) {
	if idx, err := Check(&state.State{}, Gate{Resource: lab, Caller: one, Now: now, TTL: ttl}); err != nil || idx != -1 {
		t.Fatalf("direct claim: %d, %v", idx, err)
	}
	st := &state.State{Grants: []state.Grant{grant(one, now), grant(two, now)}}
	for _, who := range []state.Party{alex, two} {
		if _, err := Check(st, Gate{Resource: lab, Caller: who, Now: now, TTL: ttl}); err == nil {
			t.Errorf("%s bypassed queue", who.Name)
		}
	}
	if idx, err := Check(st, Gate{Resource: lab, Caller: one, Now: now, TTL: ttl}); err != nil || idx != 0 {
		t.Fatalf("head: %d, %v", idx, err)
	}
	var refusal *Refusal
	_, err := Check(st, Gate{Resource: lab, Caller: two, Now: now, TTL: ttl})
	if !errors.As(err, &refusal) || refusal.Position != 2 || refusal.Ahead != one.Name {
		t.Fatalf("queue refusal lacks position or predecessor: %v", err)
	}
	if idx, err := Check(st, Gate{Resource: "browser", Caller: two, Now: now, TTL: ttl}); err != nil || idx != -1 {
		t.Fatalf("unrelated resource blocked: %d, %v", idx, err)
	}
	st.Grants = st.Grants[1:]
	if idx, err := Check(st, Gate{Resource: lab, Caller: two, Now: now, TTL: ttl}); err != nil || idx != 0 {
		t.Fatalf("successor: %d, %v", idx, err)
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
	h := Holder{Env: "browser", Session: "s1", Name: agentOne, Purpose: "proof", Since: since}
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
	h := Holder{Env: browserRes, Session: "s1", Name: agentOne, Purpose: "a hand-over", Since: since}
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

func TestDirListReportsDamagedHolder(t *testing.T) {
	for _, kind := range []string{"truncated JSON", "directory"} {
		t.Run(kind, func(t *testing.T) {
			d := Dir(t.TempDir())
			healthy := Holder{Env: "healthy", Session: "synthetic-session", Purpose: "test", Since: since}
			if _, err := d.Claim("healthy", healthy); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(d.path("pending"), 0o700); err != nil {
				t.Fatal(err)
			}
			// Multiple failures must all be reported, even before readable entries.
			for _, res := range damagedRes {
				if err := os.Mkdir(d.path(res), 0o700); err != nil {
					t.Fatal(err)
				}
				record := filepath.Join(d.path(res), "holder.json")
				var err error
				if kind == "directory" {
					err = os.Mkdir(record, 0o700)
				} else {
					err = os.WriteFile(record, []byte(`{"env":`), 0o600)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			holders, err := d.List()
			if err != nil {
				t.Fatalf("List failed on damaged holder records: %v", err)
			}
			var got []string
			for _, h := range holders {
				got = append(got, h.Env)
				if !slices.Contains(damagedRes, h.Env) {
					continue
				}
				if h.Damaged == "" || h.Holder != "?" || !strings.Contains(h.Purpose, "release "+h.Env+" --force") {
					t.Errorf("damaged lease %s listed as %+v", h.Env, h)
				}
			}
			// The damaged leases are held, named, next to the readable ones.
			if want := []string{"also-damaged", damagedOne, "pending", "healthy"}; !slices.Equal(got, want) {
				t.Errorf("List = %q, want %q", got, want)
			}
			// Another resource is claimed; a damaged one stays held.
			if cur, err := d.Claim("other", Holder{Env: "other", Since: since}); err != nil || cur != nil {
				t.Errorf("claim of another resource: %+v, %v", cur, err)
			}
			if cur, err := d.Claim(damagedOne, Holder{Env: damagedOne, Since: since}); err != nil || cur == nil || cur.Damaged == "" {
				t.Errorf("claim of a damaged lease: %+v, %v; want its damaged holder", cur, err)
			}
			for _, res := range damagedRes {
				if err := d.Release(res); err != nil {
					t.Fatal(err)
				}
			}
			if h, err := d.Get(damagedOne); err != nil || h != nil {
				t.Errorf("Get after release = %+v, %v; want free", h, err)
			}
		})
	}
}
