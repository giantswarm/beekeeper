package cmd

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/state"
)

// yoursGrant is the PreToolUse hook's record of the supervisor's word: a
// `yours <resource>` (`<resource> yours`, `<resource> is yours`) in a
// message from the session holding the supervisor role grants the resource
// to the message's target, as lease grant records it, so the word and the
// record never drift apart. It returns what the sender is told: each grant
// recorded, or why none was and the command that records it; "" for a
// message naming no resource or from another session.
func (a *app) yoursGrant(session, to, message string) string {
	if a.loadConfig() != nil {
		return ""
	}
	named := guard.Yours(message, a.cfg.Leasable())
	if len(named) == 0 {
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
	a.store, a.now = store, time.Now()
	sessions, _, err := a.sessions()
	if err != nil {
		return ""
	}
	return a.yoursGrants(sessions, sup.Party, to, named)
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
		msg, gerr := a.grant(me, res, to, "")
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
