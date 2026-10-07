package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/guard"
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

// ghKey starts the condition key of an agent whose gh is unbrokered.
const ghKey = "gh "

// unbrokered says each agent on the roster whose shell resolves a gh other
// than the agent's own (agents.shell.path, the gh link to devctl): its gh
// acts on the person's own login, or fails, instead of the App's token. One
// GH UNBROKERED line each, and its ENDED line once the agent's gh is its own
// or it left the roster.
func (w *watcher) unbrokered() {
	path := w.cfg.Agents.Shell.Path
	if len(path) == 0 {
		return
	}
	st, err := w.store.Read()
	if err != nil {
		return
	}
	found := map[string]bool{}
	for _, ag := range st.Agents {
		if ag.GH == "" || guard.Brokered(ag.GH, path) {
			continue
		}
		key := ghKey + ag.Name
		found[key] = true
		w.emit(key, "GH UNBROKERED %q: its shell resolves gh to %s (read %s), not the agent's own in %s (agents.shell.path): its gh acts without the App's token",
			ag.Name, ag.GH, clock(w.now, ag.GHAt), strings.Join(path, ", "))
	}
	w.clearMissing(ghKey, found)
}

// recordSessionGH records gh on the roster entry of the session a
// SessionStart event (raw) starts. It never fails the session's start: a
// session not on the roster, or a state it cannot write, records nothing.
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
	_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
		return nil, recordGH(st, p, gh, a.now.UTC())
	})
}
