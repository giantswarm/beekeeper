package claude

import (
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// The process names of the tools a wait runs.
const (
	devctlComm = "devctl"
	sleepComm  = "sleep"
)

// IsWait reports whether p is a long-running command that blocks on
// something outside the session: a merge, a release, a CI run, an image
// moving between stores, a port-forward.
func IsWait(p *proc.Process) bool {
	has := argsHave(p.Args)
	switch p.Comm {
	case devctlComm:
		return has("pr", "wait") || has("pr", "merge") || has("release", "wait")
	case "gh":
		return has("--watch") || has("run", "watch")
	case "ctr":
		return has("import")
	case "docker":
		return len(p.Args) > 1 && (p.Args[1] == "save" || p.Args[1] == "load" || has("image", "save") || has("image", "load"))
	case "kubectl":
		return has("port-forward")
	}
	return false
}

// onlyWaits reports whether p, a tool command's first non-shell process,
// does nothing but wait: IsWait, a sleep (a bounded wait, or the pause of a
// polling loop), or beekeeper waiting its merge's turn at the gate or for a
// lease.
func onlyWaits(p *proc.Process) bool {
	has := argsHave(p.Args)
	switch p.Comm {
	case sleepComm:
		return true
	case project.Name:
		return has("gate") || has("lease", "--wait")
	}
	return IsWait(p)
}

// argsHave reports whether args hold every one of words.
func argsHave(args []string) func(words ...string) bool {
	return func(words ...string) bool {
		for _, w := range words {
			if !slices.Contains(args, w) {
				return false
			}
		}
		return true
	}
}

// OwnWait says what the session waits on while it does no work of its own:
// it runs at least one tool command, every one of them only waits
// (Command.Wait), and its transcript has been quiet for quiet. It is "" for
// a session that works, runs nothing (its turn ended, or the model thinks),
// or whose last activity is unknown. A session in a wait of its own (a
// polling loop, a CI watch, a merge's turn) spends next to no budget.
func (s *Session) OwnWait(now time.Time, quiet time.Duration) string {
	if len(s.Commands) == 0 || s.LastActive.IsZero() || now.Sub(s.LastActive) < quiet {
		return ""
	}
	if slices.ContainsFunc(s.Commands, func(c Command) bool { return !c.Wait }) {
		return ""
	}
	c := slices.MaxFunc(s.Commands, func(x, y Command) int { return int(x.Remaining - y.Remaining) })
	w := c.Args
	if len(w) > 60 {
		w = strings.TrimSpace(w[:57]) + "…"
	}
	if c.Remaining > 0 {
		w += " (" + c.Remaining.String() + " left)"
	}
	return w
}
