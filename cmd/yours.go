package cmd

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/state"
)

// yoursGrant is the PreToolUse hook's record of the supervisor's word: a
// `yours <resource>` (`<resource> yours`, `<resource> is yours`) in a
// message from the session holding the supervisor role grants the resource
// to the message's target, as lease grant --first records it, so the word
// and the record never drift apart: the granted session claims next. It returns what the sender is told: each grant
// recorded, or why none was and the command that records it. The same
// message to an agent that reported its task idle re-tasks it (retask):
// the sender is told its task record is back. "" for a message from
// another session, or one that names no resource and re-tasks nobody.
func (a *app) yoursGrant(session, to, message string) string {
	if a.loadConfig() != nil {
		return ""
	}
	store, err := state.Open(a.cfg.StateDir)
	if err != nil {
		return ""
	}
	st, err := store.Peek()
	if err != nil {
		return ""
	}
	sup := st.Supervisor
	if sup == nil || !sup.Is(state.Party{Session: session, HostSession: os.Getenv("CLAUDE_CODE_HOST_SESSION_ID")}) {
		return ""
	}
	named := guard.Yours(message, a.cfg.Leasable())
	if len(named) == 0 && !slices.ContainsFunc(st.Agents, retaskable) {
		return ""
	}
	a.store, a.now = store, time.Now()
	sessions, _, err := a.sessions()
	if err != nil {
		return ""
	}
	var lines []string
	if line, err := a.retaskAgent(sup.Party, messagedAgent(sessions, to, liveSocket), message); err == nil && line != "" {
		lines = append(lines, "beekeeper: "+line)
	}
	if len(named) > 0 {
		lines = append(lines, a.yoursGrants(sessions, sup.Party, to, named))
	}
	return strings.Join(lines, "\n")
}

// yoursGrants records me's grants of resources to the session q, one line
// per resource: what lease grant says, or why nothing was recorded and the
// command that records it.
func (a *app) yoursGrants(sessions []*claude.Session, me state.Party, q string, resources []string) string {
	to, absent, err := a.grantee(sessions, q)
	lines := make([]string, 0, len(resources))
	for _, res := range resources {
		if err != nil {
			lines = append(lines, fmt.Sprintf("your `yours %s` recorded no grant: %v; its claim is refused until `beekeeper lease grant %s <session>` records one", res, err, res))
			continue
		}
		msg, gerr := a.grant(me, res, to, "", true)
		if gerr != nil {
			lines = append(lines, fmt.Sprintf("your `yours %s` recorded no grant: %v; its claim is refused until `beekeeper lease grant %s %q` records one", res, gerr, res, to.Name))
			continue
		}
		if absent != "" {
			msg += "; " + absent
		}
		lines = append(lines, fmt.Sprintf("your `yours %s` is recorded: %s", res, msg))
	}
	return "beekeeper: " + strings.Join(lines, "\n")
}
