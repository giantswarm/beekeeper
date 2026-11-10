package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/secret"
)

const secretMoveOp = "move"

func (a *app) secretMoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "move op://<source-vault>/<item> <destination-vault>",
		Short: "Move a whole item to an allowed vault through the broker and 1Password app",
		Long: `Move a whole item, including its fields and attachments, using 1Password's
native item move. The source must be in secret.vault or secret.vaults and
the destination must be explicitly listed in secret.moveVaults. An empty
destination list refuses every move. This does not grant field-write access.

Every move runs in the broker using 1Password desktop app integration;
approve its authentication prompt there. The broker's cached vault session
and automatic sign-in helper are not used. The desktop app must already
have CLI integration enabled. No value, item JSON or op diagnostic is
returned or logged. A failed or interrupted move is never automatically
retried: inspect item metadata in both vaults before retrying.

1Password moves the item out of the source vault; existing source references
must be updated; the moved item gets a new ID. Use the item ID when titles are ambiguous.

  beekeeper secret move 'op://Personal/application' 'Team Shared'`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// No token file is needed: the desktop app owns authentication.
			ops := &secret.Ops{Run: secretRun, Vault: a.cfg.Secret.Vault,
				Vaults: a.cfg.Secret.Vaults, MoveVaults: a.cfg.Secret.MoveVaults}
			err := ops.MoveItem(cmd.Context(), args[0], args[1])
			if err := a.secretLog(err, secretMoveOp, "%s to %s: %s", args[0], args[1], outcome(err, "moved")); err != nil {
				return err
			}
			return a.secretPrint(map[string]string{"source": args[0], "destination": args[1], "status": "moved"},
				fmt.Sprintf("moved %s to %s\n", args[0], args[1]))
		},
	}
}
