package cmd

import (
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

// boardGH is the gh CLI the board commands read and write through; a seam
// for the tests.
var boardGH github.GH = github.RunGH

func (a *app) boardCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "board",
		Short: "Pick the next item of work from the project board, move items",
		Long: `The project board of board.owner and board.project, the items of board.team.
next applies the configured order (board.order) to the board's open items
and the roster and prints the first free item with why it is picked; move
sets an item's Status by the board's canonical names.`,
	}
	c.AddCommand(a.boardNextCmd(), a.boardMoveCmd())
	return c
}

func (a *app) boardNextCmd() *cobra.Command {
	var claim bool
	var waits string
	c := &cobra.Command{
		Use:   "next",
		Short: "Print the next free board item; --claim records it as the calling session's",
		Long: `Reads the board once and walks board.order: each step offers the open items
of its statuses, kinds and labels (the open sub-issues of the epics it
matches, with subIssues; only items whose recorded blockers all closed, with
unblocked; only items created within createdWithin), or a GitHub search's
open issues. A sub-issue that is a board item is held to the order by its
own Status: one no step offers on its own (a Backlog item older than the
Backlog step's createdWithin, a blocked one, one in Inbox) is skipped with
the reason, even when its epic is in progress. The first item that is free
is picked: not served by a running session (a sessions serve record, a busy
agent's task) and named by no open note (it waits on the note's person),
not assigned to anybody outside board.people, and active within
board.staleAfter. It prints the item, why it is picked, and why every item
above it was skipped.

--claim records the pick as the calling session's sessions serve record
under the state lock, after checking again that nobody claimed it since:
two concurrent claims never get the same item. It changes nothing on the
board: the item keeps its Status until the caller, having judged it, moves
it with board move. The claim ends when the session ends, when its agent
reports idle (agents idle) or with sessions unserve. Exit 3 when no item is free.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var me state.Party
			if claim {
				var err error
				if me, err = a.caller(); err != nil {
					return err
				}
				if me.Session == "" {
					return usageErr("--claim records a session's serve: run it inside a Claude Code session, not --as")
				}
			} else {
				me, _ = a.caller()
			}
			cl := &board.Client{GH: boardGH, Board: a.cfg.Board}
			snap, err := cl.Read(cmd.Context(), a.now)
			if err != nil {
				return err
			}
			cands := board.Rank(snap, a.cfg.Board, a.now)
			listed := time.Now()
			sessions, _, err := a.sessions()
			if err != nil {
				return err
			}
			alive := func(p state.Party) bool {
				return slices.ContainsFunc(sessions, func(s *claude.Session) bool { return s.Party().Is(p) })
			}
			var res nextResult
			if claim {
				res, err = claimNext(a.store, cands, me, alive, listed, waits)
			} else {
				var st *state.State
				if st, err = a.store.Read(); err == nil {
					res = nextFree(st, cands, me, alive, listed)
				}
			}
			if err != nil {
				return err
			}
			return a.printNext(res, len(cands))
		},
	}
	c.Flags().BoolVar(&claim, "claim", false, "record the item as the calling session's (sessions serve), atomically")
	c.Flags().StringVar(&waits, "waits", "", "what the session waits on, for the record")
	return c
}

// nextResult is the pick, nil when no item is free, and the candidates
// skipped above it with the reason.
type nextResult struct {
	Pick    *board.Candidate  `json:"pick"`
	Claimed bool              `json:"claimed,omitempty"`
	Skipped []board.Candidate `json:"skipped,omitempty"`
}

// nextFree walks the candidates in order and returns the first free one.
// An item is owned by a record of a running session (or of one that
// started after listed, the moment the running sessions were listed),
// unless the session is a registered agent reporting idle, and by a busy
// agent whose task names it.
func nextFree(st *state.State, cands []board.Candidate, me state.Party, alive func(state.Party) bool, listed time.Time) nextResult {
	owners := boardOwners(st, me, alive, listed)
	var res nextResult
	for _, c := range cands {
		if c.Skip == "" {
			if o, ok := owners[strings.ToLower(c.Ref)]; ok {
				c.Skip = o
				if !strings.HasPrefix(o, "note ") {
					c.Skip = "served by " + o
				}
			}
		}
		if c.Skip == "" {
			res.Pick = &c
			return res
		}
		res.Skipped = append(res.Skipped, c)
	}
	return res
}

// claimNext picks the next free candidate and records it as me's serve
// in one update of the state, so a concurrent claim sees it.
func claimNext(store *state.Store, cands []board.Candidate, me state.Party, alive func(state.Party) bool, listed time.Time, waits string) (nextResult, error) {
	var res nextResult
	err := store.Update(func(st *state.State) ([]state.Event, error) {
		res = nextFree(st, cands, me, alive, listed)
		if res.Pick == nil {
			return nil, nil
		}
		r := state.Record{Session: me, Issue: res.Pick.Ref, Waits: strings.Join(strings.Fields(waits), " "), By: me, At: time.Now().UTC()}
		st.Records = slices.DeleteFunc(st.Records, func(o state.Record) bool { return o.Session.Is(me) })
		st.Records = append(st.Records, r)
		res.Claimed = true
		return []state.Event{event(me, "board.claim", "%s: %s (%s)", me.Name, recordText(r), res.Pick.Why())}, nil
	})
	return res, err
}

// taskRef finds the issues an agent's task names: owner/repo#n or a URL.
var taskRef = regexp.MustCompile(`([\w.-]+/[\w.-]+)#(\d+)|github\.com/([\w.-]+/[\w.-]+)/(?:issues|pull)/(\d+)`)

// boardOwners maps each issue (lower-cased owner/repo#n) a live session
// serves, or an open note waits on, to who serves it or whom it waits on.
func boardOwners(st *state.State, me state.Party, alive func(state.Party) bool, listed time.Time) map[string]string {
	idle := func(p state.Party) bool {
		i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(p) })
		return i >= 0 && (st.Agents[i].Task == "" || st.Agents[i].Done)
	}
	name := func(p state.Party) string {
		if p.Is(me) {
			return "you"
		}
		return fmt.Sprintf("%q", p.Name)
	}
	out := map[string]string{}
	for _, r := range st.Records {
		if r.Ended.IsZero() && (alive(r.Session) || r.At.After(listed)) && !idle(r.Session) {
			out[strings.ToLower(r.Issue)] = name(r.Session)
		}
	}
	named := func(text, who string) {
		for _, m := range taskRef.FindAllStringSubmatch(text, -1) {
			ref := strings.ToLower(m[1] + "#" + m[2])
			if m[3] != "" {
				ref = strings.ToLower(m[3] + "#" + m[4])
			}
			if _, ok := out[ref]; !ok {
				out[ref] = who
			}
		}
	}
	for _, ag := range st.Agents {
		if ag.Task != "" && !ag.Done {
			named(ag.Task, name(ag.Party)+" (task)")
		}
	}
	for _, n := range st.Notes {
		named(n.Text, fmt.Sprintf("note #%d (waits on %s)", n.ID, cmp.Or(n.For, "the supervisor")))
	}
	return out
}

func (a *app) printNext(res nextResult, offered int) error {
	if a.json {
		if err := a.printJSON(res); err != nil {
			return err
		}
	} else {
		if res.Pick != nil {
			p := res.Pick
			_, _ = fmt.Fprintf(a.out, "%s %s\n  %s\n  picked: %s\n", p.Ref, p.Title, p.URL, p.Why())
			if res.Claimed {
				_, _ = fmt.Fprintf(a.out, "  claimed: you serve %s now, its Status unchanged (board move %s %q once you take it on; sessions unserve releases it)\n", p.Ref, p.Ref, "in progress")
			}
		}
		if len(res.Skipped) > 0 {
			head := "skipped above it:"
			if res.Pick == nil {
				head = "skipped:"
			}
			_, _ = fmt.Fprintln(a.out, head)
			w := a.table()
			for _, c := range res.Skipped {
				_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", c.Ref, truncate(c.Title, 50), c.Step, c.Skip)
			}
			_ = w.Flush()
		}
	}
	if res.Pick == nil {
		return refused("no free board item: the order offered %d, all skipped", offered)
	}
	return nil
}

func (a *app) boardMoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "move <owner/repo#n|URL> <status>",
		Short: "Set a board item's Status by its canonical name",
		Long: `Sets the Status of the issue's board item. The status is the board's value,
or an unambiguous part of it, case, emoji and punctuation ignored ("up next",
"progress"); anything else is refused with the board's values, before
anything is written. Exit 3 when refused.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, _, _, err := board.Ref(args[0]); err != nil {
				return usageErr("%v", err)
			}
			me, _ := a.caller()
			cl := &board.Client{GH: boardGH, Board: a.cfg.Board}
			mv, err := cl.Move(cmd.Context(), args[0], args[1])
			if r := (*board.Refusal)(nil); errors.As(err, &r) {
				return refused("%s", r.Reason)
			}
			if err != nil {
				return err
			}
			if mv.From != mv.To {
				_ = a.store.Log(event(me, "board.move", "%s: %s → %s", mv.Ref, mv.From, mv.To))
			}
			if a.json {
				return a.printJSON(mv)
			}
			if mv.From == mv.To {
				_, err = fmt.Fprintf(a.out, "%s is in %s already\n", mv.Ref, mv.To)
				return err
			}
			_, err = fmt.Fprintf(a.out, "moved %s: %s → %s\n", mv.Ref, mv.From, mv.To)
			return err
		},
	}
}
