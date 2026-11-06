package cmd

import (
	"encoding/json"
	"io"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/peer"
)

func (a *app) relayHookCmd() *cobra.Command {
	return &cobra.Command{
		Use: "relay", Short: "Deliver queued instructions at a Claude tool or turn boundary",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				return err
			}
			var ev struct {
				Session string `json:"session_id"`
				Event   string `json:"hook_event_name"`
			}
			if json.Unmarshal(raw, &ev) != nil {
				return nil
			}
			if err := a.loadConfig(); err != nil {
				return err
			}
			out, err := peer.InstructionHook(a.cfg.StateDir, ev.Session, ev.Event)
			if err != nil {
				return err
			}
			if len(out) > 0 {
				_, err = a.out.Write(out)
			}
			return err
		},
	}
}
