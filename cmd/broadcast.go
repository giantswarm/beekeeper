package cmd

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

// broadcastName is the agents subcommand that messages every running agent.
const broadcastName = "broadcast"

func (a *app) agentBroadcastCmd() *cobra.Command {
	return &cobra.Command{
		Use:   broadcastName + " <message>",
		Short: "Message every registered agent whose CLI runs, by name, one after the other",
		Long: `broadcast sends one message by name to every registered agent whose CLI
runs, the supervisor and the caller left out, as SendMessage by name does:
it runs at the agent's next tool call or as its next turn. A stopped agent
is not resumed for it. One agents.broadcast event logs whom it reached.
` + "`beekeeper supervisor start`" + ` runs it when a new holder takes the role, so
every agent learns the name to report to.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			msg := strings.TrimSpace(args[0])
			if msg == "" {
				return usageErr("an empty message tells nobody")
			}
			by, err := a.caller()
			if err != nil {
				return err
			}
			st, err := a.store.Read()
			if err != nil {
				return err
			}
			sessions, _, err := a.sessions()
			if err != nil {
				return err
			}
			skip := []state.Party{by}
			if st.Supervisor != nil {
				skip = append(skip, st.Supervisor.Party)
			}
			told, missed := broadcast(cmd.Context(), st.Agents, sessions, skip, func(ctx context.Context, name string) error {
				return a.peerSend(ctx, name, msg)
			})
			for _, m := range missed {
				_, _ = fmt.Fprintln(a.out, "not reached: "+m)
			}
			line := fmt.Sprintf("told %d of %d agents", len(told), len(told)+len(missed))
			if len(told) > 0 {
				line += ": " + strings.Join(told, ", ")
			}
			_ = a.store.Log(event(by, "agents."+broadcastName, "%s: %s", line, truncate(msg, 100)))
			_, err = fmt.Fprintln(a.out, line)
			return err
		},
	}
}

// broadcast sends to every agent whose CLI runs, skip left out, one after
// the other: told names those reached, missed says why each other running
// one was not. A stopped agent is neither.
func broadcast(ctx context.Context, agents []state.Agent, sessions []*claude.Session, skip []state.Party, send func(context.Context, string) error) (told, missed []string) {
	for _, ag := range agents {
		if skipped(ag.Party, skip) {
			continue
		}
		s, ok := wakeLive(sessions, ag.Party, ag.Session)
		if !ok {
			continue
		}
		name, err := uniqueName(sessions, s)
		if err == nil {
			err = send(ctx, name)
		}
		if err != nil {
			missed = append(missed, fmt.Sprintf("%s: %v", ag.Name, err))
			continue
		}
		told = append(told, ag.Name)
	}
	return told, missed
}

func skipped(p state.Party, skip []state.Party) bool {
	for _, s := range skip {
		if p.Is(s) || p.Name != "" && strings.EqualFold(p.Name, s.Name) {
			return true
		}
	}
	return false
}

// unitChars are what a unit name of a role's run keeps.
var unitChars = regexp.MustCompile(`[^a-z0-9]+`)

// announce has a transient unit tell every running agent that name answers
// now; the start does not wait for the sends. The line says where it runs,
// or what to run where no unit can start.
func (a *app) announce(name string) string {
	msg := fmt.Sprintf("%q answers now as the supervisor: report to it and ask it by that name (`beekeeper supervisor status` names the holder).", name)
	by := fmt.Sprintf("run `beekeeper --as %q agents %s %q` to tell the agents", name, broadcastName, msg)
	if !plat.Launcher.Available() {
		return "no unit can start here: " + by
	}
	self, err := os.Executable()
	if err == nil {
		unit := "beekeeper-broadcast-" + strings.Trim(unitChars.ReplaceAllString(strings.ToLower(name), "-"), "-") + "-" + a.now.Format("150405")
		if err = launch(unit, a.cfg.StateDir, a.explicitConfig(), nil, []string{self, "--as", name, agentsName, broadcastName, msg}); err == nil {
			return fmt.Sprintf("telling every running agent that %q answers now (journalctl --user -u %s; beekeeper log --verb agents.%s)", name, unit, broadcastName)
		}
	}
	return fmt.Sprintf("the broadcast did not start (%v): %s", err, by)
}
