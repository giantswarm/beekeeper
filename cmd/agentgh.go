package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// errNotAgent leaves the state unwritten: the session is not on the roster.
var errNotAgent = errors.New("not an agent on the roster")

// recordGH records gh, the gh the shell of session p resolves, on p's
// roster entry. A session not on the roster records nothing (errNotAgent).
func recordGH(st *state.State, p state.Party, gh string, now time.Time) error {
	i := slices.IndexFunc(st.Agents, func(x state.Agent) bool { return x.Is(p) })
	if i < 0 {
		return errNotAgent
	}
	if gh == "" {
		gh = ghNone
	}
	st.Agents[i].GH, st.Agents[i].GHAt = gh, now
	return nil
}

// ghNone is the GH of a session whose shell finds no gh at all.
const ghNone = "none"

// recordSessionGH records gh on the roster entry of the session a
// SessionStart event (raw) starts. It never fails the session's start: a
// session not on the roster, or a state it cannot write within
// sessionStartWait, records nothing; the next start or registration reads
// it again.
func (a *app) recordSessionGH(raw []byte, gh string) {
	var ev struct {
		Session string `json:"session_id"`
	}
	if json.Unmarshal(raw, &ev) != nil || ev.Session == "" || a.load() != nil {
		return
	}
	p := state.Party{Session: ev.Session, HostSession: os.Getenv("CLAUDE_CODE_HOST_SESSION_ID")}
	if st, err := a.store.Peek(); err != nil || !slices.ContainsFunc(st.Agents, func(x state.Agent) bool { return x.Is(p) }) {
		return
	}
	_ = a.store.UpdateWithin(sessionStartWait, func(st *state.State) ([]state.Event, error) {
		return nil, recordGH(st, p, gh, a.now.UTC())
	})
}

// sessionStartWait bounds the SessionStart hook's wait for the state lock:
// a session's start never queues behind a slow holder.
const sessionStartWait = 2 * time.Second
