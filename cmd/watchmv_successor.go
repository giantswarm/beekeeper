package cmd

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// runPattern matches a role run's name.
var runPattern = regexp.MustCompile(`^(\S+) run (\d+)$`)

// runName is the name of rl's run n.
func (rl role) runName(n int) string { return fmt.Sprintf("%s run %d", rl.title, n) }

// runOf is the run of rl that name names, 0 when it names none.
func (rl role) runOf(name string) int {
	m := runPattern.FindStringSubmatch(strings.TrimSpace(name))
	if m == nil || m[1] != rl.title {
		return 0
	}
	n, _ := strconv.Atoi(m[2])
	return n
}

// lastRun is the highest run of rl that r records: its Run, and the names
// of its holder, its relay and the holders it relieved.
func (rl role) lastRun(r state.Role) int {
	n := r.Run
	see := func(p state.Party) { n = max(n, rl.runOf(p.Name)) }
	if r.Holder != nil {
		see(r.Holder.Party)
	}
	if r.Relay != nil {
		see(r.Relay.From)
		see(r.Relay.To)
	}
	for _, rf := range r.Relieved {
		see(rf.Party)
		see(rf.By)
	}
	return n
}

// successorBrief is the first prompt of name, rl's next run after from.
// Its first turn runs headless and only takes the role: a headless turn
// that arms a watch never ends, so the desktop never gets the session's
// CLI and a click on its row would start a second one. The standby watch
// resumes it in its desktop CLI. It opens with the role skill's slash
// command (roleSlash), which loads the skill into the session for good.
func (rl role) successorBrief(cfg config.Role, name, from string) string {
	return fmt.Sprintf(`%sYou are %q, the next %s run, a fresh session beekeeper started to take over %s%s.

This first turn runs headless from the command line. In it, only take the role: run `+"`beekeeper %s start`"+`, then end the turn. Arm no Monitor, watch or background task in it: the desktop gets this session's CLI only once this turn has ended.

Once the desktop runs your CLI, a message tells you so there: then run `+"`%s`"+` and follow it. Peers message you as %q.`,
		roleSlash(cfg), name, rl.name, rl.duty, fromText(from), rl.name, rl.handover, name)
}

// fromText names the holder a run takes over from, "" when none is
// recorded.
func fromText(from string) string {
	if from == "" {
		return ", which no session holds"
	}
	return fmt.Sprintf(" from %q", from)
}

// backgroundSuccessorBrief is successorBrief for a background session
// (agents.mode background): its turns all run in the one CLI the claude
// daemon keeps, so its first turn takes the role and goes on with it.
func (rl role) backgroundSuccessorBrief(cfg config.Role, name, from string) string {
	return fmt.Sprintf(`%sYou are %q, the next %s run, a fresh background session beekeeper started to take over %s%s.

Take the role: run `+"`beekeeper %s start`"+`, then run `+"`%s`"+` and follow it. Peers message you as %q.`,
		roleSlash(cfg), name, rl.name, rl.duty, fromText(from), rl.name, rl.handover, name)
}
