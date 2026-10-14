package cmd

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/state"
)

// onlyPerson is the rule a change of the priority list is refused by.
const onlyPerson = "only the person changes priorities"

func (a *app) priorityCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "priority",
		Short: "Print the person's priority list of epics; set and unset change it, on the person's word only",
		Long: `The priority list is the desk's ordered list of epics, held on the board
(board.owner, board.project) in its number field ` + board.PriorityField + `: an epic's rank, 1
the top, written and read per item by its id. Only the person sets it.
priority prints the open epics of board.team that hold a rank: rank, epic
URL, title and Status, read from the board (--json the list).

board next applies it: its candidates are ordered by the rank of their epic
first, an issue's own or its nearest ranked ancestor's through the native
sub-issue relation, the unranked after the ranked, each rank in board.order's
order, and the pick prints the rank it applied. Every consumer reads the list
with the one function board.Client.Priorities and ranks an issue with
Priorities.Ranks().Of(ref, parent): lease, lanes and capacity call the same.

set and unset change it, and only with --as naming a configured person
(guide.person, a name of serve.people, identity.person): the guide relays
the person's word. A call without --as, from any session (the supervisor, the
guide, a worker), or with another name, is refused (exit 3): ` + onlyPerson + `.
Every change is a priority.set event in beekeeper log naming the person.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			list, err := a.boardClient().Priorities(cmd.Context())
			if err != nil {
				return err
			}
			return a.printPriorities(list)
		},
	}
	c.AddCommand(a.prioritySetCmd(), a.priorityUnsetCmd())
	return c
}

func (a *app) prioritySetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <epic: owner/repo#n|URL> <rank> --as <person>",
		Short: "Put an epic at a rank of the priority list, 1 the top, on the person's word",
		Long: `Puts the epic at the rank: the epics at and below it move down one and the
list stays ranked 1..n, a rank past its end is its end. Only the items whose
rank changes are written. An epic without an item on the board is added
first, as board move adds one, with board.team as its Team. Refused (exit 3)
without --as <person> (` + onlyPerson + `), for an issue that does not exist,
is closed or is no epic (board.epics' kinds), for an issue of a repository
outside the board's owner without an item (never added), and on a board without the
number field ` + board.PriorityField + `.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			rank, err := strconv.Atoi(args[1])
			if err != nil || rank < 1 {
				return usageErr("rank %q: a rank is a whole number from 1, 1 the top", args[1])
			}
			return a.setPriority(cmd, args[0], rank)
		},
	}
}

func (a *app) priorityUnsetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unset <epic: owner/repo#n|URL> --as <person>",
		Short: "Take an epic off the priority list, on the person's word",
		Long: `Clears the epic's rank; the epics below it move up one. An epic not on the
list is left as it is. Refused (exit 3) without --as <person>: ` + onlyPerson + `.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.setPriority(cmd, args[0], 0)
		},
	}
}

// setPriority puts ref at rank, 0 off the list, on the word of the person
// --as names, and logs it.
func (a *app) setPriority(cmd *cobra.Command, ref string, rank int) error {
	if _, _, _, err := board.Ref(ref); err != nil {
		return usageErr("%v", err)
	}
	person, err := a.priorityPerson()
	if err != nil {
		return err
	}
	p, err := a.boardClient().SetPriority(cmd.Context(), ref, rank)
	if r := (*board.Refusal)(nil); errors.As(err, &r) {
		return refused("%s", r.Reason)
	}
	if err != nil {
		return err
	}
	by := state.Party{Name: person}
	relay := ""
	if s := envParty(os.Getenv); s.Name != "" {
		relay = fmt.Sprintf(", relayed by %q", s.Name)
	}
	if p.From != p.To {
		_ = a.store.Log(event(by, "priority.set", "%s: %s, the word of %s%s", p.Ref, rankChange(p), person, relay))
	}
	if a.json {
		return a.printJSON(p)
	}
	if _, err := fmt.Fprintf(a.out, "%s: %s, the word of %s\n", p.Ref, rankChange(p), person); err != nil {
		return err
	}
	return a.printPriorities(p.List)
}

// rankChange says how p changed the epic's rank.
func rankChange(p *board.Prioritized) string {
	switch {
	case p.From == p.To && p.To == 0:
		return "not on the priority list, unchanged"
	case p.From == p.To:
		return fmt.Sprintf("rank %d already", p.To)
	case p.To == 0:
		return fmt.Sprintf("off the priority list (was %d)", p.From)
	case p.From == 0:
		return fmt.Sprintf("rank %d (was unranked)", p.To)
	}
	return fmt.Sprintf("rank %d (was %d)", p.To, p.From)
}

// priorityPerson is the configured person --as names, whose word a change
// of the priority list is; anything else is refused with the rule.
func (a *app) priorityPerson() (string, error) {
	persons := a.persons()
	if a.as == "" {
		who := "a call without --as"
		if role := a.callerRole(); role != "" {
			who = fmt.Sprintf("the %s without --as", role)
		}
		person := "<person>"
		if len(persons) > 0 {
			person = persons[0]
		}
		return "", refused("%s: %s; the guide relays the person's word with --as %s", who, onlyPerson, person)
	}
	if i := slices.IndexFunc(persons, func(p string) bool { return strings.EqualFold(p, a.as) }); i >= 0 {
		return persons[i], nil
	}
	if len(persons) == 0 {
		return "", refused("--as %q: no person is configured (guide.person, serve.people, identity.person): %s", a.as, onlyPerson)
	}
	return "", refused("--as %q is no configured person (%s): %s", a.as, strings.Join(persons, ", "), onlyPerson)
}

// persons are the configured persons: guide.person, the names of
// serve.people and identity.person, each once.
func (a *app) persons() []string {
	var out []string
	for _, p := range append([]string{a.cfg.Guide.Person, a.cfg.Identity.Person}, slices.Sorted(maps.Keys(a.cfg.Serve.People))...) {
		if p != "" && !slices.ContainsFunc(out, func(o string) bool { return strings.EqualFold(o, p) }) {
			out = append(out, p)
		}
	}
	return out
}

// callerRole is the roster role of the calling session: the supervisor or
// the guide it holds, a worker for an agent on the roster; "" for none or
// outside a session.
func (a *app) callerRole() string {
	me := envParty(os.Getenv)
	if me.Session == "" && me.HostSession == "" {
		return ""
	}
	st, err := a.store.Read()
	if err != nil {
		return ""
	}
	for _, rl := range roles {
		if h := rl.get(st).Holder; h != nil && h.Is(me) {
			return rl.name
		}
	}
	if slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(me) }) {
		return workerRole
	}
	return ""
}

func (a *app) printPriorities(list board.Priorities) error {
	if a.json {
		if list == nil {
			list = board.Priorities{}
		}
		return a.printJSON(list)
	}
	if len(list) == 0 {
		_, err := fmt.Fprintf(a.out, "no epic on the priority list of the board %s/%d (its field %s)\n", a.cfg.Board.Owner, a.cfg.Board.Project, board.PriorityField)
		return err
	}
	w := a.table()
	_, _ = fmt.Fprintln(w, "RANK\tEPIC\tTITLE\tSTATUS")
	for _, e := range list {
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", e.Rank, e.URL, truncate(e.Title, 60), e.Status)
	}
	return w.Flush()
}
