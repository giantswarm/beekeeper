package cmd

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
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
}

// view is everything the session-level commands show.
type view struct {
	Sessions   []*sessionView   `json:"sessions"`
	Totals     *metricsTotals   `json:"totals,omitempty"`
	Overlaps   []claude.Overlap `json:"overlaps,omitempty"`
	Supervisor *supervisorView  `json:"supervisor,omitempty"`
	Leases     []leaseView      `json:"leases,omitempty"`
	raw        []*claude.Session
	st         *state.State
}

type supervisorView struct {
	state.Supervisor
	Live bool `json:"live"`
	// CLIGone is set while its CLI is gone, RestartUntil while it may still
	// come back as a restart; the grant rule holds either way.
	CLIGone      time.Time `json:"cliGone,omitzero"`
	RestartUntil time.Time `json:"restartUntil,omitzero"`
	// Relay is the supervisor's last relay; Relieved says it relieved the caller.
	Relay    *state.Relay `json:"relay,omitempty"`
	Relieved bool         `json:"relieved,omitempty"`
	// Context is its session's context in tokens (0: not read), RelayAt
	// the context at which the watch reports the relay due.
	Context int64 `json:"contextTokens,omitempty"`
	RelayAt int64 `json:"relayAt"`
}

// viewSupervisor is the recorded supervisor as sv reads it, with its
// session's context; nil when none is recorded.
func (a *app) viewSupervisor(st *state.State, sessions []*claude.Session, sv supervision) *supervisorView {
	if st.Supervisor == nil {
		return nil
	}
	return &supervisorView{
		Supervisor: *st.Supervisor, Live: sv.live, CLIGone: sv.gone, RestartUntil: sv.until, Relay: st.Relay,
		Context: sessionContext(sessions, st.Supervisor.Party, a.now), RelayAt: int64(a.cfg.Supervisor.RelayAt),
	}
}

// contextText says how far the supervisor's context is towards relayAt,
// "" when it is not known.
func (v *supervisorView) contextText() string {
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
	v.Supervisor = a.viewSupervisor(st, raw, a.supervision(st, raw))
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
	for i, s := range raw {
		sv := &sessionView{Session: s}
		if withWork {
			sv.Work, sv.Metrics = works[i], ms[i]
			work[s.PID] = sv.Work
		}
		sv.Role = roleOf(st, s)
		if i := slices.IndexFunc(st.Records, func(r state.Record) bool { return r.Session.Is(s.Party()) }); i >= 0 {
			sv.Serves = &st.Records[i]
		}
		for _, h := range holders {
			if h.Party().Is(s.Party()) {
				sv.Leases = append(sv.Leases, h.Env)
			}
		}
		v.Sessions = append(v.Sessions, sv)
	}
	slices.SortFunc(v.Sessions, func(x, y *sessionView) int { return y.LastActive.Compare(x.LastActive) })
	if withWork {
		v.Overlaps = claude.Overlaps(raw, work, claude.OverlapOptions{
			Skip:        func(s *claude.Session) bool { return st.Supervisor != nil && st.Supervisor.Is(s.Party()) },
			Ignore:      a.cfg.Overlaps.Ignore,
			ActiveSince: a.now.Add(-a.cfg.Overlaps.ActiveWithin.Duration),
		})
	}
	return v, nil
}

func roleOf(st *state.State, s *claude.Session) string {
	if st.Supervisor != nil && st.Supervisor.Is(s.Party()) {
		return "supervisor"
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
	var all bool
	c := &cobra.Command{
		Use:     "sessions",
		Aliases: []string{"ps"},
		Short:   "List the running sessions: what each is on, what it runs, what it holds",
		Long: `List the running Claude Code sessions, most recently active first: the
repository and the issues or pull requests its latest turns are about, when
it was last active, the tool commands it runs right now (a devctl wait, a
bounded sleep with the time left), its memory, how full its context is,
its last hour (turns, tool calls and their errors, GitHub calls, cost),
and its role and leases. A
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
session is on now. --all adds the sessions of the last 24 hours that run no
CLI (paused or closed, not archived): a message to them does not arrive.

The session records (sessions serve) follow the table: which session serves
which issue and what it waits on, and the records whose session has ended.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			v, err := a.collect(true)
			if err != nil {
				return err
			}
			var paused []*claude.Record
			if all {
				paused = a.paused(v.raw)
			}
			if a.json {
				return a.printJSON(struct {
					*view
					Records []state.Record   `json:"records,omitempty"`
					Paused  []*claude.Record `json:"paused,omitempty"`
				}{v, v.st.Records, paused})
			}
			a.printSessions(v)
			if len(v.st.Records) > 0 {
				_, _ = fmt.Fprintf(a.out, "\nSession records:\n")
				a.printRecords(v.st.Records, v.raw)
			}
			if len(paused) > 0 {
				_, _ = fmt.Fprintf(a.out, "\nNot running (paused or closed):\n")
				w := a.table()
				for _, r := range paused {
					_, _ = fmt.Fprintf(w, "  %s\t%s\tactive %s ago\n", truncate(r.Title, 50), r.Branch,
						ago(a.now, time.UnixMilli(r.LastActivityAt)))
				}
				_ = w.Flush()
			}
			return nil
		},
	}
	c.Flags().BoolVar(&all, "all", false, "also list the sessions of the last 24 hours that run no CLI")
	c.AddCommand(a.serveCmd(), a.unserveCmd())
	return c
}

// issueRef is an issue or pull request, owner/repo#n.
var issueRef = regexp.MustCompile(`^[\w.-]+/[\w.-]+#[0-9]+$`)

func (a *app) serveCmd() *cobra.Command {
	var waits string
	c := &cobra.Command{
		Use:   "serve <session> <owner/repo#n>",
		Short: "Record the issue or epic a session serves and what it waits on",
		Long: `Record, for any running session (a registered agent or not), the issue or
epic it serves and what it waits on. A new record replaces the session's
last one. sessions and the hand-over show it, and beekeeper watch prints one
line when the session ends, naming the issue to re-query. The session is a
name, a unique part of one, a session id or a PID.`,
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			if !issueRef.MatchString(args[1]) {
				return usageErr("%q is not an issue: owner/repo#n", args[1])
			}
			me, err := a.caller()
			if err != nil {
				return err
			}
			sessions, _, err := a.sessions()
			if err != nil {
				return err
			}
			s, err := claude.Resolve(sessions, args[0])
			if err != nil {
				return refused("%v", err)
			}
			r := state.Record{Session: s.Party(), Issue: args[1], Waits: strings.Join(strings.Fields(waits), " "), By: me, At: a.now.UTC()}
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Records = slices.DeleteFunc(st.Records, func(o state.Record) bool { return o.Session.Is(r.Session) })
				st.Records = append(st.Records, r)
				return []state.Event{event(me, "session.serve", "%s: %s", s.Name, recordText(r))}, nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.out, "%q %s\n", s.Name, recordText(r))
			return err
		},
	}
	c.Flags().StringVar(&waits, "waits", "", "what the session waits on")
	return c
}

func (a *app) unserveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unserve <session>",
		Short: "Remove a session's record: its work is done or handed on",
		Long: `Remove a session's record, running or ended. The session is a name, a
unique part of one or a session id.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			me, err := a.caller()
			if err != nil {
				return err
			}
			var r state.Record
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				parties := make([]state.Party, len(st.Records))
				for i, r := range st.Records {
					parties[i] = r.Session
				}
				i, err := findParty(parties, args[0], "session record", "session records")
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

// recordText says what a record's session serves.
func recordText(r state.Record) string {
	s := "serves " + r.Issue
	if r.Waits != "" {
		s += ", waiting on " + r.Waits
	}
	return s
}

// printRecords lists the session records: the running sessions' first,
// then those whose session has ended.
func (a *app) printRecords(records []state.Record, sessions []*claude.Session) {
	if len(records) == 0 {
		_, _ = fmt.Fprintln(a.out, "no session records")
		return
	}
	var ended []string
	for _, r := range records {
		if _, live := claude.Live(sessions, r.Session); live && r.Ended.IsZero() {
			_, _ = fmt.Fprintf(a.out, "- %q %s\n", r.Session.Name, recordText(r))
			continue
		}
		when := "has ended"
		if !r.Ended.IsZero() {
			when = "ended at " + clock(a.now, r.Ended)
		}
		ended = append(ended, fmt.Sprintf("- %q %s: it %s; re-query %s", r.Session.Name, when, recordText(r), r.Issue))
	}
	for _, l := range ended {
		_, _ = fmt.Fprintln(a.out, l)
	}
}

func (a *app) paused(running []*claude.Session) []*claude.Record {
	var out []*claude.Record
	for _, r := range claude.RecentRecords(a.cfg, a.now.Add(-24*time.Hour)) {
		if !slices.ContainsFunc(running, func(s *claude.Session) bool { return s.HostID == r.SessionID }) {
			out = append(out, r)
		}
	}
	return out
}

func (a *app) printSessions(v *view) {
	w := a.table()
	_, _ = fmt.Fprintln(w, "SESSION\tON\tACTIVE\tRUNNING\tMEM\tCTX\tLAST HOUR\tROLE / LEASES")
	for _, s := range v.Sessions {
		role := s.Role
		if role == "" && s.Parent != "" {
			role = "started by " + parentName(v.raw, s.Parent)
		}
		if len(s.Leases) > 0 {
			role = strings.TrimPrefix(role+" holds "+strings.Join(s.Leases, ","), " ")
		}
		hour := "-"
		if s.Metrics != nil {
			hour = hourText(s.Metrics.LastHour)
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%dM\t%s\t%s\t%s\n",
			truncate(s.Name, 44), truncate(on(s), 44), ago(a.now, s.LastActive),
			truncate(running(s.Commands), 40), s.MemMiB, contextText(s.Metrics), hour, truncate(role, 40))
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

func (a *app) tailCmd() *cobra.Command {
	var n int
	c := &cobra.Command{
		Use:   "tail <session>",
		Short: "Print a session's last turns: what it said and what it was told",
		Long: `Print the last turns of a session's transcript, without tool calls and tool
results: its own words, the person's and its peers' messages. The session is
a name (or a unique part of it), a session id or a PID.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			raw, _, err := a.sessions()
			if err != nil {
				return err
			}
			s, err := claude.Resolve(raw, args[0])
			if err != nil {
				return err
			}
			if s.Transcript == "" {
				return fmt.Errorf("no transcript found for %q", s.Name)
			}
			turns, err := claude.Tail(s.Transcript, n)
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
