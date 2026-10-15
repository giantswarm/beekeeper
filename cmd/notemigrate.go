package cmd

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/state"
)

// noteMigrated is the event of a memo or pinned note `note migrate --apply`
// closed once its content moved to its issue.
const noteMigrated = "note.migrated"

// noteMigrateCmd lists and closes the memos and pinned notes an older
// beekeeper filed, which every other reader of the state reads as closed.
func (a *app) noteMigrateCmd() *cobra.Command {
	var dryRun, apply bool
	c := &cobra.Command{
		Use:   "migrate --dry-run | --apply [<id>...]",
		Short: "List the memos and pinned notes of an older beekeeper, or close them",
		Long: `A one-time migration of the memos and pinned notes an older beekeeper
filed. beekeeper files only decisions and sign-ins: a memo, a note filed
before kinds and a pinned note are closed for every reader of the state
but this command, which keeps them until they are closed here.

--dry-run lists each one with its whole text and the issues and pull
requests it names (owner/repo#n, from its text and its --ref links), so
its content can move to its issue: a comment, a blocked-by (board block),
a rank (board rank), a board item for deferred work, a rule for a
standing instruction. --json prints them as note add's --json --full does.

--apply closes them, every one or the ids given, each logged as
note.migrated with its whole text. A pinned note, a standing instruction,
first moves into standing.md in the state directory, which the
supervisor's and the guide's hand-overs and prompts print in full: its
block there is its id, whom it is for, who pinned it and its whole text.
A pin whose block the file does not carry after the write is not closed:
--apply closes nothing and names it. Until --apply, the hand-overs print
the open pins in full beside the file.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if dryRun == apply {
				return usageErr("note migrate takes one of --dry-run and --apply")
			}
			ids, err := parseIDs(args, "note")
			if err != nil {
				return err
			}
			if dryRun {
				if len(ids) > 0 {
					return usageErr("--dry-run lists every memo: ids go with --apply")
				}
				return a.listMemos()
			}
			return a.closeMemos(ids)
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "list the memos and pinned notes with the issues each names")
	c.Flags().BoolVar(&apply, "apply", false, "close the memos and pinned notes: every one, or the ids given")
	return c
}

// listMemos prints the state's memos and pinned notes, each with its whole
// text and what it names.
func (a *app) listMemos() error {
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	if a.json {
		out := make([]map[string]any, 0, len(st.Memos))
		for _, n := range st.Memos {
			m, err := a.namedNote(&n)
			if err != nil {
				return err
			}
			out = append(out, m)
		}
		return a.printJSON(out)
	}
	if len(st.Memos) == 0 {
		_, err = fmt.Fprintln(a.out, "no memos or pinned notes")
		return err
	}
	pinned, named := 0, map[string]bool{}
	for _, n := range st.Memos {
		tags := "memo"
		if n.Pinned() {
			tags, pinned = "pinned", pinned+1
		}
		if n.For != "" {
			tags += ", for " + n.For
		}
		if n.By.Name != "" {
			tags += ", by " + quotedOwner(n.By)
		}
		_, _ = fmt.Fprintf(a.out, "#%d [%s]\n", n.ID, tags)
		for line := range strings.Lines(strings.TrimRight(n.Text, "\n")) {
			_, _ = fmt.Fprintf(a.out, "  %s\n", strings.TrimRight(line, "\n"))
		}
		if names := a.noteNames(&n); len(names) > 0 {
			_, _ = fmt.Fprintf(a.out, "  names: %s\n", strings.Join(names, ", "))
			for _, r := range names {
				named[r] = true
			}
		}
	}
	_, err = fmt.Fprintf(a.out, "%d memos (%d pinned) naming %d issues and pull requests: move each memo's content to its issue, then note migrate --apply, which moves the pins into "+standingFile+"\n",
		len(st.Memos), pinned, len(named))
	return err
}

// closeMemos closes the memos and pinned notes with ids, every one without.
// It closes a pin only once standing.md carries its whole text: a pin it
// cannot move there keeps every note open and is named in the refusal.
func (a *app) closeMemos(ids []int) error {
	me, err := a.caller()
	if err != nil {
		return err
	}
	var missing, lost []int
	var moveErr error
	var evs []state.Event
	moved := 0
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		missing = slices.DeleteFunc(slices.Clone(ids), func(id int) bool {
			return slices.ContainsFunc(st.Memos, func(n state.Note) bool { return n.ID == id })
		})
		if len(missing) > 0 {
			return nil, nil
		}
		closing := func(n state.Note) bool { return len(ids) == 0 || slices.Contains(ids, n.ID) }
		var pins []state.Note
		for _, n := range st.Memos {
			if closing(n) && n.Pinned() {
				pins = append(pins, n)
			}
		}
		if lost, moveErr = a.movePins(pins); moveErr != nil {
			return nil, nil
		}
		moved = len(pins)
		st.Memos = slices.DeleteFunc(st.Memos, func(n state.Note) bool {
			if !closing(n) {
				return false
			}
			evs = append(evs, event(me, noteMigrated, "#%d %s", n.ID, n.Text))
			return true
		})
		return evs, nil
	})
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return refused("no memo or pinned note #%s: note migrate --dry-run lists them", joinIDs(missing))
	}
	if moveErr != nil {
		return refused("closed nothing: pinned note #%s not moved into %s: %v", joinIDs(lost), a.standingPath(), moveErr)
	}
	_, err = fmt.Fprintf(a.out, "closed %d memos and pinned notes; %d pins moved into %s, which both hand-overs print\n", len(evs), moved, a.standingPath())
	return err
}

// joinIDs is ids as "3, #5".
func joinIDs(ids []int) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = fmt.Sprint(id)
	}
	return strings.Join(s, ", #")
}
