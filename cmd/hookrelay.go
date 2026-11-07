package cmd

import (
	"encoding/json"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/peer"
	"github.com/giantswarm/beekeeper/internal/state"
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
			if ev.Event == guideStopEvent || ev.Event == "UserPromptSubmit" {
				store, err := state.Open(a.cfg.StateDir)
				if err != nil {
					return err
				}
				st, err := store.Peek()
				if err != nil {
					return err
				}
				if ev.Event == "UserPromptSubmit" && st.Guide != nil && st.Guide.Holder != nil && st.Guide.Holder.Is(state.Party{Session: ev.Session}) {
					if err := store.UpdateWithin(time.Second, func(st *state.State) ([]state.Event, error) {
						if st.Guide != nil && st.Guide.Holder != nil && st.Guide.Holder.Is(state.Party{Session: ev.Session}) {
							st.Guide.WorkChecked = nil
						}
						return nil, nil
					}); err != nil {
						return err
					}
				}
				if reason := guideStopReason(st, ev.Session, time.Now()); ev.Event == guideStopEvent && a.cfg.Hooks.GuardsEnabled() && reason != "" {
					return json.NewEncoder(a.out).Encode(map[string]string{"decision": "block", "reason": reason})
				}
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
