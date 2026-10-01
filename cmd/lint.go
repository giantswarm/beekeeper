package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/brieflint"
)

func (a *app) lintCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "lint",
		Short: "Check skills and briefs for what goes stale",
		Args:  cobra.NoArgs,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	c.AddCommand(&cobra.Command{
		Use:   "briefs <file or folder>...",
		Short: "Refuse dated lines, waits on a release and fixed workarounds in skills and briefs",
		Long: `briefs reads each file named, and every Markdown file below each folder
named (hidden folders left out), and prints one line per line a rule
refuses, path:line: rule: why, followed by the line:

  date         a date on its own (2026-09-27), not one inside a file name or
               a version: the line says what held then, not what holds now
  until-ships  "until X ships" (lands, merges, is released, is fixed, ...):
               the line turns wrong the day X ships
  fixed-in     "(v0.53.0: ...)", "since v0.53.0", "fixed in v...": the
               history of a behaviour instead of the behaviour
  workaround   "workaround", "for now", "temporarily": a step that stops
               being right once the fix ships
  role-run     "Supervisor run 59": a relay makes it stale; "the supervisor"
               reaches whoever holds the role

Skills and briefs say what holds now; the history lives in the issues and
the changelog. A clean run prints nothing and exits 0; any finding exits 3,
so a project holding skills can run it as a CI step.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			var found []brieflint.Finding
			for _, p := range args {
				fi, err := os.Stat(p)
				if err != nil {
					return err
				}
				dir, root := p, "."
				if !fi.IsDir() {
					dir, root = filepath.Dir(p), filepath.Base(p)
				}
				f, err := brieflint.LintFS(os.DirFS(dir), root, dir)
				if err != nil {
					return err
				}
				found = append(found, f...)
			}
			if a.json {
				if err := a.printJSON(found); err != nil {
					return err
				}
			} else {
				for _, f := range found {
					if _, err := fmt.Fprintf(a.out, "%s:%d: %s: %s\n  %s\n", f.Path, f.Line, f.Rule, f.Why, f.Text); err != nil {
						return err
					}
				}
			}
			if len(found) > 0 {
				return refused("%d stale lines in %d paths checked", len(found), len(args))
			}
			return nil
		},
	})
	return c
}
