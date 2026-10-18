package cmd

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/state"
)

// boardGH is the gh CLI the board commands read and write through; a seam
// for the tests.
var boardGH github.GH = github.RunGH

// boardClient is the client of the configured board and plans repositories.
func (a *app) boardClient() *board.Client {
	return &board.Client{GH: boardGH, Board: a.cfg.Board, Plans: a.cfg.Plans.Repositories}
}

func (a *app) boardCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "board",
		Short: "Pick the next item of work from the project board, move items",
		Long: `The project board of board.owner and board.project, the items of board.team.
next applies the configured order (board.order) to the board's open items
and the roster and prints the first free item with why it is picked; move
sets an item's Status by the board's canonical names; rank sets an item's
position on the board, which is the order next reads the items of each step
in, under the person's priority list (beekeeper priority); block and
unblock record and remove a hold on an item, a GitHub blocked-by
dependency on another issue, which next reads.`,
	}
	c.AddCommand(a.boardNextCmd(), a.boardMoveCmd(), a.boardRankCmd(), a.boardHoldCmd(true), a.boardHoldCmd(false))
	return c
}

func (a *app) boardNextCmd() *cobra.Command {
	var claim, replace bool
	var waits string
	var view nextView
	c := &cobra.Command{
		Use:   "next",
		Short: "Print the next free board item; --claim records it as the calling session's",
		Long: `Reads the board once and walks board.order: each step offers the open items
of its statuses, kinds and labels (the open sub-issues of the epics it
matches, with subIssues; only items whose recorded blockers all closed, with
unblocked, and whose Status was set before the last of them closed: a Status
set after it was set for a reason no blocker records, a person or a plan,
and holds the item until it changes; only items created within
createdWithin), or a GitHub search's
open issues. A sub-issue that is a board item is held to the order by its
own Status, whatever its Team: one no step offers on its own (a Backlog item older than the
Backlog step's createdWithin, a blocked one, one in Inbox) is skipped with
the reason, even when its epic is in progress. The epic gate (board.epics)
holds whatever the order says: an epic in Up Next is in planning, skipped
with its plan ("epic in planning (Up Next), plan <url> (draft): its
sub-issues wait for the go") or without one (its planning session's), and
never offered through its sub-issues; an issue whose nearest epic among its
parents on the board is in planning is skipped as "sub-issue of epic
<owner/repo#n>, in planning (Up Next)". Moving the epic to In Progress is
the go: from then its open sub-issues are offered, in their order. The
person's priority list (beekeeper priority, the board's Priority field) comes
first: the candidates are ordered by the rank of their epic, an issue's own
or that of its nearest ranked ancestor through the sub-issue relation, the
unranked after the ranked, each rank in the order above; the pick prints the
rank it applied ("priority 2 of epic <owner/repo#n>", --json priority and
priorityOf). A plan is
the open or merged pull request of plans.repositories whose description
names the epic; its state never gates. The first item that is free
is picked: not served by a running session (a sessions serve record, a busy
agent's task, or else what a session active within overlaps.activeWithin
works on now, its sessions ON refs, "(works on it)", or names by number in
its title or a busy agent's task, "BS 3057 …" or "FIX backstage 3067 …":
a tag its repository's name abbreviates, letter by letter from the first,
or the repository itself, then the number; a busy agent's however
quiet: a role holder's and an idle agent's are left out; one naming a pull request serves the issues it
closes too, In Progress or not) nor by an agent on the roster, its CLI running or not, while it is
busy with its task or parked and kept (agents keep, or an open timer that
wakes it by name), not assigned to anybody outside board.people, without an open recorded
blocker, active within board.staleAfter, and with every lease its labels
name (lease/<resource>, lease/agentlab-1) free or the caller's: an item
whose lease another session holds is passed over until it is released,
the items behind it offered meanwhile. A sub-issue offered through
an epic passes the same checks, and a serve record or task naming
the epic covers it too ("…, on epic owner/repo#n"). No note holds an item:
a hold is a blocked-by (board block), a priority a board rank. A held item,
one with an open recorded blocker, is skipped as "held: blocked by
<owner/repo#n>; 1 of 1 blockers open"; a held epic is never offered through
its sub-issues, and every issue under it is skipped as "held: sub-issue of
<epic>, blocked by …". The epics of the priority list are read whatever their
Team field says when they carry the team label (board.teamLabel), and a
change of a ranked epic's rank or Team beekeeper did not write is a line
"changed outside beekeeper: …" first (--json changed) and a board.changed
event (see priority). A task names
an issue or pull request by its URL, as owner/repo#n, as repo#n of
board.owner, as a repository before a list ("beekeeper: #524, #525") or as
a bare #n after the last repository it named ("beekeeper#173, #176"); a
bare #n before any repository names nothing, and "note #n", "timer #n",
"memo #n" and "decision #n" are beekeeper's own items. It prints each item
above the pick that was skipped on a line of its own, "skipped
<owner/repo#n>: <reason>", the same list as --json's skipped, then the item and why it is picked, without --claim the
free items behind it in their order (the preview of the board's work), and
the skipped items' count by kind: served (a session's record, a busy
agent's task, a running session's work), assigned (outside
board.people), blocked, stale, lease (another session holds it), planning
(the epic gate holds it), unknown (GitHub answered its board fields or an
epic's sub-issues with an error) and order (a rule of board.order turns it
away: an old Backlog item, one in Inbox, one without recorded blockers), and
each epic in planning or past its go with its plan's URL, state and head;
--json lists every item behind the pick as after_pick, each with its skip
reason or free (an empty skip), the counts as skipped_by and the epics with
their plans as epics. The sub-issues are read 150 at a time, under GitHub's
resource limits; whatever GitHub still answers with an error is named, never
read as absent: an item's missing fields as its missing ("<field>:
<message>"), anything else on a line "unknown, GitHub answered with an
error: …" and in --json's missing.

--claim records the pick as the calling session's sessions serve record
under the state lock, after checking again that nobody claimed it since:
two concurrent claims never get the same item. It changes nothing on the
board: the item keeps its Status until the caller, having judged it, moves
it with board move. The claim ends when the session ends, when its agent
reports idle (agents idle) or with sessions unserve <owner/repo#n>. A
second claim while the session still serves an open item is refused with
its record, which stays as it was; --replace takes the next item and
replaces the record. Exit 3 when no item is free or the claim is refused.

--limit <n> lists at most n items of each list: the skipped lines above the
pick and the free items behind it, and --json's skipped and after_pick,
each followed by how many it left out ("… 12 more skipped above the pick",
--json skipped_more and after_pick_more); the counts by kind and the
pick are whole. --fields ref,title,url (implies --json) keeps only the
named fields of each item in pick, skipped and after_pick, a name it does
not know refused with the names it knows. Without either the output is
the whole walk.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if view.limit < 0 {
				return usageErr("--limit %d: a count of items, 0 for all", view.limit)
			}
			if err := checkFields(view.fields); err != nil {
				return usageErr("%v", err)
			}
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
			cl := a.boardClient()
			snap, err := cl.Read(cmd.Context(), a.now)
			if err != nil {
				return err
			}
			changed := a.rankChanges(cmd.Context(), cl, snap.Priorities)
			cands := board.Rank(snap, a.cfg.Board, a.now)
			board.ByPriority(cands, snap.Priorities.Ranks(), snap.Parent)
			listed := time.Now()
			sessions, _, err := a.sessions()
			if err != nil {
				return err
			}
			alive := func(p state.Party) bool {
				return slices.ContainsFunc(sessions, func(s *claude.Session) bool { return s.Party().Is(p) })
			}
			holders, err := lease.Dir(a.cfg.LeaseDir).List()
			if err != nil {
				return err
			}
			skipHeldLeases(cands, holders, me)
			st, err := a.store.Read()
			if err != nil {
				return err
			}
			sc := pickScope{me: me, alive: alive, listed: listed, owner: a.cfg.Board.Owner,
				works: servingWork(st, sessions, a.sessionWorks(sessions), a.now.Add(-a.cfg.Overlaps.ActiveWithin.Duration))}
			if sc.closes, err = cl.Closes(cmd.Context(), offBoard(boardOwners(st, sc), cands)); err != nil {
				return err
			}
			var res nextResult
			if claim {
				open := func(string) bool { return false }
				if !replace {
					if open, err = servedOpen(cmd.Context(), cl, a.store, me); err != nil {
						return err
					}
				}
				res, err = claimNext(a.store, cands, sc, waits, open)
			} else {
				res = nextFree(st, cands, sc)
			}
			if err != nil {
				return err
			}
			res.Epics, res.Missing, res.Changed = snap.Epics(), snap.Missing, changed
			return a.printNext(res, len(cands), view)
		},
	}
	c.Flags().BoolVar(&claim, "claim", false, "record the item as the calling session's (sessions serve), atomically")
	c.Flags().BoolVar(&replace, "replace", false, "with --claim: replace the session's record even while the item it serves is open")
	c.Flags().StringVar(&waits, "waits", "", "what the session waits on, for the record")
	c.Flags().IntVar(&view.limit, "limit", 0, "list at most this many items of each list, saying how many it left out (0: all)")
	c.Flags().StringSliceVar(&view.fields, "fields", nil, "with --json (implied): keep only these fields of each item, comma-separated: "+strings.Join(candidateFields(), ","))
	return c
}

// nextView is how much of a nextResult board next prints: at most limit
// items of each list (0: all), and with fields only those JSON fields of
// each item.
type nextView struct {
	limit  int
	fields []string
}

// firstN is at most n of list (all for 0) and how many it left out.
func firstN[T any](list []T, n int) ([]T, int) {
	if n <= 0 || len(list) <= n {
		return list, 0
	}
	return list[:n], len(list) - n
}

// candidateFields are the JSON names of a board.Candidate's fields, in
// their order, the names --fields takes.
func candidateFields() []string {
	var names []string
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			if f.Anonymous {
				walk(f.Type)
				continue
			}
			if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); name != "" && name != "-" {
				names = append(names, name)
			}
		}
	}
	walk(reflect.TypeFor[board.Candidate]())
	return names
}

// checkFields refuses a field name a candidate has not.
func checkFields(fields []string) error {
	known := candidateFields()
	for _, f := range fields {
		if !slices.Contains(known, f) {
			return fmt.Errorf("--fields %s: no such field, the fields are %s", f, strings.Join(known, ","))
		}
	}
	return nil
}

// projectFields keeps the named fields of c; one c leaves empty is left out,
// as --json leaves it out.
func projectFields(c board.Candidate, fields []string) (map[string]any, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	all := map[string]any{}
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, err
	}
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		if v, ok := all[f]; ok {
			out[f] = v
		}
	}
	return out, nil
}

// nextJSON is --json's document: res with its lists capped by view.limit,
// each item reduced to view.fields when it names any.
func nextJSON(res nextResult, view nextView) (any, error) {
	var skippedMore, afterMore int
	res.Skipped, skippedMore = firstN(res.Skipped, view.limit)
	res.AfterPick, afterMore = firstN(res.AfterPick, view.limit)
	type limited struct {
		nextResult
		SkippedMore   int `json:"skipped_more,omitempty"`
		AfterPickMore int `json:"after_pick_more,omitempty"`
	}
	doc := limited{res, skippedMore, afterMore}
	if len(view.fields) == 0 {
		return doc, nil
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	many := func(cs []board.Candidate) ([]map[string]any, error) {
		ps := make([]map[string]any, 0, len(cs))
		for _, c := range cs {
			p, err := projectFields(c, view.fields)
			if err != nil {
				return nil, err
			}
			ps = append(ps, p)
		}
		return ps, nil
	}
	if res.Pick != nil {
		if out["pick"], err = projectFields(*res.Pick, view.fields); err != nil {
			return nil, err
		}
	}
	for key, cs := range map[string][]board.Candidate{"skipped": res.Skipped, "after_pick": res.AfterPick} {
		if len(cs) == 0 {
			continue
		}
		if out[key], err = many(cs); err != nil {
			return nil, err
		}
	}
	return out, nil
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

// skipHeldLeases marks every free candidate skipped whose labels name a
// lease another party holds; the caller's own lease is no obstacle.
func skipHeldLeases(cands []board.Candidate, holders []lease.Holder, me state.Party) {
	for i := range cands {
		if cands[i].Skip != "" {
			continue
		}
		for _, res := range cands[i].Leases() {
			j := slices.IndexFunc(holders, func(h lease.Holder) bool { return strings.EqualFold(h.Env, res) })
			if j >= 0 && !holders[j].Party().Is(me) {
				cands[i].Skip = fmt.Sprintf("needs lease %s, held by %q", res, holders[j].Name)
				break
			}
		}
	}
}

// pickScope is what a pick reads beside the state: who asks, which sessions
// run and when they were listed, which issues the pull requests close
// (closes, from Client.Closes) and the board's owner, which a repo#n a
// task or note names belongs to, and what the running sessions work on
// (servingWork).
type pickScope struct {
	me     state.Party
	alive  func(state.Party) bool
	listed time.Time
	closes map[string][]string
	owner  string
	works  []sessionWork
}

// sessionWork is the issues and pull requests a running session works on
// now: its current refs, the sessions table's ON, and its title.
type sessionWork struct {
	party state.Party
	refs  []string
	title string
}

// servingWork is what the running sessions work on now (work[i] is
// sessions[i]'s), for board next, its sessions ON: a busy agent's, quiet
// or not, and another session's active since since; a role
// holder's (it reads every item) and an agent's reporting idle (its last
// task's) are left out.
func servingWork(st *state.State, sessions []*claude.Session, work []claude.Work, since time.Time) []sessionWork {
	var out []sessionWork
	for i, s := range sessions {
		p := s.Party()
		holder := slices.ContainsFunc(roles, func(rl role) bool { h := rl.get(st).Holder; return h != nil && h.Is(p) })
		registered := slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(p) })
		if i >= len(work) || holder || registered && agentIdle(st, p) || !registered && s.LastActive.Before(since) {
			continue
		}
		refs := work[i].Refs
		if len(refs) > 0 || titleRef.MatchString(p.Name) {
			out = append(out, sessionWork{party: p, refs: refs[:min(claude.Current, len(refs))], title: p.Name})
		}
	}
	return out
}

// sessionWorks reads what each session's transcript says it works on.
func (a *app) sessionWorks(sessions []*claude.Session) []claude.Work {
	out := make([]claude.Work, len(sessions))
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	for i, s := range sessions {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			if s.Harness == omp.Harness {
				out[i], _ = omp.ReadTranscript(s.Transcript, a.now)
			} else {
				out[i], _ = a.readTranscript(s.Transcript)
			}
		})
	}
	wg.Wait()
	return out
}

// nextResult is the pick, nil when no item is free, the candidates
// skipped above it with the reason, every candidate behind it: with
// its skip reason, or free (an empty skip) and next in line, and the
// skipped candidates counted by the kind of their reason (skipKind).
type nextResult struct {
	Pick    *board.Candidate `json:"pick"`
	Claimed bool             `json:"claimed,omitempty"`
	// Held is the record a claim was refused for: the open item the
	// session serves.
	Held      *state.Record     `json:"held,omitempty"`
	Skipped   []board.Candidate `json:"skipped,omitempty"`
	AfterPick []board.Candidate `json:"after_pick,omitempty"`
	SkippedBy map[string]int    `json:"skipped_by,omitempty"`
	// Epics are the epics the epic gate watches, in planning or past their
	// go, each with its plan.
	Epics []board.Epic `json:"epics,omitempty"`
	// Missing names what GitHub answered with an error and no candidate's
	// missing holds, a search step's issues for one.
	Missing []string `json:"missing,omitempty"`
	// Changed are the changes of a ranked epic's Team or Priority that
	// beekeeper did not write, since the last read of the priority list.
	Changed []string `json:"changed,omitempty"`
}

// The kinds of a skip reason, skipKind's words and skipped_by's keys.
const (
	kindServed   = "served"
	kindAssigned = "assigned"
	kindBlocked  = "blocked"
	kindStale    = "stale"
	kindLease    = "lease"
	kindPlanning = "planning"
	kindUnknown  = "unknown"
	kindOrder    = "order"
)

// skipKind names the kind of a skip reason: who holds the item, or what
// turns it away.
func skipKind(reason string) string {
	switch {
	case strings.HasPrefix(reason, "served by "):
		return kindServed
	case strings.HasPrefix(reason, "assigned to "):
		return kindAssigned
	case strings.HasSuffix(reason, "blockers open"), strings.HasPrefix(reason, board.HeldPrefix), strings.HasPrefix(reason, board.HeldWord):
		return kindBlocked
	case strings.HasPrefix(reason, "no activity since "):
		return kindStale
	case strings.HasPrefix(reason, "needs lease "):
		return kindLease
	case strings.Contains(reason, "in planning ("):
		return kindPlanning
	case strings.HasPrefix(reason, "board fields unknown: "), strings.HasPrefix(reason, "sub-issues unknown: "):
		return kindUnknown
	}
	return kindOrder
}

// skipCounts says how many candidates were skipped, by kind, most first.
func skipCounts(by map[string]int) string {
	kinds := slices.SortedFunc(maps.Keys(by), func(a, b string) int {
		return cmp.Or(cmp.Compare(by[b], by[a]), cmp.Compare(a, b))
	})
	total := 0
	parts := make([]string, 0, len(kinds))
	for _, k := range kinds {
		total += by[k]
		parts = append(parts, fmt.Sprintf("%s %d", k, by[k]))
	}
	return fmt.Sprintf("%d in all: %s", total, strings.Join(parts, ", "))
}

// nextFree walks all the candidates in order and picks the first free one;
// the rest go to AfterPick, each with its skip reason or free.
// An item is owned by a record of a running session (or of one that
// started after sc.listed, the moment the running sessions were listed),
// unless the session is a registered agent reporting idle, by the record
// of an agent kept on the roster (keptBy) or busy with its task on it,
// whether its CLI runs or not, by a
// busy agent whose task names it, else by a running session working on it
// (sc.works), else by a title naming it by number (coverTitled). An issue a
// pull request closes (sc.closes) is owned by whatever owns
// the pull request, and a sub-issue offered through an epic by whatever owns
// the epic.
func nextFree(st *state.State, cands []board.Candidate, sc pickScope) nextResult {
	owners := coverTitled(coverClosed(boardOwners(st, sc), sc.closes), titleOwners(st, sc), cands)
	res := nextResult{SkippedBy: map[string]int{}}
	owned := func(ref string) string {
		o, ok := owners[strings.ToLower(ref)]
		if ok {
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
		if c.Skip != "" {
			res.SkippedBy[skipKind(c.Skip)]++
		}
		switch {
		case res.Pick != nil:
			res.AfterPick = append(res.AfterPick, c)
		case c.Skip == "":
			res.Pick = &c
		default:
			res.Skipped = append(res.Skipped, c)
		}
	}
	if len(res.SkippedBy) == 0 {
		res.SkippedBy = nil
	}
	return res
}

// claimNext picks the next free candidate and records it as me's serve
// in one update of the state, so a concurrent claim sees it. While me
// serves an item open reports open, and me is no agent reporting idle,
// it changes nothing and returns that record as Held.
func claimNext(store state.Store, cands []board.Candidate, sc pickScope, waits string, open func(string) bool) (nextResult, error) {
	me := sc.me
	var res nextResult
	err := store.Update(func(st *state.State) ([]state.Event, error) {
		if i := slices.IndexFunc(st.Records, func(r state.Record) bool {
			return r.Session.Is(me) && r.Ended.IsZero() && open(r.Issue)
		}); i >= 0 && !agentIdle(st, me) {
			res.Held = &st.Records[i]
			return nil, nil
		}
		res = nextFree(st, cands, sc)
		if res.Pick == nil {
			return nil, nil
		}
		r := state.Record{Session: servedBy(st, me), Issue: res.Pick.Ref, Waits: strings.Join(strings.Fields(waits), " "), By: me, At: time.Now().UTC()}
		st.Records = slices.DeleteFunc(st.Records, func(o state.Record) bool { return o.Session.Is(me) })
		st.Records = append(st.Records, r)
		res.Claimed = true
		return []state.Event{about(event(me, verbBoardClaim, "%s: %s (%s)", r.Session.Name, recordText(r), res.Pick.Why()), r.Issue)}, nil
	})
	return res, err
}

// agentIdle reports whether p is a registered agent reporting idle: without
// a task, or done with it.
func agentIdle(st *state.State, p state.Party) bool {
	i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(p) })
	return i >= 0 && (st.Agents[i].Task == "" || st.Agents[i].Done)
}

// namedRef finds what names an issue or pull request in a task or a note,
// as people write them: its URL; owner/repo#n or repo#n; a repository
// before a list, "beekeeper: #524, #525"; a bare #n; and "note #n",
// "timer #n", "memo #n" or "decision #n", beekeeper's own items.
var namedRef = regexp.MustCompile(`(?i)github\.com/([\w.-]+/[\w.-]+)/(?:issues|pull)/(\d+)|\b([a-z][\w.-]*(?:/[\w.-]+)?)(?:#|:\s*#)(\d+)\b|\b(?:notes?|timers?|memos?|decisions?)\s+#\d+\b|#(\d+)\b`)

// listSeparator is what may stand between the items of a list of
// references: commas, semicolons, "and", "or", slashes and short
// parenthesised notes, "#534 (a), #535".
var listSeparator = regexp.MustCompile(`(?i)^(?:\s|[,;&/+]|\band\b|\bor\b|\([^()#]*\))*$`)

// namedRefs are the issues and pull requests text names, lower-cased
// owner/repo#n, each once in the order named: by URL, as owner/repo#n, as
// repo#n of owner (the board's), as a repository before a list, or as a
// bare #n continuing a list after a reference, so "beekeeper#173, #176"
// names two; a bare #n after prose, "gpm#282 is live; tick #37850", names
// nothing, as does one before any repository, and beekeeper's own items
// (note #n) name nothing and end a list.
func namedRefs(text, owner string) []string {
	var out []string
	repo, end := "", 0
	for _, loc := range namedRef.FindAllStringSubmatchIndex(text, -1) {
		m := make([]string, len(loc)/2)
		for i := range m {
			if loc[2*i] >= 0 {
				m[i] = text[loc[2*i]:loc[2*i+1]]
			}
		}
		listed := listSeparator.MatchString(text[end:loc[0]])
		end = loc[1]
		var n string
		switch {
		case m[1] != "":
			repo, n = m[1], m[2]
		case m[3] != "" && (owner != "" || strings.Contains(m[3], "/")):
			repo, n = m[3], m[4]
			if !strings.Contains(repo, "/") {
				repo = owner + "/" + repo
			}
		case m[5] != "" && listed:
			n = m[5]
		case m[5] == "":
			// beekeeper's own item ends a list of references.
			repo = ""
		}
		if n == "" || repo == "" {
			continue
		}
		if ref := strings.ToLower(repo + "#" + n); !slices.Contains(out, ref) {
			out = append(out, ref)
		}
	}
	return out
}

// titleRef is how a desk's task or session title names its item by
// number: a tag or a repository, then the number, "BS 3057 roster read" or
// "FIX backstage 3067 avatars"; a version, "AP 4.141.1 promoter", names
// none.
var titleRef = regexp.MustCompile(`(?i)^(?:fix\s+)?([a-z][\w.-]*)\s+#?(\d+)(?:[\s,:;()]|$)`)

// titled is an item a title names by number (titleRef) and who it names.
type titled struct {
	tag, number, who string
}

// titleOwners are the items the titles of the sessions working name: a
// busy agent's task and name, a running session's title (sc.works).
func titleOwners(st *state.State, sc pickScope) []titled {
	var out []titled
	add := func(title, who string) {
		if m := titleRef.FindStringSubmatch(strings.TrimSpace(title)); m != nil {
			out = append(out, titled{tag: m[1], number: m[2], who: who})
		}
	}
	for _, ag := range st.Agents {
		if ag.Task != "" && !ag.Done {
			add(ag.Task, partyLabel(ag.Party, sc.me)+" (task)")
			add(ag.Name, partyLabel(ag.Party, sc.me)+" (title)")
		}
	}
	for _, w := range sc.works {
		add(w.title, partyLabel(w.party, sc.me)+" (title)")
	}
	return out
}

// coverTitled adds to owners each candidate, and each candidate's epic, a
// title names by its number and a tag its repository's name abbreviates
// (tagNames), owned by who the title names; an owned item keeps its owner.
func coverTitled(owners map[string]string, titles []titled, cands []board.Candidate) map[string]string {
	if len(titles) == 0 {
		return owners
	}
	cover := func(ref string) {
		ref = strings.ToLower(ref)
		repo, n, ok := strings.Cut(ref, "#")
		if !ok || owners[ref] != "" {
			return
		}
		_, name, _ := strings.Cut(repo, "/")
		if i := slices.IndexFunc(titles, func(t titled) bool { return t.number == n && tagNames(t.tag, name) }); i >= 0 {
			owners[ref] = titles[i].who
		}
	}
	for _, c := range cands {
		cover(c.Ref)
		if c.Epic != "" {
			cover(c.Epic)
		}
	}
	return owners
}

// tagNames reports whether tag names the repository name: the name itself,
// or its letters in order from its first, "BS" backstage, "GH" github,
// "ACC" app-catalog-cleanup-tool.
func tagNames(tag, name string) bool {
	tag, name = strings.ToLower(tag), strings.ToLower(name)
	if len(tag) < 2 || name == "" || tag[0] != name[0] {
		return false
	}
	i := 0
	for j := 0; j < len(name) && i < len(tag); j++ {
		if name[j] == tag[i] {
			i++
		}
	}
	return i == len(tag)
}

// partyLabel is how an owner reads: "you" for me, else its quoted name.
func partyLabel(p, me state.Party) string {
	if p.Is(me) {
		return "you"
	}
	return fmt.Sprintf("%q", p.Name)
}

// boardOwners maps each issue (lower-cased owner/repo#n) a live session
// serves to who serves it.
func boardOwners(st *state.State, sc pickScope) map[string]string {
	me, alive, listed := sc.me, sc.alive, sc.listed
	name := func(p state.Party) string { return partyLabel(p, me) }
	// kept says what keeps the record's session on the roster, parked.
	kept := func(p state.Party) string { return rosterKept(st, p, listed) }
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
		for _, ref := range namedRefs(text, sc.owner) {
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
	// Last, what a running session works on with no record naming it.
	for _, w := range sc.works {
		for _, ref := range w.refs {
			if ref = strings.ToLower(ref); out[ref] == "" {
				out[ref] = name(w.party) + " (works on it)"
			}
		}
	}
	return out
}

// offBoard are the refs of owners that are no candidate, sorted: a board
// item is an issue, so the pull requests among what the sessions serve
// and the tasks name are among these, and Closes is asked about
// these only.
func offBoard(owners map[string]string, cands []board.Candidate) []string {
	return slices.DeleteFunc(slices.Sorted(maps.Keys(owners)), func(ref string) bool {
		return slices.ContainsFunc(cands, func(c board.Candidate) bool { return strings.EqualFold(c.Ref, ref) })
	})
}

// coverClosed adds to owners every issue a pull request it names closes,
// owned by whatever owns the pull request "through" it; an issue owned on
// its own keeps its owner.
func coverClosed(owners map[string]string, closes map[string][]string) map[string]string {
	covered := map[string]string{}
	for _, pr := range slices.Sorted(maps.Keys(owners)) {
		for _, issue := range closes[pr] {
			if _, ok := owners[issue]; !ok && covered[issue] == "" {
				covered[issue] = owners[pr] + " through " + pr
			}
		}
	}
	maps.Copy(owners, covered)
	return owners
}

func (a *app) printNext(res nextResult, offered int, view nextView) error {
	if a.json || len(view.fields) > 0 {
		doc, err := nextJSON(res, view)
		if err != nil {
			return err
		}
		if err := a.printJSON(doc); err != nil {
			return err
		}
	} else {
		for _, l := range res.Changed {
			_, _ = fmt.Fprintln(a.out, l)
		}
		if h := res.Held; h != nil {
			_, _ = fmt.Fprintf(a.out, "this session %s: finish it, release it with sessions unserve %s, or claim with --replace\n", recordText(*h), h.Issue)
		}
		// Every skip above the pick comes first, the same list as --json's
		// skipped, so a held item reads apart from an absent one.
		skipped, more := firstN(res.Skipped, view.limit)
		for _, c := range skipped {
			_, _ = fmt.Fprintf(a.out, "skipped %s: %s\n", c.Ref, c.Skip)
		}
		if more > 0 {
			_, _ = fmt.Fprintf(a.out, "… %d more skipped above the pick (--limit %d)\n", more, view.limit)
		}
		if res.Pick != nil {
			p := res.Pick
			_, _ = fmt.Fprintf(a.out, "%s %s\n  %s\n  picked: %s\n", p.Ref, p.Title, p.URL, p.Why())
			if ls := p.Leases(); len(ls) > 0 {
				_, _ = fmt.Fprintf(a.out, "  needs lease %s: free now, claim it on the supervisor's word\n", strings.Join(ls, ", "))
			}
			if res.Claimed {
				_, _ = fmt.Fprintf(a.out, "  claimed: you serve %s now, its Status unchanged (board move %s %q once you take it on; sessions unserve %s releases it)\n", p.Ref, p.Ref, "in progress", p.Ref)
			}
		}
		if n := len(res.AfterPick); n > 0 {
			free := slices.DeleteFunc(slices.Clone(res.AfterPick), func(c board.Candidate) bool { return c.Skip != "" })
			if res.Claimed {
				_, _ = fmt.Fprintf(a.out, "behind it: %d more, %d of them free (--json lists them as after_pick)\n", n, len(free))
			} else {
				// The preview lists the free items in their order.
				_, _ = fmt.Fprintf(a.out, "free behind it, next in line: %d of %d\n", len(free), n)
				w := a.table()
				shown, more := firstN(free, view.limit)
				for _, c := range shown {
					line := fmt.Sprintf("  %s\t%s\t%s", c.Ref, truncate(c.Title, 50), c.Step)
					if p := c.PriorityText(); p != "" {
						line += "\t" + p
					}
					_, _ = fmt.Fprintln(w, line)
				}
				_ = w.Flush()
				if more > 0 {
					_, _ = fmt.Fprintf(a.out, "  … %d more free (--limit %d)\n", more, view.limit)
				}
			}
		}
		if len(res.SkippedBy) > 0 {
			_, _ = fmt.Fprintf(a.out, "skipped: %s (--json lists each with its reason)\n", skipCounts(res.SkippedBy))
		}
		if len(res.Epics) > 0 && !res.Claimed {
			_, _ = fmt.Fprintf(a.out, "epics in planning and past their go: %d\n", len(res.Epics))
			a.printEpics(res.Epics, "  ")
		}
		for _, m := range res.Missing {
			_, _ = fmt.Fprintf(a.out, "unknown, GitHub answered with an error: %s\n", m)
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
		Short: "Set a board item's Status by its canonical name, adding the issue to the board when it is missing",
		Long: `Sets the Status of the issue's board item. An issue without an item on the
board is added first (addProjectV2ItemById, the issue's own mutation: the
board's items are never read) and given board.team as its Team, then the
Status. An issue of a repository outside the board's owner (a personal
account's, another organisation's) is never added: its item on a project
of the repository's own owner (a personal account's user project) moves
there by that project's Status values, without a Team or the epic gate;
on no such project, or on several, it is refused with the gh project
item-add or item-edit recipe. Every GitHub call acts on the
issue's repository (GH_REPO), so a gh link that picks its login by the owner
(devctl's agent link: the App for the organisation, the person's own login
for a personal account's repository) reaches the issue. The status is the
board's value, or an unambiguous part of it, case, emoji and punctuation
ignored ("up next", "progress"); anything else is refused with the board's
values, before anything is written, as is an issue that does not exist.

Moving an epic to In Progress (board.epics' go) is the go: when a plan
pull request names it, the go is recorded as an agent-written comment on
the epic's private thread naming the plan and its head commit, the plan
implemented at that head: the epic itself in a private repository, else
the plan pull request in a private one. An epic of a public repository
whose plan is public too has no private thread: the move is refused before
anything is written. Exit 3 when refused.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, _, _, err := board.Ref(args[0]); err != nil {
				return usageErr("%v", err)
			}
			me, _ := a.caller()
			cl := a.boardClient()
			mv, err := cl.Move(cmd.Context(), args[0], args[1])
			if r := (*board.Refusal)(nil); errors.As(err, &r) {
				return refused("%s", r.Reason)
			}
			if err != nil {
				return err
			}
			if g := mv.Go; g != nil && g.Plan != nil {
				if g.Comment, err = cl.RecordGo(cmd.Context(), g, g.Text(cmp.Or(me.Name, "the person"), mv.To)); err != nil {
					return fmt.Errorf("moved %s to %s, the go not recorded on %s: %w", mv.Ref, mv.To, g.Thread, err)
				}
				_ = a.store.Log(event(me, "board.go", "%s: plan %s at %s, recorded on %s: %s", mv.Ref, g.Plan.URL, g.Plan.Head, g.Thread, g.Comment))
			}
			from := cmp.Or(mv.From, "no Status")
			team := ""
			if mv.Team != "" {
				team = ", Team " + mv.Team
			}
			if mv.Board != "" {
				team = " on the board " + mv.Board
			}
			switch {
			case mv.Added:
				_ = a.store.Log(event(me, "board.move", "%s: added to the board%s, → %s", mv.Ref, team, mv.To))
			case mv.From != mv.To:
				_ = a.store.Log(event(me, "board.move", "%s: %s → %s%s", mv.Ref, from, mv.To, team))
			}
			if a.json {
				return a.printJSON(mv)
			}
			switch {
			case mv.Added:
				_, err = fmt.Fprintf(a.out, "added %s to the board %s/%d%s: → %s\n", mv.Ref, a.cfg.Board.Owner, a.cfg.Board.Project, team, mv.To)
			case mv.From == mv.To:
				_, err = fmt.Fprintf(a.out, "%s is in %s already%s\n", mv.Ref, mv.To, team)
			default:
				_, err = fmt.Fprintf(a.out, "moved %s: %s → %s%s\n", mv.Ref, from, mv.To, team)
			}
			if g := mv.Go; err == nil && g != nil {
				if g.Plan == nil {
					_, err = fmt.Fprintf(a.out, "the go of %s: no plan pull request names it, no plan head recorded\n", mv.Ref)
				} else {
					_, err = fmt.Fprintf(a.out, "the go of %s: plan %s (%s) at %s, recorded on %s: %s\n", mv.Ref, g.Plan.URL, g.Plan.State, g.Plan.Head, g.Thread, g.Comment)
				}
			}
			return err
		},
	}
}

func (a *app) boardRankCmd() *cobra.Command {
	var at board.Place
	c := &cobra.Command{
		Use:   "rank <owner/repo#n|URL> --top | --before <issue> | --after <issue>",
		Short: "Set a board item's priority: move it to the top, or before or after another item",
		Long: `An item's position on the board orders it within its rank of the person's
priority list (beekeeper priority): board next walks the items of each
board.order step in the board's position order, the order a view
without a sort shows and a drag in it changes (the roadmap board's Epics and
Backlogs views). rank moves the issue's item to the top of the board, right
before or right after the item of another issue (updateProjectV2ItemPosition),
and prints the neighbours the move names. An issue without an item on the
board is added first, as move adds it, with board.team as its Team and no
Status; an issue of a repository outside the board's owner is never added.
The other issue must be on the board already. Only the items named are read,
never the board's items. Every GitHub call acts on the repository of the
issue it asks about (GH_REPO). Exit 3 when refused: an issue that does not
exist, an issue outside the board's owner without an item, another issue
that has no item on the board, an item placed against itself.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, r := range []string{args[0], at.Before, at.After} {
				if r == "" {
					continue
				}
				if _, _, _, err := board.Ref(r); err != nil {
					return usageErr("%v", err)
				}
			}
			me, _ := a.caller()
			cl := a.boardClient()
			rk, err := cl.Rank(cmd.Context(), args[0], at)
			if r := (*board.Refusal)(nil); errors.As(err, &r) {
				return refused("%s", r.Reason)
			}
			if err != nil {
				return err
			}
			place := rankPlace(rk)
			added := ""
			if rk.Added {
				added = ", added to the board"
				if rk.Team != "" {
					added += " with Team " + rk.Team
				}
			}
			_ = a.store.Log(event(me, "board.rank", "%s: %s%s", rk.Ref, place, added))
			if a.json {
				return a.printJSON(rk)
			}
			_, err = fmt.Fprintf(a.out, "ranked %s on the board %s/%d: %s%s\n", rk.Ref, a.cfg.Board.Owner, a.cfg.Board.Project, place, added)
			return err
		},
	}
	c.Flags().BoolVar(&at.Top, "top", false, "move the item to the top of the board")
	c.Flags().StringVar(&at.Before, "before", "", "move the item right before this issue's item")
	c.Flags().StringVar(&at.After, "after", "", "move the item right after this issue's item")
	c.MarkFlagsMutuallyExclusive("top", "before", "after")
	c.MarkFlagsOneRequired("top", "before", "after")
	return c
}

// boardHoldCmd is board block, or board unblock when block is false.
func (a *app) boardHoldCmd(block bool) *cobra.Command {
	var by string
	use, short, long := "block", "Hold a board item until another issue closes: record a GitHub blocked-by dependency",
		`Records the issue --by names as a blocker of the item's issue, a GitHub
blocked-by dependency (addBlockedBy): board next skips the item while any of
its recorded blockers is open ("1 of 1 blockers open") and offers it again at
the next board next once they all closed, with no session acting, unless its
Status was set after the last of them closed: that Status holds it until it
changes. A
dependency recorded already is left as it is.`
	if !block {
		use, short, long = "unblock", "Remove a hold from a board item: the GitHub blocked-by dependency block recorded",
			`Removes the issue --by names from the blockers of the item's issue
(removeBlockedBy): board next offers the item again once none of its other
recorded blockers is open. A dependency not recorded is left as it is.`
	}
	c := &cobra.Command{
		Use:   use + " <owner/repo#n|URL> --by <owner/repo#n|URL>",
		Short: short,
		Long: long + `

Only the two issues are read, never the board's items; each is read on its
own repository (GH_REPO), the dependency written on the item's. A hold is on
an issue: a blocker that is a pull request, or that GitHub does not resolve,
is refused naming why before anything is written, as is an item that is no
issue and an item held by itself. A hold on something that is no issue (a
person's review or answer) is the item's Status Blocked / Waiting plus a
decision for that person. Exit 3 when refused.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, r := range []string{args[0], by} {
				if _, _, _, err := board.Ref(r); err != nil {
					return usageErr("%v", err)
				}
			}
			me, _ := a.caller()
			cl := a.boardClient()
			hold := cl.Unblock
			if block {
				hold = cl.Block
			}
			h, err := hold(cmd.Context(), args[0], by)
			if r := (*board.Refusal)(nil); errors.As(err, &r) {
				return refused("%s", r.Reason)
			}
			if err != nil {
				return err
			}
			if h.Changed {
				_ = a.store.Log(event(me, "board."+use, "%s: by %s (%s), %s", h.Ref, h.By, openWord(h.ByOpen), holdState(h)))
			}
			if a.json {
				return a.printJSON(h)
			}
			_, err = fmt.Fprintln(a.out, holdText(h, block))
			return err
		},
	}
	c.Flags().StringVar(&by, "by", "", "the blocking issue, owner/repo#n or its URL")
	_ = c.MarkFlagRequired("by")
	return c
}

// holdText says what board block (block) or unblock did and what board
// next makes of the item now.
func holdText(h *board.Held, block bool) string {
	var did string
	switch {
	case block && h.Changed:
		did = fmt.Sprintf("blocked %s by %s (%s)", h.Ref, h.By, openWord(h.ByOpen))
	case block:
		did = fmt.Sprintf("%s is blocked by %s (%s) already", h.Ref, h.By, openWord(h.ByOpen))
	case h.Changed:
		did = fmt.Sprintf("unblocked %s: %s (%s) no longer blocks it", h.Ref, h.By, openWord(h.ByOpen))
	default:
		did = fmt.Sprintf("%s is not blocked by %s (%s)", h.Ref, h.By, openWord(h.ByOpen))
	}
	return did + ": " + holdState(h)
}

// holdState is what board next makes of a held item's blockers: skipped
// while one is open, held by none once all closed.
func holdState(h *board.Held) string {
	if h.OpenBlockers > 0 {
		return fmt.Sprintf("%d of %d blockers open, board next skips it until they close", h.OpenBlockers, h.Blockers)
	}
	return fmt.Sprintf("%d of %d blockers open, none holds it from board next", h.OpenBlockers, h.Blockers)
}

func openWord(open bool) string {
	if open {
		return "open"
	}
	return "closed"
}

// printEpics prints one line per epic: its status and its plan.
func (a *app) printEpics(epics []board.Epic, indent string) {
	for _, e := range epics {
		_, _ = fmt.Fprintf(a.out, "%s%s %s (%s): %s\n", indent, e.Ref, truncate(e.Title, 50), e.Status, e.PlanText())
	}
}

// rankPlace is where rk put the item, with the neighbours it names.
func rankPlace(rk *board.Ranked) string {
	switch {
	case rk.Top && rk.Below != "":
		return "at the top, above " + rk.Below
	case rk.Top:
		return "at the top"
	case rk.Above != "":
		return "after " + rk.Above
	default:
		return "before " + rk.Below
	}
}
