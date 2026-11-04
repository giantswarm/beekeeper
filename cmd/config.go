package cmd

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/config"
)

// configName is the config command's name.
const configName = "config"

func (a *app) configCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   configName,
		Short: "Check or edit the configuration",
		Args:  cobra.NoArgs,
		// The configuration it edits may not load: it is what config set
		// repairs.
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return nil
		},
	}
	c.AddCommand(&cobra.Command{
		Use:   "check",
		Short: "Validate configuration keys, values and local input files",
		Long: `check reads the same configuration file as other commands (--config,
$BEEKEEPER_CONFIG, or $XDG_CONFIG_HOME/beekeeper/config.yaml). It reports
unknown keys, invalid values and missing explicitly configured local input
files by key path. It does not read referenced files, resolve secrets, run
commands or open state. Output paths and unconfigured defaults need not exist.

Exit codes: 0 valid, 1 invalid or unreadable, 2 usage.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := config.Path(a.cfgPath)
			if err != nil {
				return err
			}
			findings, err := config.Check(path)
			if err != nil {
				return err
			}
			if len(findings) != 0 {
				return errors.New(strings.Join(findings, "\n"))
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "valid configuration: %s\n", path)
			return err
		},
	})
	var spec overrideSpec
	set := &cobra.Command{
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

--until makes the change a temporary override with an end: a time (18:00,
2h) or an issue or pull request (owner/repo#n or its URL) whose closing
ends it, and --reason says why. The state records each key's value before
(or that the file had none); config overrides and beekeeper handover list
the overrides in effect. Once the end arrives, the watch (or the supervisor
loop) says so and restores the values before, each step in the event log
(config.override.end, config.restored). A key that changed since the
override set it is not restored: the watch asks guide.person in a note,
answered with config restore <id> --force or config keep <id>. A key
another override in effect set is taken over, with that one's value before.
stateDir cannot be overridden temporarily: restoring it would switch state history.

  beekeeper config set --until owner/repo#42 \
    --reason "budget floor off for the merge batch" -- github.floor -1

A value that starts with "-" follows "--", the flags before it.

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
			if spec.until != "" || spec.reason != "" {
				if spec.until == "" {
					return usageErr("--reason needs --until: a change without an end is a plain set")
				}
				return a.setOverride(path, settings, spec)
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
	}
	set.Flags().StringVar(&spec.until, "until", "", "make it an override that ends at a time or duration, or once an issue or pull request (owner/repo#n) closes")
	set.Flags().StringVar(&spec.reason, "reason", "", "why the override is in effect (with --until)")
	c.AddCommand(set)
	c.AddCommand(a.overridesCmds()...)
	return c
}

// warnIncomplete says each reference of c whose source is missing.
func warnIncomplete(w io.Writer, c *config.Config) {
	for _, m := range c.Incomplete() {
		_, _ = fmt.Fprintln(w, "beekeeper: warning: "+m)
	}
}
