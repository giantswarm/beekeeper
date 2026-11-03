package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A hand-over's predecessor and its follow-up, both under the agent's name.
var (
	handedFrom = state.Party{Session: "s-old", HostSession: "local_s-old", Name: agentOne}
	handedTo   = state.Party{Session: "s-new", HostSession: "local_s-new", Name: agentOne}
)

// handClaims gives the follow-up the leases its predecessor holds and the
// grants queued for it, in their places, and leaves everyone else's.
func TestHandClaimsGivesTheFollowUpItsLeasesAndGrants(t *testing.T) {
	dir := lease.Dir(t.TempDir())
	other := state.Party{Session: "s-other", Name: agentTwo}
	for _, h := range []lease.Holder{
		{Env: browser, Session: handedFrom.Session, HostSession: handedFrom.HostSession, Name: agentOne, Purpose: theProof, Since: "2026-10-14T20:00:00Z"},
		{Env: uiLab1, Session: other.Session, Name: other.Name, Purpose: "its own", Since: "2026-10-14T20:01:00Z"},
	} {
		if cur, err := dir.Claim(h.Env, h); err != nil || cur != nil {
			t.Fatalf("Claim %s = %v, %v", h.Env, cur, err)
		}
	}
	st := &state.State{Grants: []state.Grant{
		{Resource: uiLab1, To: other},
		{Resource: uiLab1, To: handedFrom},
		{Resource: uiLab2, To: handedFrom},
	}}
	handed, err := handClaims(st, dir, handedFrom, handedTo)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(handed, ", "), "browser held, kind-1 granted, kind-2 granted"; got != want {
		t.Errorf("handed %q, want %q", got, want)
	}
	cur, err := dir.Get(browser)
	if err != nil || cur == nil || !cur.Party().Is(handedTo) || cur.Purpose != theProof || cur.Since != "2026-10-14T20:00:00Z" {
		t.Fatalf("browser is held by %+v (%v), want the follow-up with the claim's purpose and time", cur, err)
	}
	if cur, _ := dir.Get(uiLab1); cur == nil || !cur.Party().Is(other) {
		t.Errorf("another holder's lease moved: %+v", cur)
	}
	if !st.Grants[0].To.Is(other) || !st.Grants[1].To.Is(handedTo) || !st.Grants[2].To.Is(handedTo) {
		t.Errorf("grants %+v: the predecessor's go to the follow-up in their places", st.Grants)
	}
	if list, _ := dir.List(); len(list) != 2 {
		t.Errorf("the lease directory lists %+v", list)
	}
}

// While a hand-over's follow-up and its predecessor both run under the
// agent's name, a grant by that name goes to the follow-up, the roster's
// agent, instead of failing as ambiguous.
func TestAGrantByAHandedOverNameGoesToTheFollowUp(t *testing.T) {
	var out bytes.Buffer
	a := quietApp(t, &out)
	a.cfg.Resources = []string{uiLab1}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = append(st.Agents, state.Agent{Party: handedTo, Registered: a.now, Task: theProof})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	sessions := []*claude.Session{
		{PID: 7, ID: handedFrom.Session, HostID: handedFrom.HostSession, Name: agentOne},
		{PID: 8, ID: handedTo.Session, HostID: handedTo.HostSession, Name: agentOne},
	}
	to, _, err := a.grantee(sessions, agentOne)
	if err != nil || to == nil || to.ID != handedTo.Session {
		t.Fatalf("grantee by the shared name: %+v, %v; want session %s", to, err, handedTo.Session)
	}
	// Two sessions of that name neither of which is on the roster stay
	// ambiguous.
	if _, _, err := sessionNamed(sessions, &state.State{}, agentOne, a.now); err == nil || !strings.Contains(err.Error(), "matches 2 sessions") {
		t.Errorf("two unregistered sessions of one name: %v", err)
	}
}

// A message by a name several running CLIs carry goes to the roster
// agent's own CLI through its socket; a name one CLI carries, or no agent
// does, stays as written; an agent whose CLI does not run is refused.
func TestSharedNameAddress(t *testing.T) {
	now := time.Date(2026, 10, 14, 22, 30, 0, 0, time.UTC)
	sock := func(pid int) string {
		if pid == 8 {
			return "/run/user/1000/cc-socks/8.sock"
		}
		return ""
	}
	st := &state.State{Agents: []state.Agent{{Party: handedTo, Task: theProof}}}
	pred := &claude.Session{PID: 7, ID: handedFrom.Session, HostID: handedFrom.HostSession, Name: agentOne}
	succ := &claude.Session{PID: 8, ID: handedTo.Session, HostID: handedTo.HostSession, Name: agentOne}
	if got, err := sharedNameAddress(st, []*claude.Session{pred, succ}, "agent ONE", now, sock); err != nil || got != "uds:/run/user/1000/cc-socks/8.sock" {
		t.Errorf("both run: %q, %v", got, err)
	}
	if got, err := sharedNameAddress(st, []*claude.Session{succ}, agentOne, now, sock); err != nil || got != "" {
		t.Errorf("the follow-up alone: %q, %v", got, err)
	}
	if got, err := sharedNameAddress(st, []*claude.Session{pred, succ}, agentTwo, now, sock); err != nil || got != "" {
		t.Errorf("no agent of that name: %q, %v", got, err)
	}
	if _, err := sharedNameAddress(st, []*claude.Session{pred}, agentOne, now, sock); err == nil || !strings.Contains(err.Error(), "would reach session s-old") {
		t.Errorf("the follow-up's CLI does not run: %v", err)
	}
	if _, err := sharedNameAddress(st, []*claude.Session{pred, succ}, agentOne, now, noSocket); err == nil || !strings.Contains(err.Error(), "no peer socket") {
		t.Errorf("no socket: %v", err)
	}
}

// theProof is the purpose and task of the hand-over tests' agent.
const theProof = "the proof"
