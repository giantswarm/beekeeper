package cmd

import (
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/agy"
)

// agyRunCmdName is the hidden command an agy agent's user unit runs to drive
// `agy --input-format stream-json --output-format stream-json` from its
// persistent inbox.
const agyRunCmdName = "agy-run"

// agyUnit is the transient user unit an agy agent runs in.
func agyUnit(id string) string { return "beekeeper-agy-" + id[:min(8, len(id))] }

func (a *app) preInvocationCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "preinvocation",
		Short: "The Antigravity PreInvocation hook: drain the session's inbox before each model call",
		Long: `preinvocation reads an Antigravity CLI (agy) PreInvocation event on stdin,
drains the session's persistent inbox (resolved by conversationId from the
hook's stdin, or BEEKEEPER_AGY_AGENT), and prints an injectSteps JSON object
with one userMessage step per queued message. A message sent by "beekeeper
agents wake <name> <message>" while a turn runs reaches the model before its
next model call inside the running turn; when the session is idle, the unit's
runner delivers the message on stdin as the next turn.

Register it in ~/.gemini/config/hooks.json, as beekeeper install does, next
to the PreToolUse guard: PreInvocation lists the commands themselves, not
matchers (agy rejects the whole entry for one):

  {"beekeeper": {
    "PreInvocation": [{"type": "command",
      "command": "~/.go/bin/beekeeper hook preinvocation", "timeout": 30}],
    "PreToolUse": [{"matcher": "*", "hooks": [{"type": "command",
      "command": "~/.go/bin/beekeeper hook pretooluse --format agy", "timeout": 30}]}]}}`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			defer func() { _ = recover() }() // a broken hook must not block the model call
			raw, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				return nil
			}
			if a.cfg == nil {
				if err := a.loadConfig(); err != nil {
					return nil
				}
			}
			if out := agy.PreInvocation(a.cfg.StateDir, raw); out != nil {
				_, _ = a.out.Write(out)
			}
			return nil
		},
	}
}

func (a *app) agyRunCmd() *cobra.Command {
	c := &cobra.Command{
		Use:    agyRunCmdName + " <inbox> <log> <command> [args...]",
		Short:  "Run an agy stream-json session on its persistent inbox",
		Hidden: true,
		Args:   cobra.MinimumNArgs(3),
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return agy.Run(cmd.Context(), args[0], args[1], args[2:], os.Stderr)
		},
	}
	c.Flags().SetInterspersed(false)
	return c
}
