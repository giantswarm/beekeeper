package cmd

import (
	"cmp"
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The doctor does by rule the chores the note queue used to ask a person
// for: it takes finished workers off the roster and archives the desktop
// sessions beekeeper started for them, takes relieved role holders off the
// roster, gives a desktop session the title the desktop dropped back, and
// probes and remedies the known faults of doctor.faults. Each is
// reversible (the desktop's Archived list brings a session back, a worker
// registers again), and it never archives or retitles a session a person
// started or one that holds or held the supervisor's or the guide's role;
// a run a relay relieved it archives, and a handed-over session.

// remedyTimeout bounds one run of a fault's remedy.
const remedyTimeout = 2 * time.Minute

// faultNote opens the text of the note a fault the doctor could not fix
// files for the person: an open one with it is the fault's only note.
const faultNote = "Known fault"

type choreKind int

const (
	choreRemove choreKind = iota
	choreRetitle
	// choreKeep is a stale entry the doctor leaves, since something keeps
	// it (keptBy): only a dry run says it.
	choreKeep
)

// chore is one roster or desktop fix the doctor found: an agent to take off
// the roster (and its desktop session to archive), or a desktop session to
// retitle with the agent's name.
type chore struct {
	kind    choreKind
	agent   state.Agent
	archive bool
	host    string // the desktop session to retitle
	why     string
	// stale says the agent is removed for staleness, which a keep marker
	// set since the plan stops.
	stale bool
	// stays says why a dry run would leave the desktop session.
	stays string
}

// String says the chore as the doctor would do it.
func (c chore) String() string {
	switch c.kind {
	case choreRetitle:
		return fmt.Sprintf("retitle %s %q (%s)", c.host, c.agent.Name, c.why)
	case choreKeep:
		return fmt.Sprintf("leave %q on the roster and its desktop session unarchived (%s)", c.agent.Name, c.why)
	}
	s := fmt.Sprintf("take %q off the roster", c.agent.Name)
	switch {
	case c.archive:
		s += " and archive its desktop session"
	case c.stays != "":
		s += "; " + c.stays
	}
	return s + " (" + c.why + ")"
}

// planChores finds the doctor's roster and desktop chores. An agent is
// taken off the roster once it reported its work done and its CLI is idle
// (busy: a turn or a gated merge of its own runs), once it was relieved of
// a role, or once it stayed idle staleAfter (0: never) with no CLI running,
// unless a keep marker or a timer that wakes it keeps it (a keep chore).
// A started session the roster keeps whose desktop record shows another
// title than its roster name is retitled while its CLI runs no turn.
// record reads a desktop session's record.
func planChores(st *state.State, sessions []*claude.Session, record func(host string) (*claude.Record, bool),
	busy func(state.Party) bool, staleAfter time.Duration, now time.Time,
) []chore {
	var out []chore
	for _, ag := range st.Agents {
		if ag.Task != "" || holdsRole(st, ag.Party) {
			continue
		}
		_, live := claude.Live(sessions, ag.Party)
		idle := now.Sub(cmp.Or(ag.IdleSince, ag.Registered))
		switch {
		case ag.Done:
			if !busy(ag.Party) {
				out = append(out, chore{kind: choreRemove, agent: ag, archive: true, why: "it reported its work done"})
			}
			continue
		case keepsRole(st, ag.Party):
			out = append(out, chore{kind: choreRemove, agent: ag, why: "it was relieved of its role, which relays instead"})
			continue
		case !live && staleAfter > 0 && idle >= staleAfter:
			stale := fmt.Sprintf("idle %s, its CLI no longer runs", dur(idle))
			kept := keptBy(st, ag, now)
			if kept == "" {
				out = append(out, chore{kind: choreRemove, agent: ag, archive: true, why: stale, stale: true})
				continue
			}
			out = append(out, chore{kind: choreKeep, agent: ag, why: stale + ", but " + kept})
		}
		i := slices.IndexFunc(st.Starts, func(x state.Start) bool { return x.Session != "" && x.Session == ag.Session })
		if i < 0 || st.Starts[i].HostSession == "" {
			continue
		}
		host := st.Starts[i].HostSession
		if r, ok := record(host); ok && !r.IsArchived && r.Title != ag.Name && !busy(ag.Party) {
			out = append(out, chore{kind: choreRetitle, agent: ag, host: host, why: "the desktop recorded " + recorded(r.Title, ag.Name)})
		}
	}
	return out
}

// faultFinding is what the doctor found of one known fault: whether its
// probe passed, whether it ran the remedy and whether that fixed it.
type faultFinding struct {
	fault         config.Fault
	healthy, ran  bool
	fixed         bool
	remedyFailure error
}

// String says the finding in one line.
func (f faultFinding) String() string {
	switch {
	case f.healthy && f.ran:
		return fmt.Sprintf("fixed fault %q: its probe failed, its remedy `%s` ran, its probe passes", f.fault.Name, f.fault.Remedy)
	case f.healthy:
		return fmt.Sprintf("fault %q: absent", f.fault.Name)
	case f.ran && f.remedyFailure != nil:
		return fmt.Sprintf("fault %q: its probe fails and its remedy `%s` failed: %v", f.fault.Name, f.fault.Remedy, f.remedyFailure)
	case f.ran:
		return fmt.Sprintf("fault %q: its probe fails, also after its remedy `%s` ran", f.fault.Name, f.fault.Remedy)
	}
	return fmt.Sprintf("fault %q: its probe fails; its remedy `%s` runs attended only: beekeeper doctor --fault %s", f.fault.Name, f.fault.Remedy, f.fault.Name)
}

// shell runs a probe or a remedy; an exit other than 0 is an error.
type shell func(ctx context.Context, command string, timeout time.Duration) error

func runShell(ctx context.Context, command string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sh", "-c", command).CombinedOutput() //nolint:gosec // the configured probe or remedy of this machine
	if err != nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			return fmt.Errorf("%w: %s", err, truncate(s, 200))
		}
	}
	return err
}

// checkFaults probes each fault and runs the remedy of a failing one when
// fix allows it, probing again after.
func checkFaults(ctx context.Context, faults []config.Fault, fix func(config.Fault) bool, run shell) []faultFinding {
	out := make([]faultFinding, 0, len(faults))
	for _, f := range faults {
		r := faultFinding{fault: f, healthy: run(ctx, f.Probe, probeTimeout) == nil}
		if !r.healthy && fix(f) {
			r.ran = true
			r.remedyFailure = run(ctx, f.Remedy, remedyTimeout)
			r.healthy = run(ctx, f.Probe, probeTimeout) == nil
		}
		r.fixed = r.healthy && r.ran
		out = append(out, r)
	}
	return out
}

// noteFaults files one note for person for each fault still failing that
// has none open, and closes the open note of a fault that is absent again.
// It returns the lines and events of what it filed and closed.
func noteFaults(st *state.State, findings []faultFinding, person string, now time.Time) ([]string, []state.Event) {
	var lines []string
	var evs []state.Event
	for _, f := range findings {
		prefix := fmt.Sprintf("%s %q:", faultNote, f.fault.Name)
		i := slices.IndexFunc(st.Notes, func(n state.Note) bool { return strings.HasPrefix(n.Text, prefix) })
		switch {
		case f.healthy && i >= 0:
			n := st.Notes[i]
			st.Notes = slices.Delete(st.Notes, i, i+1)
			lines = append(lines, fmt.Sprintf("closed note #%d: fault %q is absent again", n.ID, f.fault.Name))
			evs = append(evs, event(watchParty, noteDone, "#%d closed, its fault is absent again: %s", n.ID, n.Text))
		case !f.healthy && i < 0 && person != "":
			d := noteDraft{
				Question:  fmt.Sprintf("%s run `beekeeper doctor --fault %s` (its remedy: `%s`)?", prefix, f.fault.Name, f.fault.Remedy),
				StatusQuo: fmt.Sprintf("its probe `%s` fails", f.fault.Probe),
				Why:       faultWhy(f),
				Options:   []string{"run it: the remedy runs once, the doctor probes again"},
			}
			st.NextNote++
			n := state.Note{ID: st.NextNote, For: person, Text: d.text(), By: watchParty, At: now.UTC(),
				Default: "the doctor keeps probing and closes this note once the fault is absent"}
			st.Notes = append(st.Notes, n)
			lines = append(lines, fmt.Sprintf("note #%d for %s: fault %q", n.ID, person, f.fault.Name))
			evs = append(evs, event(watchParty, "note.add", "#%d %s", n.ID, n.Text))
		}
	}
	return lines, evs
}

func faultWhy(f faultFinding) string {
	if f.ran {
		return "its remedy ran and did not fix it"
	}
	return "its remedy may not run unattended (doctor.faults)"
}

// doctorRun is what one run of the doctor does: dryRun only says it,
// attended are the faults whose remedy runs although it may not run
// unattended, and retry says whether to retitle a desktop session again.
type doctorRun struct {
	by       state.Party
	dryRun   bool
	attended []string
	retry    func(host string) bool
	run      shell
}

// doctorReport is what a run of the doctor found and did, a line each.
type doctorReport struct {
	chores []string
	faults []faultFinding
	notes  []string
	// unagreed counts the finished workers whose archive waits on
	// agents.archiveAgreement, said once for all, not per agent.
	unagreed int
	// stale are the running processes of an older beekeeper that saved the
	// state after a newer one.
	stale []state.StaleWriter
}

// staleLine says a stale writer and what ends it.
func staleLine(w state.StaleWriter) string {
	return "stale writer: " + w.String() + "; it keeps the fields it does not know but saves by its older rules until it ends or is restarted"
}

// liveStaleWriters are the stale writers whose process still runs.
func liveStaleWriters(st *state.State, alive func(int) bool) []state.StaleWriter {
	var out []state.StaleWriter
	for _, w := range st.StaleWriters {
		if alive(w.PID) {
			out = append(out, w)
		}
	}
	return out
}

// doctor finds the chores and the faults, and fixes what it may.
func (a *app) doctor(ctx context.Context, r doctorRun) (doctorReport, error) {
	var rep doctorReport
	st, err := a.store.Read()
	if err != nil {
		return rep, err
	}
	rep.stale = liveStaleWriters(st, proc.Alive)
	sessions, t, err := a.sessions()
	if err != nil {
		return rep, err
	}
	record := func(host string) (*claude.Record, bool) { return claude.ReadRecord(a.cfg, host) }
	busy := func(p state.Party) bool {
		_, turn := turnRunning(sessions, t, p, a.now)
		return turn || mergeInFlight(st, p, proc.Alive)
	}
	chores := planChores(st, sessions, record, busy, a.cfg.Agents.StaleAfter.Duration, a.now)
	fix := func(f config.Fault) bool { return !r.dryRun && (f.Unattended || slices.Contains(r.attended, f.Name)) }
	run := r.run
	if run == nil {
		run = runShell
	}
	rep.faults = checkFaults(ctx, a.cfg.Doctor.Faults, fix, run)
	if r.dryRun {
		for _, c := range chores {
			if c.archive {
				if host, why := a.archivable(st, c.agent.Party); why != "" {
					c.archive, c.stays = false, why
					if host != "" {
						c.stays += "; the doctor owes it the archive"
					}
				}
			}
			rep.chores = append(rep.chores, "would "+c.String())
		}
		seedArchives(st, record, a.now)
		for _, o := range planArchives(st, record, busy, a.now) {
			rep.chores = append(rep.chores, "would "+o.String())
		}
		rep.chores = append(rep.chores, a.reopenRowless(ctx, st, t, record, r)...)
		return rep, nil
	}
	failing := slices.ContainsFunc(rep.faults, func(f faultFinding) bool { return !f.healthy })
	if failing || slices.ContainsFunc(st.Notes, func(n state.Note) bool { return strings.HasPrefix(n.Text, faultNote+" ") }) {
		err = a.store.Update(func(st *state.State) ([]state.Event, error) {
			lines, evs := noteFaults(st, rep.faults, a.cfg.Guide.Person, a.now)
			rep.notes = lines
			return evs, nil
		})
		if err != nil {
			return rep, err
		}
	}
	removed, err := a.removeAgents(chores, r.by)
	if err != nil {
		return rep, err
	}
	var archive []state.Party
	for _, c := range removed {
		rep.chores = append(rep.chores, fmt.Sprintf("took %q off the roster: %s", c.agent.Name, c.why))
		if c.archive {
			archive = append(archive, c.agent.Party)
		}
	}
	owed, err := a.owedArchives(sessions, record, busy, r.by)
	if err != nil {
		return rep, err
	}
	rep.chores = append(rep.chores, owed.lines...)
	for _, p := range owed.retry {
		if !slices.ContainsFunc(archive, p.Is) {
			archive = append(archive, p)
		}
	}
	if len(archive) > 0 {
		outcomes := a.archiveDesktops(ctx, st, archive, "beekeeper doctor")
		_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
			lines := owe(st, outcomes, a.now)
			var evs []state.Event
			for i, l := range lines {
				if outcomes[i].unagreed {
					rep.unagreed++
					continue
				}
				rep.chores = append(rep.chores, fmt.Sprintf("%q: %s", archive[i].Name, l))
				evs = append(evs, event(r.by, "agents.archive", "%s: %s", archive[i].Name, l))
			}
			return evs, nil
		})
	}
	for _, c := range chores {
		if c.kind != choreRetitle || (r.retry != nil && !r.retry(c.host)) {
			continue
		}
		line, err := a.restoreTitle(ctx, c.host, c.agent.Name)
		if err != nil {
			line = fmt.Sprintf("its title stays: %v", err)
		}
		rep.chores = append(rep.chores, fmt.Sprintf("%q: %s", c.agent.Name, line))
		_ = a.store.Update(func(*state.State) ([]state.Event, error) {
			return []state.Event{event(r.by, "agents.retitle", "%s: %s", c.agent.Name, line)}, nil
		})
	}
	rep.chores = append(rep.chores, a.reopenRowless(ctx, st, t, record, r)...)
	return rep, nil
}

// owedRun is what the doctor's run found of the archives owed: the agents
// whose archive it asks for again, and a line for each it owes no longer.
type owedRun struct {
	retry []state.Party
	lines []string
}

// owedArchives seeds the archives owed once, drops those owed no longer and
// returns the agents whose archive is due again. It writes the state only
// when it changes.
func (a *app) owedArchives(sessions []*claude.Session, record func(host string) (*claude.Record, bool), busy func(state.Party) bool, by state.Party) (owedRun, error) {
	var run owedRun
	sort := func(st *state.State) []state.Event {
		run = owedRun{}
		seedArchives(st, record, a.now)
		var evs []state.Event
		for _, o := range planArchives(st, record, busy, a.now) {
			switch {
			case o.drop != "":
				st.Archives = slices.DeleteFunc(st.Archives, func(x state.Archive) bool { return x.Host == o.ar.Host })
				line := fmt.Sprintf("its desktop session %s is owed no archive any more: %s", o.ar.Host, o.drop)
				run.lines = append(run.lines, fmt.Sprintf("%q: %s", o.ar.Name, line))
				evs = append(evs, event(by, "agents.archive", "%s: %s", o.ar.Name, line))
			case o.wait == "" && len(run.retry) < archiveBatch:
				run.retry = append(run.retry, o.ar.Party)
			}
		}
		return evs
	}
	st, err := a.store.Read()
	if err != nil {
		return run, err
	}
	seeded := st.ArchivesSeeded
	if sort(st); seeded && len(run.lines) == 0 {
		return run, nil
	}
	err = a.store.Update(func(st *state.State) ([]state.Event, error) { return sort(st), nil })
	return run, err
}

// removeAgents takes the agents of the remove chores off the roster, those
// still idle, and returns the chores it did.
func (a *app) removeAgents(chores []chore, by state.Party) ([]chore, error) {
	var removed []chore
	if !slices.ContainsFunc(chores, func(c chore) bool { return c.kind == choreRemove }) {
		return nil, nil
	}
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		removed = nil
		var evs []state.Event
		for _, c := range chores {
			i := slices.IndexFunc(st.Agents, func(x state.Agent) bool { return x.Task == "" && x.Is(c.agent.Party) })
			if c.kind != choreRemove || i < 0 || c.stale && keptBy(st, st.Agents[i], a.now) != "" {
				continue
			}
			removeAgent(st, i)
			removed = append(removed, c)
			evs = append(evs, event(by, "agents.remove", "%s: %s", c.agent.Name, c.why))
		}
		return evs, nil
	})
	return removed, err
}

func (a *app) doctorCmd() *cobra.Command {
	var dryRun bool
	var attended []string
	c := &cobra.Command{
		Use:   "doctor",
		Short: "Find and fix the roster's, the desktop's and the machine's known faults",
		Long: `Does by rule what a person was asked for in notes, and says each in one
line:

- takes an agent off the roster once it reported its work done (agents idle
  --done) and its CLI runs no turn, once it was relieved of the
  supervisor's or the guide's role, or once it stayed idle
  agents.staleAfter (24h) with no CLI running; the desktop sessions
  beekeeper started for the finished and the stale ones are archived in
  one steward's turn (the desktop's Archived list brings one back); a
  stale entry kept on purpose (agents keep, or an open timer that wakes it
  by name) stays, and so does its desktop session;
- asks again for an archive that stayed (its CLI ran a turn, no steward
  recorded it) on its later runs while the CLI runs no turn, until the
  desktop records it, up to 5 stewards' turns 10 minutes apart within 24h;
  so are archived the session an agents handover ended and the run of the
  supervisor or the guide a relay relieved, which frees its desktop CLI;
- gives a session beekeeper started the roster name back when the desktop
  recorded another title, through a steward;
- reopens a worker whose session the desktop never imported (no row in the
  sidebar: its start ran at the desktop's cap of CLIs, say) and whose CLI
  does not run, once the desktop runs fewer CLIs than its cap, in a
  transient unit beekeeper-reopen-<id> that gives it its row and warms its
  CLI (agent.reopen in the log); the watch says NO DESKTOP ROW once per
  such worker meanwhile;
- probes each fault of doctor.faults (a probe exits 0 while the fault is
  absent) and runs the remedy of a failing one that may run unattended,
  or the faults named with --fault; a fault still failing is one note for
  guide.person, closed once its probe passes;
- trims the Go build cache that every session's builds share once it is over
  doctor.goCacheMaxGiB (20), least recently used entries first, down to
  three quarters of the cap, never while a go build runs (the watch does
  it every doctor.goCacheEvery, 1h, and says GO CACHE when a trim waited
  that long or failed); each trim is logged (gocache.trim);
- reports a stale writer: a running process of an older beekeeper that
  saved the state after a newer one (state.stale-writer in the log). Its
  saves keep the fields it does not know, yet it acts by its older rules
  until it ends or is restarted.

A session a person started is never archived or retitled, nor one that
holds or held the supervisor's or the guide's role unless a relay
relieved it. The watch runs the
doctor every tick (beekeeper watch); --dry-run says what it would do,
the archives it owes and the kept entries it leaves included, probing the
faults but remedying none.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			for _, n := range attended {
				if !slices.ContainsFunc(a.cfg.Doctor.Faults, func(f config.Fault) bool { return f.Name == n }) {
					return usageErr("--fault %q: doctor.faults has no such fault", n)
				}
			}
			me, err := a.caller()
			if err != nil {
				return err
			}
			rep, err := a.doctor(cmd.Context(), doctorRun{by: me, dryRun: dryRun, attended: attended})
			if err != nil {
				return err
			}
			lines := append(slices.Clone(rep.chores), rep.notes...)
			if rep.unagreed > 0 {
				lines = append(lines, unagreedLine(rep.unagreed))
			}
			for _, f := range rep.faults {
				lines = append(lines, f.String())
			}
			for _, w := range rep.stale {
				lines = append(lines, staleLine(w))
			}
			if r := a.trimGoCache(dryRun, goBuilds); r.notable() {
				lines = append(lines, r.String())
			}
			if len(lines) == 0 {
				lines = []string{"doctor: nothing to fix"}
			}
			for _, l := range lines {
				if _, err := fmt.Fprintln(a.out, l); err != nil {
					return err
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "say what the doctor would do, fixing nothing")
	c.Flags().StringArrayVar(&attended, "fault", nil, "run this fault's remedy although it may not run unattended (repeatable)")
	return c
}

// retitleAgain is how long the watch's doctor leaves a desktop session it
// asked a steward to retitle before it asks again.
const retitleAgain = 30 * time.Minute

// doctor runs the doctor in the background, unless its last run still
// goes: a steward's turn takes up to a minute, and the poll goes on. Each
// chore it did is one DOCTOR line; a fault it could not fix is one DOCTOR
// FAULT line while it lasts.
func (w *watcher) doctor(ctx context.Context) {
	if !w.chores || !w.doctoring.CompareAndSwap(false, true) {
		return
	}
	d := *w.app // the poll moves w.now on
	retry := func(host string) bool {
		if d.now.Sub(w.retitled[host]) < retitleAgain {
			return false
		}
		w.retitled[host] = d.now
		return true
	}
	go func() {
		defer w.doctoring.Store(false)
		rep, err := d.doctor(ctx, doctorRun{by: watchParty, retry: retry})
		if err != nil {
			w.emit("doctor", "DOCTOR: %v", err)
			return
		}
		w.clear("doctor")
		w.sayChores(rep)
		for _, f := range rep.faults {
			if f.fixed {
				w.emitNow("doctor", "DOCTOR %s", f)
			}
			w.check("doctor-fault "+f.fault.Name, !f.healthy, "DOCTOR FAULT %s", f)
		}
		stale := map[string]bool{}
		for _, s := range rep.stale {
			key := fmt.Sprintf("doctor-stale %d", s.PID)
			stale[key] = true
			w.check(key, true, "DOCTOR %s", staleLine(s))
		}
		w.clearMissing("doctor-stale ", stale)
	}()
}

// sayChores says the doctor's chore lines, each once per watch (a stay that
// repeats is not said again), its notes, and the finished workers waiting on
// agents.archiveAgreement as one summary while any do.
func (w *watcher) sayChores(rep doctorReport) {
	if w.doctored == nil {
		w.doctored = map[string]bool{}
	}
	for _, l := range rep.chores {
		if !w.doctored[l] {
			w.doctored[l] = true
			w.emitNow("doctor", "DOCTOR %s", l)
		}
	}
	for _, l := range rep.notes {
		w.emitNow("doctor", "DOCTOR %s", l)
	}
	w.check("doctor-unagreed", rep.unagreed > 0, "DOCTOR %s", unagreedLine(rep.unagreed))
}

// unagreedLine says how many finished workers' desktop sessions stay
// unarchived for want of the person's agreement.
func unagreedLine(n int) string {
	return fmt.Sprintf("%d finished workers' desktop sessions stay unarchived: set agents.archiveAgreement to the person's agreement for a steward to archive them", n)
}
