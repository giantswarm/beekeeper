package cmd

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/transcripts"
)

// sessionView is a session with what the state and its transcript add.
type sessionView struct {
	*claude.Session
	Work   claude.Work `json:"work"`
	Role   string      `json:"role,omitempty"`
	Leases []string    `json:"leases,omitempty"`
	// Serves is the session's record: the issue it serves, what it waits on.
	Serves *state.Record `json:"serves,omitempty"`
	// Metrics are how it has been doing: read with its work.
	Metrics *metrics `json:"metrics,omitempty"`
	// Self marks the session that runs this command.
	Self bool `json:"self,omitempty"`
}

// view is everything the session-level commands show.
type view struct {
	Sessions []*sessionView   `json:"sessions"`
	Totals   *metricsTotals   `json:"totals,omitempty"`
	Overlaps []claude.Overlap `json:"overlaps,omitempty"`
	// SupervisorLoop is the PID of the running supervisor loop, 0 when it
	// does not run.
	SupervisorLoop int         `json:"supervisorLoopPID,omitempty"`
	Leases         []leaseView `json:"leases,omitempty"`
	raw            []*claude.Session
	st             *state.State
}

// roleView is a relayed role's holder (the guide's) as the commands show
// it.
type roleView struct {
	state.Supervisor
	Live bool `json:"live"`
	// CLIGone is set while its CLI is gone, RestartUntil while it may still
	// come back as a restart.
	CLIGone      time.Time `json:"cliGone,omitzero"`
	RestartUntil time.Time `json:"restartUntil,omitzero"`
	// Relay is the role's last relay; Relieved says it relieved the caller.
	Relay    *state.Relay `json:"relay,omitempty"`
	Relieved bool         `json:"relieved,omitempty"`
	// Run is the role's run the holder is: "<Role> run <Run>".
	Run int `json:"run,omitempty"`
	// Context is its session's context in tokens (0: not read), RelayAt
	// the context at which the watch reports the relay due.
	Context int64 `json:"contextTokens,omitempty"`
	RelayAt int64 `json:"relayAt"`
}

// viewRole is rl's recorded holder r as sv reads it, with its session's
// context; nil when none is recorded.
func (a *app) viewRole(rl role, r state.Role, sessions []*claude.Session, sv supervision) *roleView {
	if r.Holder == nil {
		return nil
	}
	return &roleView{
		Supervisor: *r.Holder, Live: sv.live, CLIGone: sv.gone, RestartUntil: sv.until, Relay: r.Relay,
		Context: sessionContext(sessions, r.Holder.Party), RelayAt: int64(rl.cfg(a.cfg).RelayAt), Run: r.Run,
	}
}

// contextText says how far the holder's context is towards relayAt,
// "" when it is not known.
func (v *roleView) contextText() string {
	if v.Context == 0 {
		return ""
	}
	return fmt.Sprintf(", %s tokens of context (relay at %s)", tokensText(v.Context), tokensText(v.RelayAt))
}

// collect reads processes, sessions, state and leases; withWork also scans
// the transcripts for what each session is on and how it has been doing.
func (a *app) collect(withWork bool) (*view, error) {
	raw, t, err := a.sessions()
	if err != nil {
		return nil, err
	}
	st, err := a.store.Read()
	if err != nil {
		return nil, err
	}
	holders, err := lease.Dir(a.cfg.LeaseDir).List()
	if err != nil {
		return nil, err
	}
	v := &view{raw: raw, st: st}
	v.SupervisorLoop = a.loopPID()
	for _, h := range holders {
		v.Leases = append(v.Leases, a.leaseView(raw, h))
	}
	work := map[int]claude.Work{}
	var works []claude.Work
	var ms []*metrics
	if withWork {
		works, ms = a.sessionMetrics(raw, t, holders)
		v.Totals = totals(raw, ms)
	}
	self := envParty(os.Getenv)
	for i, s := range raw {
		sv := &sessionView{Session: s, Self: isSelf(self, s)}
		sv.Role = roleOf(st, s)
		if i := slices.IndexFunc(st.Records, func(r state.Record) bool { return r.Session.Is(s.Party()) }); i >= 0 {
			sv.Serves = &st.Records[i]
		}
		if withWork {
			sv.Work, sv.Metrics = works[i], ms[i]
			if sv.Serves != nil && sv.Serves.Ended.IsZero() {
				sv.Work = sv.Work.Serve(sv.Serves.Issue)
			}
			work[s.PID] = sv.Work
		}
		for _, h := range holders {
			if h.Party().Is(s.Party()) {
				sv.Leases = append(sv.Leases, h.Env)
			}
		}
		v.Sessions = append(v.Sessions, sv)
	}
	v.attributeBrowseSessions()
	slices.SortFunc(v.Sessions, func(x, y *sessionView) int { return y.LastActive.Compare(x.LastActive) })
	if withWork {
		v.Overlaps = claude.Overlaps(raw, work, claude.OverlapOptions{
			Ignore:      a.cfg.Overlaps.Ignore,
			ActiveSince: a.now.Add(-a.cfg.Overlaps.ActiveWithin.Duration),
		})
	}
	return v, nil
}

// attributeBrowseSessions uses browse's explicit start record: its CLI has
// intentionally shed the caller's environment and may run outside a checkout.
func (v *view) attributeBrowseSessions() {
	for _, s := range v.Sessions {
		for _, start := range v.st.Starts {
			if start.Name != browseStart || !start.Is(s.Party()) {
				continue
			}
			s.Parent = cmp.Or(start.By.Session, start.By.HostSession)
			parent := &claude.Session{ID: start.By.Session, HostID: start.By.HostSession}
			if s.Role == "" {
				s.Role = roleOf(v.st, parent)
			}
			for _, p := range v.Sessions {
				if p == s || !start.By.Is(p.Party()) {
					continue
				}
				if s.Repo == "" {
					s.Repo = p.Repo
				}
				if len(s.Work.Repos) == 0 {
					s.Work.Repos = slices.Clone(p.Work.Repos)
				}
				if len(s.Work.Refs) == 0 {
					s.Work.Refs = slices.Clone(p.Work.Refs)
				}
				break
			}
			break
		}
	}
}

func roleOf(st *state.State, s *claude.Session) string {
	for _, rl := range roles {
		if h := rl.get(st).Holder; h != nil && h.Is(s.Party()) {
			return rl.name
		}
	}
	for _, ag := range st.Agents {
		if ag.Is(s.Party()) {
			if ag.Task != "" {
				return "agent: " + ag.Task
			}
			return "agent (idle)"
		}
	}
	return ""
}

func (a *app) sessionsCmd() *cobra.Command {
	var all, full bool
	var by, day, tz string
	c := &cobra.Command{
		Use:   "sessions",
		Short: "List the running sessions: what each is on, what it runs, what it holds",
		Long: `List the running sessions across Claude Code, omp (oh-my-pi), agy
(Antigravity) and codex, most recently active first. Non-Claude sessions
are marked with their harness and state (for example, "omp busy" or
"omp idle"). Each session shows the
issue its record serves and the issues or pull requests its latest turns
acted on (named in its gh and devctl commands and GitHub tool calls, or
created; a ref it only quoted or read is no act), else the repository it
acted in or its checkout, when it was last active, the tool commands it runs right now (a devctl wait, a
bounded sleep with the time left; the caller's own session
"(this session)", so a grep over the listing never matches its own
command), its memory, how full its context is,
its last hour (turns, tool calls and their errors, GitHub calls, cost),
and its role and leases, after "archived" or "test" for a session the
guide's feed leaves out. A
session another session started (a claude -p or claude --bg worker with an
id of its own) is listed under its own id and name, started by that session;
a claude --bg daemon is no session.

The figures come from the last 512 KiB of each transcript, the process
table, the memcap scopes, the event log and the leases, never from GitHub;
--json has them all per session (over the transcript window and the last
hour: busy time, turns, tool calls, errors and the most repeated failing
call, GitHub calls, tokens and cost, context; idle time, the capped runs'
memory, gh and devctl processes, merges, lease times) and their totals.
Cost is priced from metrics.models; a model without a price is "cost
unknown".

Overlaps name the issues, pull requests and repositories more than one
session acts on now. --json keeps the refs a session only mentioned in
its work's "mentioned". --all adds the sessions of the last 24 hours that run no
Claude Code CLI (paused or closed, not archived) and the omp, agy and codex
sessions no process runs:
a message to them does not arrive.

The session records (sessions serve) follow the table: which session serves
which issue and what it waits on, then the parked ones, then the records
whose session has ended. A parked record is a worker's that runs no CLI
while it stays on the roster parked (agents park) or kept (agents keep, a
timer that wakes it): it still serves its issue or pull request, and its
line names the worker and its park reason. This is the served-check: an
issue or pull request is served while a running or parked record names it.
--json carries the parked ones in "parked" (session, issue, waits, parked,
by, at); board next skips the same ones.

A caller that has read the list before gets only what changed since: a
session that started, ended or changed what it is on, runs or holds, a
record or an overlap that came or went (an age, a memory size or a context
moving is no change), or one "no change" line. --full prints everything.

--by kind prints, instead of the list, the day's figures per task kind of
agents.kinds (agents by name regexp; a name several kinds match counts in
each): its starts recorded that day, its sessions with a request that day,
their hours (each from its first to its last request of the day) and their
spend, priced as beekeeper spend prices it from the whole transcripts,
subagents included. --day names the day (2006-01-02, default today) in
--tz. A Claude Code transcript names its session; one of no recorded start
of a kind's agent, or of a harness whose files name no session beekeeper
recorded (omp, agy, codex), counts in "of no kind".`,
		Example: `  beekeeper sessions --by kind                 # today, per task kind
  beekeeper sessions --by kind --day 2026-11-02 --json`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			switch by {
			case "":
			case paramKind:
				return a.sessionsByKind(day, tz)
			default:
				return usageErr("--by %q: want kind", by)
			}
			v, err := a.collect(true)
			if err != nil {
				return err
			}
			var paused []*claude.Record
			var ended []*claude.Session
			if all {
				since := a.now.Add(-24 * time.Hour)
				paused = claude.StoppedRecords(a.cfg, v.raw, since)
				for _, h := range harnesses {
					ended = append(ended, h.Ended(a.cfg, v.raw, since)...)
				}
			}
			if a.json {
				return a.printJSON(struct {
					*view
					Records []state.Record    `json:"records,omitempty"`
					Parked  []parkedRow       `json:"parked"`
					Paused  []*claude.Record  `json:"paused,omitempty"`
					Ended   []*claude.Session `json:"ended,omitempty"`
				}{v, v.st.Records, parkedRows(v.st, v.st.Records, v.raw, a.now), paused, ended})
			}
			printFull := func() {
				a.printSessions(v)
				if len(v.st.Records) > 0 {
					_, _ = fmt.Fprintf(a.out, "\nSession records:\n")
					a.printRecords(v.st, v.st.Records, v.raw)
				}
				if len(paused)+len(ended) > 0 {
					_, _ = fmt.Fprintf(a.out, "\nNot running (paused or closed):\n")
					w := a.table()
					for _, r := range paused {
						_, _ = fmt.Fprintf(w, "  %s\t%s\tactive %s ago\t%s\n", truncate(r.Title, 50), r.Branch,
							ago(a.now, time.UnixMilli(r.LastActivityAt)), r.Aside())
					}
					for _, e := range ended {
						_, _ = fmt.Fprintf(w, "  %s\t%s\tactive %s ago\t%s\n", truncate(e.Name, 50), e.Cwd, ago(a.now, e.LastActive), cmp.Or(e.Harness, omp.Harness))
					}
					_ = w.Flush()
				}
			}
			facts := a.sessionFacts(v)
			for _, r := range paused {
				l := fmt.Sprintf("paused %q %s", r.Title, r.Branch)
				facts = append(facts, fact{Key: l, Sig: l, Line: l})
			}
			for _, e := range ended {
				l := fmt.Sprintf("ended %s %q %s", cmp.Or(e.Harness, omp.Harness), e.Name, e.Cwd)
				facts = append(facts, fact{Key: l, Sig: l, Line: l})
			}
			return a.delta("sessions", full, facts, printFull)
		},
	}
	c.Flags().BoolVar(&all, "all", false, "also list the sessions of the last 24 hours that run no CLI")
	c.Flags().StringVar(&by, "by", "", "kind: the day's starts, hours and spend per task kind (agents.kinds) instead of the list")
	c.Flags().StringVar(&day, "day", "", "with --by kind: the day (2006-01-02); default today")
	c.Flags().StringVar(&tz, "tz", "", "with --by kind: the time zone of --day and of the output (IANA); default the machine's")
	fullFlag(c, &full)
	c.AddCommand(a.serveCmd(), a.unserveCmd())
	return c
}

// issueRef is an issue or pull request, owner/repo#n.
var issueRef = regexp.MustCompile(`^[\w.-]+/[\w.-]+#[0-9]+$`)

func (a *app) serveCmd() *cobra.Command {
	var waits string
	var clearOwn bool
	c := &cobra.Command{
		Use:   "serve <session> <item> | serve --clear",
		Short: "Record the item a session serves and what it waits on",
		Long: `Record, for any running session (a registered agent or not), the item it
serves and what it waits on: an issue or epic (owner/repo#n), a repository
(owner/repo), or quoted free text. A new record replaces the session's
last one. sessions and the hand-over show it, and beekeeper watch prints one
line when the session ends, naming the issue to re-query. The session is a
session id, a PID, a session's own name or a registered agent's roster name
or task title (the roster name with a suffix such as " (opus, builds)"), or
a unique part of a name; a whole name wins over a part of another. A name
that matches nothing is refused with the closest roster names. A
registered agent whose CLI does not run is recorded too, and the line says
so. The record stores the roster name of a registered agent, the session's
own name otherwise.

A worker (a registered agent holding no guide role) records
only its own session: a serve naming another session is refused. --clear
removes the calling session's own record; agents idle --done clears it too.`,
		Args: func(c *cobra.Command, args []string) error {
			if clearOwn {
				return cobra.NoArgs(c, args)
			}
			return cobra.ExactArgs(2)(c, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			defer a.warnClaims(cmd.Context(), cmd.ErrOrStderr())
			if clearOwn {
				return a.clearRecord()
			}
			if strings.TrimSpace(args[1]) == "" {
				return usageErr("served item must not be empty")
			}
			me, err := a.caller()
			if err != nil {
				return err
			}
			sessions, _, err := a.sessions()
			if err != nil {
				return err
			}
			s, absent, err := a.namedSession(sessions, args[0], a.now)
			if err != nil {
				return err
			}
			r := state.Record{Issue: args[1], Waits: strings.Join(strings.Fields(waits), " "), By: me, At: a.now.UTC()}
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				r.Session = servedBy(st, s.Party())
				if rosterRole(st, me) == workerRole && !r.Session.Is(me) {
					return nil, refused("%q is another session: a worker records only its own, `beekeeper sessions serve <its own name> %s`", r.Session.Name, r.Issue)
				}
				st.Records = slices.DeleteFunc(st.Records, func(o state.Record) bool { return o.Session.Is(r.Session) })
				st.Records = append(st.Records, r)
				return []state.Event{about(event(me, verbServe, "%s: %s", r.Session.Name, recordText(r)), r.Issue)}, nil
			})
			if err != nil {
				return err
			}
			a.warnClaimedElsewhere(cmd.Context(), cmd.ErrOrStderr(), r.Issue)
			line := fmt.Sprintf("%q %s", r.Session.Name, recordText(r))
			if absent != "" {
				line += "; " + absent
			}
			_, err = fmt.Fprintln(a.out, line)
			return err
		},
	}
	c.Flags().StringVar(&waits, "waits", "", "what the session waits on")
	c.Flags().BoolVar(&clearOwn, "clear", false, "remove the calling session's own record")
	c.MarkFlagsMutuallyExclusive("clear", "waits")
	return c
}

// clearRecord removes the calling session's own records.
func (a *app) clearRecord() error {
	me, err := a.caller()
	if err != nil {
		return err
	}
	var cleared []state.Record
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		cleared = clearRecords(st, me)
		evs := make([]state.Event, len(cleared))
		for i, r := range cleared {
			evs[i] = event(me, "session.unserve", "%s: %s", r.Session.Name, recordText(r))
		}
		return evs, nil
	})
	if err != nil {
		return err
	}
	if len(cleared) == 0 {
		_, err = fmt.Fprintf(a.out, "%q has no record to clear\n", me.Name)
		return err
	}
	_, err = fmt.Fprintf(a.out, "cleared the record of %q: it %s\n", cleared[0].Session.Name, recordText(cleared[0]))
	return err
}

// clearRecords removes the records of session p and returns them.
func clearRecords(st *state.State, p state.Party) []state.Record {
	var gone []state.Record
	st.Records = slices.DeleteFunc(st.Records, func(r state.Record) bool {
		if r.Session.Is(p) {
			gone = append(gone, r)
			return true
		}
		return false
	})
	return gone
}

func (a *app) unserveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unserve <session|owner/repo#n>",
		Short: "Remove a session's record: its work is done or handed on",
		Long: `Remove a session's record, running or ended. The session is a name, a
unique part of one or a session id; an issue, owner/repo#n, removes the one
record serving it (a board next --claim given back).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			defer a.warnClaims(cmd.Context(), cmd.ErrOrStderr())
			me, err := a.caller()
			if err != nil {
				return err
			}
			var r state.Record
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				i, err := findRecord(st.Records, args[0])
				if err != nil {
					return nil, err
				}
				r = st.Records[i]
				st.Records = slices.Delete(st.Records, i, i+1)
				return []state.Event{event(me, "session.unserve", "%s: %s", r.Session.Name, recordText(r))}, nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.out, "removed the record of %q\n", r.Session.Name)
			return err
		},
	}
}

// findRecord is the index of the record of the session q names, or of the
// one record serving q when it is an issue.
func findRecord(records []state.Record, q string) (int, error) {
	if !issueRef.MatchString(q) {
		parties := make([]state.Party, len(records))
		for i, r := range records {
			parties[i] = r.Session
		}
		return findParty(parties, q, "session record", "session records")
	}
	var names []string
	i := -1
	for j, r := range records {
		if strings.EqualFold(r.Issue, q) {
			i = j
			names = append(names, strconv.Quote(r.Session.Name))
		}
	}
	switch len(names) {
	case 0:
		return -1, refused("no session record serves %s", q)
	case 1:
		return i, nil
	}
	return -1, refused("%s is served by %s: name the session", q, strings.Join(names, ", "))
}

// sessionNamed is the session q names, by the one rule of the commands that
// take a session: a session id, host id or PID, else a name matched whole
// (ignoring case) before a unique part of one, where a running session's
// names are its own (its desktop title or process name) and, for a
// registered agent, its roster name. A registered agent whose CLI does not
// run resolves to its roster entry, and absent says so. A name several
// sessions carry is the registered agent's of that roster name: a hand-over's
// follow-up, not the session it replaces, which keeps the name until it ends.
func sessionNamed(sessions []*claude.Session, st *state.State, q string, now time.Time) (to *claude.Session, absent string, err error) {
	type target struct {
		s      *claude.Session
		names  []string
		absent string
		// roster is its roster name, "" for a session off the roster.
		roster string
		// title is its task's title (agents start's task line before its
		// first ": "), "" without one.
		title string
	}
	targets := make([]*target, 0, len(sessions)+len(st.Agents))
	for _, s := range sessions {
		targets = append(targets, &target{s: s, names: []string{s.Name}})
	}
	for _, ag := range st.Agents {
		if i := slices.IndexFunc(sessions, func(s *claude.Session) bool { return ag.Is(s.Party()) }); i >= 0 {
			targets[i].names = append(targets[i].names, ag.Name)
			targets[i].roster, targets[i].title = ag.Name, taskTitle(ag.Task)
			continue
		}
		targets = append(targets, &target{
			s:      &claude.Session{ID: ag.Session, HostID: ag.HostSession, Name: ag.Name},
			names:  []string{ag.Name},
			absent: noCLI(ag, now),
			roster: ag.Name,
			title:  taskTitle(ag.Task),
		})
	}
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, "", refused("no session named")
	}
	lq := strings.ToLower(q)
	// bare is q without a task title's parenthesized suffix, which names
	// the roster agent even after its task changed.
	bare := strings.TrimSpace(titleSuffix.ReplaceAllString(q, ""))
	rosterIs := func(t *target) bool {
		return t.roster != "" && (strings.EqualFold(t.roster, q) || strings.EqualFold(t.roster, bare) || strings.EqualFold(t.title, q))
	}
	var whole, part []*target
	for _, t := range targets {
		if t.s.ID == q || t.s.HostID == q || t.s.PID != 0 && strconv.Itoa(t.s.PID) == q {
			return t.s, t.absent, nil
		}
		switch {
		case rosterIs(t) || slices.ContainsFunc(t.names, func(n string) bool { return strings.ToLower(n) == lq }):
			whole = append(whole, t)
		case slices.ContainsFunc(t.names, func(n string) bool { return strings.Contains(strings.ToLower(n), lq) }):
			part = append(part, t)
		}
	}
	hits := whole
	if agents := slices.DeleteFunc(slices.Clone(whole), func(t *target) bool { return !rosterIs(t) }); len(whole) > 1 && len(agents) == 1 {
		hits = agents
	}
	if len(hits) == 0 {
		hits = part
	}
	switch len(hits) {
	case 0:
		why := fmt.Sprintf("no running session or registered agent matches %q: name one by its session name, roster name or session id", q)
		if near := closestRoster(st.Agents, bare, 3); len(near) > 0 {
			why += "; the closest roster names: " + strings.Join(near, ", ")
		}
		return nil, "", refused("%s", why)
	case 1:
		return hits[0].s, hits[0].absent, nil
	}
	named := make([]string, len(hits))
	for i, t := range hits {
		named[i] = fmt.Sprintf("%q (%s)", t.s.Name, t.s.ID)
	}
	slices.Sort(named)
	return nil, "", refused("%q matches %d sessions: %s; pass one's session id", q, len(hits), strings.Join(named, ", "))
}

// titleSuffix is a task title's parenthesized suffix, " (opus, builds)".
var titleSuffix = regexp.MustCompile(`\s*\([^()]*\)\s*$`)

// taskTitle is the title of an agent's task: agents start's task line up to
// its first ": ".
func taskTitle(task string) string {
	title, _, _ := strings.Cut(task, ": ")
	return strings.TrimSpace(title)
}

// closestRoster is up to n roster names nearest to q by edit distance,
// ignoring case, nearest first, quoted.
func closestRoster(agents []state.Agent, q string, n int) []string {
	type near struct {
		name string
		d    int
	}
	lq := []rune(strings.ToLower(q))
	ranked := make([]near, 0, len(agents))
	for _, ag := range agents {
		if ag.Name != "" {
			ranked = append(ranked, near{ag.Name, editDistance(lq, []rune(strings.ToLower(ag.Name)))})
		}
	}
	slices.SortStableFunc(ranked, func(a, b near) int { return cmp.Or(cmp.Compare(a.d, b.d), strings.Compare(a.name, b.name)) })
	out := make([]string, 0, n)
	for _, r := range ranked[:min(n, len(ranked))] {
		out = append(out, strconv.Quote(r.name))
	}
	return out
}

// editDistance is the Levenshtein distance between a and b.
func editDistance(a, b []rune) int {
	row := make([]int, len(b)+1)
	for j := range row {
		row[j] = j
	}
	for i := range a {
		prev := row[0]
		row[0] = i + 1
		for j := range b {
			cur, swap := row[j+1], prev
			if a[i] != b[j] {
				swap++
			}
			row[j+1] = min(row[j+1]+1, row[j]+1, swap)
			prev = cur
		}
	}
	return row[len(b)]
}

// servedBy is the session p as a serve record names it: under its roster
// name when it is a registered agent, else under its own name (a desktop
// title, or the process name of a session the desktop has not titled).
func servedBy(st *state.State, p state.Party) state.Party {
	if i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(p) }); i >= 0 {
		p.Name = st.Agents[i].Name
	}
	return p
}

// rosterRole is the role st says me holds ("the guide", "the worker"), ""
// when it says none.
func rosterRole(st *state.State, me state.Party) string {
	switch {
	case guideRole.get(st).Holder != nil && guideRole.get(st).Holder.Is(me):
		return guideRoleAlias
	case slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(me) }):
		return workerRole
	}
	return ""
}

// recordText says what a record's session serves.
func recordText(r state.Record) string {
	s := "serves " + r.Issue
	if r.Waits != "" {
		s += ", waiting on " + r.Waits
	}
	return s
}

// parkedRow is a session record whose session runs no CLI while its agent
// stays on the roster, parked or kept: the work it serves is still served.
// Parked says what keeps it (agents park, agents keep, a timer that wakes
// it).
type parkedRow struct {
	Session state.Party `json:"session"`
	Issue   string      `json:"issue"`
	Waits   string      `json:"waits,omitempty"`
	Parked  string      `json:"parked"`
	By      state.Party `json:"by"`
	At      time.Time   `json:"at"`
}

// parkedRows are the parked rows among records at now.
func parkedRows(st *state.State, records []state.Record, sessions []*claude.Session, now time.Time) []parkedRow {
	rows := []parkedRow{}
	for _, r := range records {
		if _, live := claude.Live(sessions, r.Session); live && r.Ended.IsZero() {
			continue
		}
		if k := rosterKept(st, r.Session, now); k != "" {
			rows = append(rows, parkedRow{Session: r.Session, Issue: r.Issue, Waits: r.Waits, Parked: k, By: r.By, At: r.At})
		}
	}
	return rows
}

// text says what keeps the row's session and what it serves; the waits the
// park already says are not said twice.
func (p parkedRow) text() string {
	serves := recordText(state.Record{Issue: p.Issue, Waits: p.Waits})
	if p.Waits != "" && strings.HasSuffix(p.Parked, ": "+p.Waits) {
		serves = "serves " + p.Issue
	}
	return fmt.Sprintf("parked, no CLI, %s; it %s", p.Parked, serves)
}

// printRecords lists the session records: the running sessions' first,
// then the parked ones (parkedRows), then those whose session has ended.
func (a *app) printRecords(st *state.State, records []state.Record, sessions []*claude.Session) {
	if len(records) == 0 {
		_, _ = fmt.Fprintln(a.out, "no session records")
		return
	}
	parked := parkedRows(st, records, sessions, a.now)
	var ended []string
	for _, r := range records {
		if _, live := claude.Live(sessions, r.Session); live && r.Ended.IsZero() {
			_, _ = fmt.Fprintf(a.out, "- %q %s\n", r.Session.Name, recordText(r))
			continue
		}
		if slices.ContainsFunc(parked, func(p parkedRow) bool { return p.Session.Is(r.Session) }) {
			continue
		}
		when := "has ended"
		if !r.Ended.IsZero() {
			when = "ended at " + clock(a.now, r.Ended)
		}
		ended = append(ended, fmt.Sprintf("- %q %s: it %s; re-query %s", r.Session.Name, when, recordText(r), r.Issue))
	}
	for _, p := range parked {
		_, _ = fmt.Fprintf(a.out, "- %q %s\n", p.Session.Name, p.text())
	}
	for _, l := range ended {
		_, _ = fmt.Fprintln(a.out, l)
	}
}

func (a *app) printSessions(v *view) {
	w := a.table()
	_, _ = fmt.Fprintln(w, "SESSION\tON\tACTIVE\tRUNNING\tMEM\tCTX\tMODEL\tLAST HOUR\tROLE / LEASES")
	for _, s := range v.Sessions {
		role := s.Role
		if role == "" && s.Parent != "" {
			role = "started by " + parentName(v.raw, s.Parent)
		}
		hour := "-"
		if s.Metrics != nil {
			hour = hourText(s.Metrics.LastHour)
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%dM\t%s\t%s\t%s\t%s\n",
			truncate(s.Name, 44), truncate(on(s), 44), ago(a.now, s.LastActive),
			truncate(s.running(), 40), s.MemMiB, contextText(s.Metrics), cmp.Or(truncate(s.Model, 32), "-"), hour, truncate(roleText(s, role), 40))
	}
	_ = w.Flush()
	if t := v.Totals; t != nil {
		_, _ = fmt.Fprintf(a.out, "\nLast hour, all sessions: %s; %d gh/devctl processes now\n", hourText(t.LastHour), t.GitHubProcesses)
	}
	if len(v.Overlaps) > 0 {
		_, _ = fmt.Fprintln(a.out, "\nOverlaps:")
		for _, o := range v.Overlaps {
			_, _ = fmt.Fprintf(a.out, "  %s: %s\n", o.Key, strings.Join(o.Sessions, ", "))
		}
	}
}

// roleText is role and the session's leases, after an omp session's
// harness and state and "archived" or "test" for a session the guide's
// feed leaves out.
func roleText(s *sessionView, role string) string {
	if len(s.Leases) > 0 {
		role = strings.TrimPrefix(role+" holds "+strings.Join(s.Leases, ","), " ")
	}
	return strings.Join(strings.Fields(strings.Join([]string{s.Harness, s.State, s.Aside(), role}, " ")), " ")
}

// parentName names the session that started a child session: its name
// while it runs, else its id.
func parentName(sessions []*claude.Session, id string) string {
	if s, ok := claude.Live(sessions, state.Party{Session: id}); ok {
		return fmt.Sprintf("%q", s.Name)
	}
	return id
}

// on is the repository and refs a session is on, or its checkout.
func on(s *sessionView) string {
	var parts []string
	if len(s.Work.Refs) > 0 {
		parts = append(parts, s.Work.Refs[:min(claude.Current, len(s.Work.Refs))]...)
	} else if len(s.Work.Repos) > 0 {
		parts = append(parts, s.Work.Repos[0])
	}
	if len(parts) == 0 {
		repo := s.Repo
		if s.Branch != "" {
			repo += "@" + s.Branch
		}
		parts = append(parts, repo)
	}
	return strings.Join(parts, " ")
}

// isSelf says s is the session the environment self names: by its ids, never
// by a name alone.
func isSelf(self state.Party, s *claude.Session) bool {
	return (self.Session != "" || self.HostSession != "") && self.Is(s.Party())
}

// selfRunning stands for the caller's own tool commands: they include the
// listing itself and whatever reads it, so a text match over the listing
// would find its own pattern there.
const selfRunning = "(this session)"

// running summarizes the session's tool commands, selfRunning for the
// caller's own.
func (s *sessionView) running() string {
	if s.Self {
		return selfRunning
	}
	return running(s.Commands)
}

// running summarizes the tool commands of a session.
func running(cmds []claude.Command) string {
	if len(cmds) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(cmds))
	for _, c := range cmds {
		s := commandName(c.Args)
		if c.Remaining > 0 {
			s += " (" + dur(c.Remaining) + " left)"
		} else {
			s += " (" + dur(c.Elapsed) + ")"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "; ")
}

// commandName shortens a command line to its program and first words.
func commandName(args string) string {
	f := strings.Fields(args)
	if len(f) == 0 {
		return args
	}
	f[0] = f[0][strings.LastIndex(f[0], "/")+1:]
	return strings.Join(f[:min(4, len(f))], " ")
}

// namedSession is the session q names among sessions by sessionNamed's
// rule, over the roster as the store holds it at now.
func (a *app) namedSession(sessions []*claude.Session, q string, now time.Time) (*claude.Session, string, error) {
	st, err := a.store.Read()
	if err != nil {
		return nil, "", err
	}
	return sessionNamed(sessions, st, q, now)
}

// runningNamed is the running session q names by sessionNamed's rule; a
// registered agent whose CLI does not run is refused with what says so.
func (a *app) runningNamed(sessions []*claude.Session, q string, now time.Time) (*claude.Session, error) {
	s, absent, err := a.namedSession(sessions, q, now)
	if err != nil {
		return nil, err
	}
	if absent != "" {
		return nil, refused("%s", absent)
	}
	return s, nil
}

// sessionTail reads the last n turns of the session q names (running, or an
// ended session whose transcript is on disk) from its harness's transcript;
// with tools, the tool calls are turns too.
func (a *app) sessionTail(sessions []*claude.Session, q string, n int, tools bool, now time.Time) ([]claude.Turn, error) {
	s, err := a.tailSession(sessions, q, now)
	if err != nil {
		return nil, err
	}
	if s.Transcript == "" {
		return nil, fmt.Errorf("no transcript found for %q", s.Name)
	}
	st, err := a.store.Read()
	if err != nil {
		return nil, err
	}
	caller, _ := a.resolveTranscriptCaller(st)
	meta := transcripts.Meta{ID: s.ID, HostSession: s.HostID, Name: s.Name, Repo: s.Repo, Cwd: s.Cwd, Harness: s.Harness}
	a.enrichMetaFromState(st, &meta, s.Party(), claude.ByCLI(a.cfg))
	if !transcripts.Allowed(caller, meta, st.TranscriptGrants, now) {
		return nil, refused("transcript %q is outside your transcript access; request a grant with `beekeeper transcripts request --session %s --reason <why>`", meta.DisplayName(), cmp.Or(meta.ID, meta.HostSession))
	}
	h := harnessForSession(s)
	if tools {
		return h.Follow(s.Transcript, n)
	}
	return h.Tail(s.Transcript, n)
}

func (a *app) tailSession(sessions []*claude.Session, q string, now time.Time) (*claude.Session, error) {
	st, err := a.store.Read()
	if err != nil {
		return nil, err
	}
	s, absent, nameErr := sessionNamed(sessions, st, q, now)
	if nameErr == nil {
		if s.Transcript != "" {
			return s, nil
		}
		if tr, h := a.partyTranscript(s.Party(), sessions); tr != "" {
			cp := *s
			cp.Transcript, cp.Harness = tr, cmp.Or(cp.Harness, h)
			return &cp, nil
		}
		if absent != "" {
			return nil, refused("%s", absent)
		}
		return s, nil
	}
	if ended, ok := a.endedSessionNamed(sessions, st, q); ok {
		return ended, nil
	}
	return nil, nameErr
}

// partyTranscript locates the transcript on disk for session p across Claude
// Code (desktop records, project folders, or the state directory) and the
// non-Claude harnesses.
func (a *app) partyTranscript(p state.Party, running []*claude.Session) (path, harness string) {
	return (&transcriptFinder{a: a, running: running}).find(p)
}

// transcriptFinder locates transcripts for many parties, reading the desktop
// records by CLI session and each harness's ended sessions once.
type transcriptFinder struct {
	a       *app
	running []*claude.Session
	byCLI   map[string]*claude.Record
	ended   map[string][]*claude.Session
}

func (f *transcriptFinder) endedOf(h Harness) []*claude.Session {
	if f.ended == nil {
		f.ended = map[string][]*claude.Session{}
	}
	es, ok := f.ended[h.Name()]
	if !ok {
		es = h.Ended(f.a.cfg, f.running, time.Time{})
		f.ended[h.Name()] = es
	}
	return es
}

func (f *transcriptFinder) record(cli string) *claude.Record {
	if f.byCLI == nil {
		f.byCLI = claude.ByCLI(f.a.cfg)
	}
	return f.byCLI[cli]
}

func (f *transcriptFinder) find(p state.Party) (path, harness string) {
	if s, ok := claude.Live(f.running, p); ok && s.Transcript != "" {
		return s.Transcript, s.Harness
	}
	cfg := f.a.cfg
	if cfg == nil {
		return "", ""
	}
	if h, id, ok := harnessForHost(p.HostSession); ok {
		var convID string
		if f.a != nil && f.a.store != nil {
			if st, err := f.a.store.Read(); err == nil {
				if s, ok := startRecordOf(st, p); ok {
					convID = s.Conversation
				}
			}
		}
		for _, e := range f.endedOf(h) {
			if (e.ID == id || convID != "" && e.ID == convID || p.Session != "" && e.ID == p.Session || e.HostID == p.HostSession) && e.Transcript != "" {
				return e.Transcript, e.Harness
			}
		}
	}
	claudeFile := func(id, cwd string) string {
		if id == "" || strings.ContainsAny(id, `/\*?[`) {
			return ""
		}
		if cfg.Claude.ProjectsDir != "" {
			if tr := claude.Transcript(cfg, id, cwd); tr != "" {
				return tr
			}
		}
		if cfg.StateDir != "" {
			tr := filepath.Join(cfg.StateDir, id+".jsonl")
			if _, err := os.Stat(tr); err == nil { // #nosec G703 -- id carries no path separator (checked above), so the name stays inside the state directory
				return tr
			}
		}
		return ""
	}
	recordTranscript := func(r *claude.Record) string {
		cwd := cmp.Or(r.Cwd, r.OriginCwd)
		for _, id := range []string{p.Session, r.CLISessionID} {
			if tr := claudeFile(id, cwd); tr != "" {
				return tr
			}
		}
		for i := len(r.PriorCLISessionIDs) - 1; i >= 0; i-- {
			if tr := claudeFile(r.PriorCLISessionIDs[i], cwd); tr != "" {
				return tr
			}
		}
		return ""
	}
	if p.HostSession != "" {
		if r, ok := claude.ReadRecord(cfg, p.HostSession); ok {
			if tr := recordTranscript(r); tr != "" {
				return tr, ""
			}
		}
	}
	if p.Session != "" {
		if tr := claudeFile(p.Session, ""); tr != "" {
			return tr, ""
		}
		if r := f.record(p.Session); r != nil {
			if tr := recordTranscript(r); tr != "" {
				return tr, ""
			}
		}
	}
	for _, h := range harnesses {
		for _, e := range f.endedOf(h) {
			if (p.Session != "" && e.ID == p.Session || p.HostSession != "" && e.HostID == p.HostSession) && e.Transcript != "" {
				return e.Transcript, e.Harness
			}
		}
	}
	return "", ""
}

var asGuideRun = regexp.MustCompile(`\bas (Guide run \d+)\b`)

// guideStartVerb is the event a guide run logs when it takes the role
// (cmd/relay.go builds it as the role's name plus ".start"); its Detail names
// the run, so past guide runs are findable by their name.
const guideStartVerb = "guide.start"

// endedSessionNamed resolves q among ended sessions (past guide runs, the
// guide role's relieved and relay parties, session records, desktop records
// and ended harness sessions) whose transcript is on disk. Names and ids are
// matched first; only the matching sessions' transcripts are looked up.
func (a *app) endedSessionNamed(running []*claude.Session, st *state.State, q string) (*claude.Session, bool) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, false
	}
	lq := strings.ToLower(q)
	bare := strings.ToLower(strings.TrimSpace(titleSuffix.ReplaceAllString(q, "")))
	// matches reports whether a session with these ids and names is q.
	matches := func(id, host string, names []string) bool {
		return id == q || host == q || slices.ContainsFunc(names, func(n string) bool {
			ln := strings.ToLower(strings.TrimSpace(n))
			return ln != "" && (ln == bare || strings.Contains(ln, lq))
		})
	}

	type party struct {
		p     state.Party
		at    time.Time
		names []string
	}
	var parties []*party
	byKey := map[string]*party{}
	addParty := func(p state.Party, at time.Time, names ...string) {
		if p.Session == "" && p.HostSession == "" {
			return
		}
		k := p.Session + "\x00" + p.HostSession
		c, ok := byKey[k]
		if !ok {
			c = &party{p: p}
			byKey[k] = c
			parties = append(parties, c)
		}
		if at.After(c.at) {
			c.at = at
		}
		c.names = append(c.names, p.Name)
		c.names = append(c.names, names...)
	}
	if st != nil {
		r := st.GuideRole()
		if r.Holder != nil {
			var names []string
			if r.Run > 0 {
				names = append(names, guideRole.runName(r.Run))
			}
			addParty(r.Holder.Party, r.Holder.Since, names...)
		}
		if r.Relay != nil {
			addParty(r.Relay.From, r.Relay.At)
			addParty(r.Relay.To, r.Relay.At)
		}
		for _, rf := range r.Relieved {
			addParty(rf.Party, cmp.Or(rf.Taken, rf.At))
		}
		for _, rec := range st.Records {
			addParty(rec.Session, cmp.Or(rec.Ended, rec.At))
		}
		for _, s := range slices.Concat(st.Starts, st.EndedWorkers) {
			addParty(s.Party, s.At, s.Conversation)
		}
	}
	if evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == guideStartVerb }); err == nil {
		for _, e := range evs {
			var extra []string
			if m := asGuideRun.FindStringSubmatch(e.Detail); m != nil {
				extra = append(extra, m[1])
			}
			addParty(e.By, e.At, extra...)
		}
	}
	f := &transcriptFinder{a: a, running: running}
	if a.cfg != nil {
		for _, rec := range claude.AllRecords(a.cfg) {
			addParty(rec.Party(), rec.LastSeen(), rec.Title)
		}
	}

	type candidate struct {
		s     *claude.Session
		names []string
	}
	var list []*candidate
	byTranscript := map[string]*candidate{}
	addSession := func(s *claude.Session, names ...string) {
		if s == nil || s.Transcript == "" {
			return
		}
		c, ok := byTranscript[s.Transcript]
		if !ok {
			cp := *s
			c = &candidate{s: &cp}
			byTranscript[s.Transcript] = c
			list = append(list, c)
		} else {
			if c.s.ID == "" {
				c.s.ID = s.ID
			}
			if c.s.HostID == "" {
				c.s.HostID = s.HostID
			}
			if s.LastActive.After(c.s.LastActive) {
				c.s.LastActive = s.LastActive
			}
		}
		for _, n := range append([]string{s.Name}, names...) {
			if n = strings.TrimSpace(n); n != "" && !slices.ContainsFunc(c.names, func(o string) bool { return strings.EqualFold(o, n) }) {
				c.names = append(c.names, n)
				if c.s.Name == "" {
					c.s.Name = n
				}
			}
		}
	}
	for _, c := range parties {
		if !matches(c.p.Session, c.p.HostSession, c.names) {
			continue
		}
		tr, h := f.find(c.p)
		if tr == "" {
			continue
		}
		at := c.at
		if at.IsZero() {
			if fi, err := os.Stat(tr); err == nil {
				at = fi.ModTime()
			}
		}
		addSession(&claude.Session{ID: c.p.Session, HostID: c.p.HostSession, Name: c.p.Name, Transcript: tr, Harness: h, LastActive: at}, c.names...)
	}
	if a.cfg != nil {
		for _, h := range harnesses {
			for _, e := range f.endedOf(h) {
				if matches(e.ID, e.HostID, []string{e.Name}) {
					addSession(e, e.Name)
				}
			}
		}
		if tr, h := f.find(state.Party{Session: q, Name: q}); tr != "" {
			addSession(&claude.Session{ID: q, Name: q, Transcript: tr, Harness: h}, q)
		}
	}
	var idHits, whole, part []*candidate
	for _, c := range list {
		if c.s.ID == q || c.s.HostID == q {
			idHits = append(idHits, c)
			continue
		}
		switch {
		case slices.ContainsFunc(c.names, func(n string) bool {
			ln := strings.ToLower(n)
			return ln == lq || bare != "" && ln == bare
		}):
			whole = append(whole, c)
		case slices.ContainsFunc(c.names, func(n string) bool { return strings.Contains(strings.ToLower(n), lq) }):
			part = append(part, c)
		}
	}
	hits := idHits
	if len(hits) == 0 {
		hits = whole
	}
	if len(hits) == 0 && len(part) == 1 {
		hits = part
	}
	if len(hits) == 0 {
		return nil, false
	}
	slices.SortStableFunc(hits, func(a, b *candidate) int { return b.s.LastActive.Compare(a.s.LastActive) })
	return hits[0].s, true
}

func (a *app) tailCmd() *cobra.Command {
	var n int
	c := &cobra.Command{
		Use:   "tail <session>",
		Short: "Print a session's last turns: what it said and what it was told",
		Long: `Print the last turns of a session's transcript, without tool calls and tool
results: its own words, the person's (including question-dialog answers and
notes) and its peers' messages. The session is a name (or a unique part of
it): its desktop title, its roster name, or a past guide run's name; or a
session id or a PID.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			raw, _, err := a.sessions()
			if err != nil {
				return err
			}
			turns, err := a.sessionTail(raw, args[0], n, false, a.now)
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(turns)
			}
			for _, t := range turns {
				_, _ = fmt.Fprintf(a.out, "--- %s %s\n%s\n", clock(a.now, t.At), t.Role, t.Text)
			}
			return nil
		},
	}
	c.Flags().IntVarP(&n, "turns", "n", 3, "number of turns")
	return c
}
