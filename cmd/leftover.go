package cmd

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// leftoverGrace is how long a stopped leftover command has after SIGTERM
// before it is killed.
const leftoverGrace = 2 * time.Second

// stopProcesses ends the processes pids; tests replace it.
var stopProcesses = func(pids []int) { proc.Terminate(pids, leftoverGrace) }

// stopBackground stops the tool shells of the session whose tool command
// runs the process self, but the one self runs under, with every process
// below them: what the session's turns left running in the background. It
// returns the commands it stopped; none when the session's CLI is not in t
// (a sandbox's own process namespace hides it).
func stopBackground(t *proc.Table, self int, now time.Time) []claude.Command {
	cli := claude.OwnCLI(t, self)
	if cli == nil {
		return nil
	}
	var pids []int
	var cmds []claude.Command
	for _, sh := range claude.ToolShells(t, cli.PID, now) {
		if slices.Contains(sh.Tree, self) {
			continue
		}
		pids = append(pids, sh.Tree...)
		cmds = append(cmds, sh.Commands...)
	}
	if len(pids) > 0 {
		stopProcesses(pids)
	}
	return cmds
}

// stopOwnBackground stops what the calling session left running in the
// background and says so; nothing for a call --as another agent or of an
// omp agent, whose CLI is not the caller's.
func (a *app) stopOwnBackground() error {
	if a.as != "" || os.Getenv(omp.EnvAgent) != "" {
		return nil
	}
	t, err := plat.Machine.Processes()
	if err != nil {
		_, err = fmt.Fprintf(a.out, "register: its background commands stay unchecked: %v\n", err)
		return err
	}
	cmds := stopBackground(t, os.Getpid(), a.now)
	if len(cmds) == 0 {
		return nil
	}
	lines := make([]string, len(cmds))
	for i, c := range cmds {
		lines[i] = fmt.Sprintf("%d %s", c.PID, truncate(c.Args, 60))
	}
	_, err = fmt.Fprintf(a.out, "register: stopped %d background command(s) the session left running: %s\n", len(cmds), strings.Join(lines, ", "))
	return err
}

// leftWait is a finished agent whose CLI runs no turn but sleeping waits:
// the tool shells a background wait loop of its left behind.
type leftWait struct {
	agent  state.Party
	shells []claude.ToolShell
}

func (w leftWait) String() string {
	var cmds []string
	for _, sh := range w.shells {
		for _, c := range sh.Commands {
			cmds = append(cmds, fmt.Sprintf("%d %s", c.PID, truncate(c.Args, 60)))
		}
	}
	return fmt.Sprintf("the sleeping wait(s) %q left behind after it reported its work done: %s", w.agent.Name, strings.Join(cmds, ", "))
}

// pids are the processes of the wait's shells.
func (w leftWait) pids() []int {
	var out []int
	for _, sh := range w.shells {
		out = append(out, sh.Tree...)
	}
	return out
}

// sleepingWaits finds the finished agents (agents idle --done) whose CLI
// runs no turn but tool shells that only sleep: no headless turn, no gated
// merge of theirs (inFlight), no transcript written within stewardQuiet, and
// every command of every tool shell a `sleep`.
func sleepingWaits(st *state.State, sessions []*claude.Session, t *proc.Table, inFlight func(state.Party) bool, now time.Time) []leftWait {
	var out []leftWait
	for _, ag := range st.Agents {
		if !ag.Done || ag.Task != "" || inFlight(ag.Party) {
			continue
		}
		for _, s := range sessions {
			if s.Harness != "" || !ag.Is(s.Party()) || headlessTurn(t, s.ID) != "" || now.Sub(s.LastActive) < stewardQuiet {
				continue
			}
			shells := claude.ToolShells(t, s.PID, now)
			if len(shells) > 0 && !slices.ContainsFunc(shells, func(sh claude.ToolShell) bool { return !sh.Sleeping() }) {
				out = append(out, leftWait{agent: ag.Party, shells: shells})
			}
		}
	}
	return out
}
