package claude

import (
	"os"
	"path/filepath"
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
// does nothing but wait on the session's behalf: a sleep (a bounded wait,
// or the pause of a polling loop), a devctl or gh wait of IsWait (a pull
// request, a release, a CI run), or beekeeper waiting its merge's turn at
// the gate or for a lease (lease … --wait). An image moving between stores
// or a port-forward is IsWait's but not a session's wait.
func onlyWaits(p *proc.Process) bool {
	switch p.Comm {
	case sleepComm:
		return true
	case devctlComm, "gh":
		return IsWait(p)
	case project.Name:
		sub := beekeeperSubcommand(p)
		return sub == "gate" || sub == "lease" && slices.ContainsFunc(p.Args, func(a string) bool {
			return a == "--wait" || strings.HasPrefix(a, "--wait=")
		})
	}
	return false
}

// globalValueFlags are beekeeper's global flags that take a value: the word after
// one is no subcommand.
var globalValueFlags = []string{"--config", "--as"}

// beekeeperSubcommand is beekeeper's subcommand in p's arguments: the first word
// after the program that is no flag nor a global flag's value; "" for none.
func beekeeperSubcommand(p *proc.Process) string {
	for i := 1; i < len(p.Args); i++ {
		a := p.Args[i]
		switch {
		case a == "--":
			return ""
		case slices.Contains(globalValueFlags, a):
			i++
		case !strings.HasPrefix(a, "-"):
			return a
		}
	}
	return ""
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

// LastSubagentWrite is the latest write of the subagents' transcripts of the
// session whose transcript is transcript (<dir>/<id>.jsonl, its subagents'
// <dir>/<id>/subagents/*.jsonl); zero for none.
func LastSubagentWrite(transcript string) time.Time {
	var last time.Time
	if transcript == "" {
		return last
	}
	dir := filepath.Join(strings.TrimSuffix(transcript, filepath.Ext(transcript)), "subagents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return last
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		if fi, err := e.Info(); err == nil && fi.ModTime().After(last) {
			last = fi.ModTime()
		}
	}
	return last
}

// OwnWait says what the session waits on while it does no work of its own:
// it runs at least one tool command, every one of them only waits
// (Command.Wait), and its transcript and its subagents' have been quiet for
// quiet. It is "" for a session that works (a subagent of it included),
// runs nothing (its turn ended, or the model thinks), or whose last activity
// is unknown. A session in a wait of its own (a polling loop, a CI watch, a
// merge's turn) spends next to no budget.
func (s *Session) OwnWait(now time.Time, quiet time.Duration) string {
	last := s.LastActive
	if s.LastSubagent.After(last) {
		last = s.LastSubagent
	}
	if len(s.Commands) == 0 || s.LastActive.IsZero() || now.Sub(last) < quiet {
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
