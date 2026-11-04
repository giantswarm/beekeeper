package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

// broadcastName is the agents subcommand that messages live roster agents.
const broadcastName = "broadcast"

func (a *app) agentBroadcastCmd() *cobra.Command {
	return &cobra.Command{
		Use:   broadcastName + " <message>",
		Short: "Message live roster agents, by name, one after the other",
		Long: `broadcast sends one message by name to live roster agents whose CLI runs,
the caller left out, as SendMessage by name does. Relieved role runs,
parked agents and agents whose work is done receive nothing.
The message runs at the agent's next tool call or as its next turn. A stopped agent
is not resumed for it. One agents.broadcast event logs whom it reached.`,
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
			told, missed := broadcast(cmd.Context(), st, sessions, skip, func(ctx context.Context, name string) error {
				return a.peerSend(ctx, name, msg)
			})
			for _, m := range missed {
				_, _ = fmt.Fprintln(a.out, "not reached: "+m)
			}
			line := fmt.Sprintf("told %d of %d agents", len(told), len(told)+len(missed))
			if len(told) > 0 {
				line += ": " + strings.Join(told, ", ")
			}
			_ = a.store.Record(event(by, "agents."+broadcastName, "%s: %s", line, truncate(msg, 100)))
			_, err = fmt.Fprintln(a.out, line)
			return err
		},
	}
}

// broadcast sends to live roster agents whose CLI runs, skip left out, one
// after the other. Relieved, parked, done and stopped agents are neither
// told nor missed.
func broadcast(ctx context.Context, st *state.State, sessions []*claude.Session, skip []state.Party, send func(context.Context, string) error) (told, missed []string) {
	for _, ag := range st.Agents {
		if skipped(ag.Party, skip) || relieved(st, ag.Party) || ag.Park != nil || ag.Done {
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
