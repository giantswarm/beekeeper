package secret

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/guard"
)

// MoveItem moves a whole item through the desktop app's authentication, not
// the broker's cached session. The caller must run in the broker. Native op
// preserves the item's fields and attachments; no item JSON crosses our API.
func (o *Ops) MoveItem(ctx context.Context, source, destination string) error {
	vault, item, err := ParseItem(source)
	if err != nil {
		return err
	}
	if !slices.Contains(o.MoveVaults, destination) || destination == "" {
		return fmt.Errorf("destination %q refused: allowed destinations are %q (secret.moveVaults)", destination, o.MoveVaults)
	}
	if o.Vault == "" || !slices.Contains(o.ReadVaults(), vault) {
		return fmt.Errorf("source vault %q refused: allowed sources are %q (secret.vault, secret.vaults)", vault, o.ReadVaults())
	}
	if vault == destination {
		return fmt.Errorf("source and destination are the same vault %q", vault)
	}
	if strings.HasPrefix(item, "-") || strings.ContainsAny(source+destination, "\x00\r\n") {
		return fmt.Errorf("move requires an item name or ID and vault names without control characters or an item option")
	}
	// Runner overlays the process environment. Blank every inherited credential
	// by name, including credentials injected by a brokered child. Never use a
	// service account, Connect token or cached OP_SESSION for this operation.
	env := []string{"OP_SERVICE_ACCOUNT_TOKEN=", "OP_CONNECT_TOKEN=", "OP_CONNECT_HOST="}
	for _, entry := range append(os.Environ(), o.Env...) {
		key, _, _ := strings.Cut(entry, "=")
		if guard.VaultVar.MatchString(key) {
			env = append(env, key+"=")
		}
	}
	env = append(env, "OP_BIOMETRIC_UNLOCK_ENABLED=true")
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	// Both stdout and errors may contain values. Return neither, even wrapped.
	_, err = o.Run(ctx, "", env, nil, "op", "item", "move", item,
		"--current-vault", vault, "--destination-vault", destination)
	if err != nil {
		return fmt.Errorf("item move did not complete successfully; check 1Password desktop app approval and vault access, then inspect item metadata before retrying")
	}
	return nil
}
