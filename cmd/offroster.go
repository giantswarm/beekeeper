package cmd

import (
	"cmp"
	"errors"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/giantswarm/beekeeper/internal/state"
)

// errGone marks a wake of a session that no longer exists: no transcript to
// resume, no harness process reading its inbox. Nothing retries it.
var errGone = errors.New("its session is gone")

// goneError is a refusal of a wake whose session is gone (errGone).
type goneError struct{ err error }

func (g goneError) Error() string               { return g.err.Error() }
func (g goneError) Unwrap() error               { return g.err }
func (goneError) Is(target error) bool          { return target == errGone }
func goneRefusal(format string, a ...any) error { return goneError{refused(format, a...)} }

// wakeParty is the session a wake of q reaches: the roster agent q names
// (true), else a session off the roster (offRosterParty), so a wait the desk
// settles wakes its waiter whether or not it registered. The error is the
// roster's when q names neither.
func (a *app) wakeParty(st *state.State, q string) (state.Agent, bool, error) {
	i, err := findAgent(st, q)
	if err == nil {
		return st.Agents[i], true, nil
	}
	p, ok := offRosterParty(st, q)
	if !ok {
		return state.Agent{}, false, err
	}
	return state.Agent{Party: p}, false, nil
}

// offRosterParty is the session off the roster q addresses: a role's holder
// by its name or ids, a session parked off the
// roster, or a session by its own id (a CLI session id, a desktop local_ id,
// a harness's omp_/agy_/codex_ id), which the caller of a gated wait or a
// park recorded as its owner.
func offRosterParty(st *state.State, q string) (state.Party, bool) {
	q = strings.TrimSpace(q)
	if q == "" {
		return state.Party{}, false
	}
	for _, rl := range roles {
		if h := rl.get(st).Holder; h != nil && (strings.EqualFold(h.Name, q) || h.Session == q || h.HostSession == q) {
			return h.Party, true
		}
	}
	for _, p := range st.Parks {
		if p.By.Session == q || p.By.HostSession == q {
			return p.By, true
		}
	}
	if strings.HasPrefix(q, claudeHarness{}.HostPrefix()) {
		return state.Party{HostSession: q, Name: q}, true
	}
	if _, _, ok := harnessForHost(q); ok {
		return state.Party{HostSession: q, Name: q}, true
	}
	if _, err := uuid.Parse(q); err == nil {
		return state.Party{Session: q, Name: q}, true
	}
	return state.Party{}, false
}

// wakeID is the id a wake of p resolves p by: its CLI session id, else its
// desktop or harness id.
func wakeID(p state.Party) string { return cmp.Or(p.Session, p.HostSession) }

// onRoster says whether p is a roster agent.
func onRoster(st *state.State, p state.Party) bool {
	return slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(p) })
}
