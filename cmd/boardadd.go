package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/loop"
)

func (a *app) boardFieldsCmd(add bool) *cobra.Command {
	name, short := "show", "Show an issue's board fields without reading the whole board"
	if add {
		name, short = loop.LoopAdd, "Add an issue to the board and set Team, Status and optional Kind"
	}
	var status, kind string
	c := &cobra.Command{Use: name + " <owner/repo#n|URL>", Short: short, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, _, _, err := board.Ref(args[0]); err != nil {
				return usageErr("%v", err)
			}
			cl := a.boardClient()
			var fields *board.Fields
			var err error
			if add {
				fields, err = cl.Add(cmd.Context(), args[0], status, kind)
			} else {
				fields, err = cl.Show(cmd.Context(), args[0])
			}
			if r := (*board.Refusal)(nil); errors.As(err, &r) {
				return refused("%s", r.Reason)
			}
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(fields)
			}
			if !fields.OnBoard {
				_, err = fmt.Fprintf(a.out, "%s is not on the configured board\n", fields.Ref)
				return err
			}
			_, err = fmt.Fprintf(a.out, "%s: Team %s; Status %s; Kind %s (item %s)\n", fields.Ref, fields.Team, fields.Status, fields.Kind, fields.Item)
			return err
		}}
	if add {
		c.Flags().StringVar(&status, "status", "", "Status (default Backlog), using the board's canonical values")
		c.Flags().StringVar(&kind, "kind", "", "Kind, using the board's canonical values (omitted: preserve existing)")
	}
	return c
}
