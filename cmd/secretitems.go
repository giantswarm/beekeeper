package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/secret"
)

func (a *app) secretItemsCmd() *cobra.Command {
	var opt secret.ItemsOptions
	c := &cobra.Command{
		Use:   "items op://<vault> [<match>] [--fields]",
		Short: "The item titles of a vault beekeeper reads, and with --fields their field names and lengths, never a value",
		Long: `items lists the items of a vault beekeeper reads (secret.vault,
secret.vaults or secret.ageVaults): each item's title, category, tags and
last change, the items whose title, category or a tag contains <match>
(case-insensitive) when one is given. --fields reads each listed item in
beekeeper's process and answers its fields' labels, sections, types and
value lengths; no value is printed, logged or returned. Another vault is
refused before op runs, naming the vaults listed. With secret.session it
runs in the broker, like every call on the vault.

  beekeeper secret items "op://<vault>" openai --fields`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			vault, ok := strings.CutPrefix(args[0], guard.OpRef)
			if !ok {
				return usageErr("%q: name the vault as op://<vault>", args[0])
			}
			if len(args) > 1 {
				opt.Match = args[1]
			}
			ops, err := a.secretOps()
			if err != nil {
				return err
			}
			items, err := ops.Items(cmd.Context(), vault, opt)
			if err := a.secretLog(err, "items", "%s %q: %s", args[0], opt.Match, outcome(err, fmt.Sprintf("%d items", len(items)))); err != nil {
				return err
			}
			var b strings.Builder
			for _, it := range items {
				fmt.Fprintf(&b, "%-50s %-20s %s\n", it.Title, it.Category, strings.Join(it.Tags, ","))
				for _, f := range it.Fields {
					label := f.Label
					if f.Section != "" {
						label = f.Section + "." + label
					}
					fmt.Fprintf(&b, "  %-48s %-12s %d bytes\n", label, f.Type, f.Length)
				}
			}
			return a.secretPrint(items, b.String())
		},
	}
	c.Flags().BoolVar(&opt.Fields, "fields", false, "also each item's field labels, types and value lengths")
	return c
}
