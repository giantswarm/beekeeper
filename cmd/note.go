package cmd

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/state"
)

func (a *app) noteCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "note",
		Short: "Open items that outlive a session: questions for a person, deadlines",
		Long: `Notes are the open items a supervisor would otherwise carry only in its
transcript: the decisions waiting on a person, a deadline to check. They
appear in every hand-over until marked done.

Without a subcommand, lists the open notes.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return a.noteList() },
	}
	var forWho, due string
	add := &cobra.Command{
		Use:   "add <text>",
		Short: "Add a note",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			d, err := untilTime(a.now, due)
			if err != nil {
				return err
			}
			me, err := a.caller()
			if err != nil {
				return err
			}
			n := state.Note{For: forWho, Text: strings.Join(args, " "), Due: d.UTC(), By: me, At: a.now.UTC()}
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.NextNote++
				n.ID = st.NextNote
				st.Notes = append(st.Notes, n)
				return []state.Event{event(me, "note.add", "#%d %s", n.ID, n.Text)}, nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.out, "note #%d\n", n.ID)
			return err
		},
	}
	add.Flags().StringVar(&forWho, "for", "", "who has to act (a person's name)")
	add.Flags().StringVar(&due, "due", "", "when it is due: a time (22:55) or a duration (3h)")
	done := &cobra.Command{
		Use:   "done <id>...",
		Short: "Mark notes done",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			me, err := a.caller()
			if err != nil {
				return err
			}
			var ids []int
			for _, s := range args {
				id, err := strconv.Atoi(strings.TrimPrefix(s, "#"))
				if err != nil {
					return &exitError{code: ExitUsage, msg: fmt.Sprintf("%q is not a note id", s)}
				}
				ids = append(ids, id)
			}
			var evs []state.Event
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Notes = slices.DeleteFunc(st.Notes, func(n state.Note) bool {
					if slices.Contains(ids, n.ID) {
						evs = append(evs, event(me, "note.done", "#%d %s", n.ID, n.Text))
						return true
					}
					return false
				})
				return evs, nil
			})
			if err != nil {
				return err
			}
			if len(evs) < len(ids) {
				return refused("%d of the %d notes were open", len(evs), len(ids))
			}
			return nil
		},
	}
	list := listCmd("List the open notes", a.noteList)
	c.AddCommand(add, done, list)
	return c
}

func (a *app) noteList() error {
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(st.Notes)
	}
	a.printNotes(st.Notes)
	return nil
}

func (a *app) printNotes(notes []state.Note) {
	if len(notes) == 0 {
		_, _ = fmt.Fprintln(a.out, "no open notes")
		return
	}
	for _, n := range notes {
		var tags []string
		if n.For != "" {
			tags = append(tags, "for "+n.For)
		}
		if !n.Due.IsZero() {
			tags = append(tags, "due "+clock(a.now, n.Due))
			if n.Due.Before(a.now) {
				tags[len(tags)-1] += " (overdue)"
			}
		}
		tag := ""
		if len(tags) > 0 {
			tag = " [" + strings.Join(tags, ", ") + "]"
		}
		_, _ = fmt.Fprintf(a.out, "#%d%s %s\n", n.ID, tag, n.Text)
	}
}
