package cmd

import (
	"cmp"
	"context"
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
	var claim, replace bool
	var waits string
	c := &cobra.Command{
		Use:   "next",
		Short: "Print the next free board item; --claim records it as the calling session's",
		Long: `Reads the board once and walks board.order: each step offers the open items
of its statuses, kinds and labels (the open sub-issues of the epics it
matches, with subIssues; only items whose recorded blockers all closed, with
unblocked; only items created within createdWithin), or a GitHub search's
open issues. A sub-issue that is a board item is held to the order by its
own Status, whatever its Team: one no step offers on its own (a Backlog item older than the
Backlog step's createdWithin, a blocked one, one in Inbox) is skipped with
the reason, even when its epic is in progress. The first item that is free
is picked: not served by a running session (a sessions serve record, a busy
agent's task) nor by an agent on the roster, its CLI running or not, while it is
busy with its task or parked and kept (agents keep, or an open timer that
wakes it by name), and named by no open note (it waits on the note's person),
not assigned to anybody outside board.people, without an open recorded
blocker, and active within board.staleAfter. A sub-issue offered through
an epic passes the same checks, and a serve record, task or note naming
the epic covers it too ("…, on epic owner/repo#n"). It prints the item,
why it is picked, and why every item above it was skipped.

--claim records the pick as the calling session's sessions serve record
under the state lock, after checking again that nobody claimed it since:
two concurrent claims never get the same item. It changes nothing on the
board: the item keeps its Status until the caller, having judged it, moves
it with board move. The claim ends when the session ends, when its agent
reports idle (agents idle) or with sessions unserve <owner/repo#n>. A
second claim while the session still serves an open item is refused with
its record, which stays as it was; --replace takes the next item and
replaces the record. Exit 3 when no item is free or the claim is refused.`,
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
			} else if replace {
				return usageErr("--replace replaces the record of a --claim: pass --claim too")
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
				open := func(string) bool { return false }
				if !replace {
					if open, err = servedOpen(cmd.Context(), cl, a.store, me); err != nil {
						return err
					}
				}
				res, err = claimNext(a.store, cands, me, alive, listed, waits, open)
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
	c.Flags().BoolVar(&replace, "replace", false, "with --claim: replace the session's record even while the item it serves is open")
	c.Flags().StringVar(&waits, "waits", "", "what the session waits on, for the record")
	return c
}

// servedOpen asks GitHub whether each item me's records serve is open, ahead
// of the claim's state lock. It reports an item it did not ask about as
// open: a record written since is the session's own, and is kept.
func servedOpen(ctx context.Context, cl *board.Client, store state.Store, me state.Party) (func(string) bool, error) {
	st, err := store.Read()
	if err != nil {
		return nil, err
	}
	asked := map[string]bool{}
	for _, r := range st.Records {
		if !r.Session.Is(me) || !r.Ended.IsZero() {
			continue
		}
		if asked[strings.ToLower(r.Issue)], err = cl.Open(ctx, r.Issue); err != nil {
			return nil, fmt.Errorf("is %s, the item this session serves, open: %w", r.Issue, err)
		}
	}
	return func(ref string) bool {
		o, ok := asked[strings.ToLower(ref)]
		return o || !ok
	}, nil
}

// nextResult is the pick, nil when no item is free, and the candidates
// skipped above it with the reason.
type nextResult struct {
	Pick    *board.Candidate `json:"pick"`
	Claimed bool             `json:"claimed,omitempty"`
	// Held is the record a claim was refused for: the open item the
	// session serves.
	Held    *state.Record     `json:"held,omitempty"`
	Skipped []board.Candidate `json:"skipped,omitempty"`
}

// nextFree walks the candidates in order and returns the first free one.
// An item is owned by a record of a running session (or of one that
// started after listed, the moment the running sessions were listed),
// unless the session is a registered agent reporting idle, by the record
// of an agent kept on the roster (keptBy) or busy with its task on it,
// whether its CLI runs or not, by a
// busy agent whose task names it and by an open note naming it. A sub-issue
// offered through an epic is owned by whatever owns the epic too.
func nextFree(st *state.State, cands []board.Candidate, me state.Party, alive func(state.Party) bool, listed time.Time) nextResult {
	owners := boardOwners(st, me, alive, listed)
	var res nextResult
	owned := func(ref string) string {
		o, ok := owners[strings.ToLower(ref)]
		if ok && !strings.HasPrefix(o, "note ") {
			o = "served by " + o
		}
		return o
	}
	for _, c := range cands {
		if c.Skip == "" {
			c.Skip = owned(c.Ref)
		}
		if c.Skip == "" && c.Epic != "" {
			if o := owned(c.Epic); o != "" {
				c.Skip = o + ", on epic " + c.Epic
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
// in one update of the state, so a concurrent claim sees it. While me
// serves an item open reports open, and me is no agent reporting idle,
// it changes nothing and returns that record as Held.
func claimNext(store state.Store, cands []board.Candidate, me state.Party, alive func(state.Party) bool, listed time.Time, waits string, open func(string) bool) (nextResult, error) {
	var res nextResult
	err := store.Update(func(st *state.State) ([]state.Event, error) {
		if i := slices.IndexFunc(st.Records, func(r state.Record) bool {
			return r.Session.Is(me) && r.Ended.IsZero() && open(r.Issue)
		}); i >= 0 && !agentIdle(st, me) {
			res.Held = &st.Records[i]
			return nil, nil
		}
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

// agentIdle reports whether p is a registered agent reporting idle: without
// a task, or done with it.
func agentIdle(st *state.State, p state.Party) bool {
	i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(p) })
	return i >= 0 && (st.Agents[i].Task == "" || st.Agents[i].Done)
}

// taskRef finds the issues an agent's task names: owner/repo#n or a URL.
var taskRef = regexp.MustCompile(`([\w.-]+/[\w.-]+)#(\d+)|github\.com/([\w.-]+/[\w.-]+)/(?:issues|pull)/(\d+)`)

// boardOwners maps each issue (lower-cased owner/repo#n) a live session
// serves, or an open note waits on, to who serves it or whom it waits on.
func boardOwners(st *state.State, me state.Party, alive func(state.Party) bool, listed time.Time) map[string]string {
	name := func(p state.Party) string {
		if p.Is(me) {
			return "you"
		}
		return fmt.Sprintf("%q", p.Name)
	}
	// kept says what keeps the record's session on the roster, parked.
	kept := func(p state.Party) string {
		i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(p) })
		if i < 0 || st.Agents[i].Done {
			return ""
		}
		return keptBy(st, st.Agents[i], listed)
	}
	// busy says the record's session is an agent on the roster working its
	// task: it owns its item while its CLI is gone (the desktop warmed none,
	// a headless turn ended) until it reports idle or leaves the roster.
	busy := func(p state.Party) bool {
		i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(p) })
		return i >= 0 && !agentIdle(st, p)
	}
	out := map[string]string{}
	for _, r := range st.Records {
		live := r.Ended.IsZero() && (alive(r.Session) || r.At.After(listed))
		switch k := kept(r.Session); {
		case k != "":
			out[strings.ToLower(r.Issue)] = fmt.Sprintf("%s (parked, %s)", name(r.Session), k)
		case live && !agentIdle(st, r.Session):
			out[strings.ToLower(r.Issue)] = name(r.Session)
		case busy(r.Session):
			out[strings.ToLower(r.Issue)] = fmt.Sprintf("%s (busy, no CLI)", name(r.Session))
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
		if h := res.Held; h != nil {
			_, _ = fmt.Fprintf(a.out, "this session %s: finish it, release it with sessions unserve %s, or claim with --replace\n", recordText(*h), h.Issue)
		}
		if res.Pick != nil {
			p := res.Pick
			_, _ = fmt.Fprintf(a.out, "%s %s\n  %s\n  picked: %s\n", p.Ref, p.Title, p.URL, p.Why())
			if res.Claimed {
				_, _ = fmt.Fprintf(a.out, "  claimed: you serve %s now, its Status unchanged (board move %s %q once you take it on; sessions unserve %s releases it)\n", p.Ref, p.Ref, "in progress", p.Ref)
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
	if h := res.Held; h != nil {
		return refused("no claim: this session serves %s, still open", h.Issue)
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
