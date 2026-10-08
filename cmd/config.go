package cmd

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/config"
)

func (a *app) configCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "Edit the configuration in one validated write",
		Args:  cobra.NoArgs,
		// The configuration it edits may not load: it is what config set
		// repairs.
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return nil
		},
	}
	c.AddCommand(&cobra.Command{
		Use:   "set <key> <value> [<key> <value>]…",
		Short: "Set keys of the configuration file together, validated before the write",
		Long: `set writes every key with its value into the configuration file in one
write: the result is validated first, and a result that would not load is
refused with the file unchanged. Every session's hook loads the file, and
reads it either before the write or after it, never between two keys.

A key is a dotted path of mapping keys (secret.store.read), created where
missing; a value is YAML (a scalar, [a, b] or {k: v}) and replaces the
key's value whole. Comments and the other keys stay.

Keys that reference each other are set in one call, the reference and its
source together:

  beekeeper config set secret.store '{read: [secret-tool, lookup, entry]}' \
    secret.ageIdentities '[{recipient: age1…, ref: "store://keys/age"}]'

A reference whose source is still missing (a store:// age identity without
secret.store) is written with a warning; the commands that follow it fail
until the source is set, every other command works.

Exit codes: 0 written, 2 usage, 3 refused (the file is unchanged).`,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 || len(args)%2 != 0 {
				return fmt.Errorf("want <key> <value> pairs, got %d arguments", len(args))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := config.Path(a.cfgPath)
			if err != nil {
				return err
			}
			settings := make([]config.Setting, 0, len(args)/2)
			keys := make([]string, 0, len(args)/2)
			for i := 0; i < len(args); i += 2 {
				settings = append(settings, config.Setting{Key: args[i], Value: args[i+1]})
				keys = append(keys, args[i])
			}
			c, err := config.Set(path, settings)
			switch {
			case errors.Is(err, config.ErrInvalid):
				return refused("%v", err)
			case err != nil:
				return err
			}
			warnIncomplete(cmd.ErrOrStderr(), c)
			_, err = fmt.Fprintf(a.out, "set %s in %s\n", strings.Join(keys, ", "), path)
			return err
		},
	})
	return c
}

// warnIncomplete says each reference of c whose source is missing.
func warnIncomplete(w io.Writer, c *config.Config) {
	for _, m := range c.Incomplete() {
		_, _ = fmt.Fprintln(w, "beekeeper: warning: "+m)
	}
}
