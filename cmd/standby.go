package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/peer"
	"github.com/giantswarm/beekeeper/internal/state"
)

// resumeMessage tells a role's holder to take its role up again in a new
// CLI: the handover prompt itself is too long to relay through the
// sender's turn.
func (rl role) resumeMessage(why string) string {
	return "beekeeper: " + why + ". Run `" + rl.handover + "` and follow it."
}

// reopenGrace is how long the standby watch gives a supervisor whose row it
// reopened after the desktop app started to come back before it starts a
// successor.
const reopenGrace = 3 * time.Minute

// standbyWatch is what the standby watch remembers of the messages it sent,
// the successors it started and the rows it reopened; a restarted watch
// starts afresh, and the relay a successor's start opens is in the state.
type standbyWatch struct {
	send func(ctx context.Context, to, msg string) error
	// open hands a claude:// link to the desktop app (plat.Opener.Open).
	open func(ctx context.Context, url string, running bool) error
	// succeed starts rl's next run after its holder from is gone
	// (app.startSuccessor).
	succeed func(ctx context.Context, rl role, from state.Party) (state.Party, error)
	// revive resumes rl's holder headless with msg (app.wakeAgent): a
	// successor whose first turn ended and whose desktop CLI never came.
	revive func(ctx context.Context, rl role, holder state.Party, msg string) error
	// chains are the successors of each role, by its name, that did not
	// come up.
	chains map[string]*successorChain
	// turning reports whether a unit of beekeeper's start or wake of
	// session id runs a turn or the reopen after it (unitsTurning); nil:
	// none does.
	turning func(ctx context.Context, id string) bool
	busy    atomic.Bool
	// guideGap is the term of the gone guide this watch said.
	guideGap string
	// starting is the role whose successor is being started, one at a time;
	// inflight waits for it (a watch --once).
	starting atomic.Bool
	inflight sync.WaitGroup
	// reopened is the gone supervisor term reopened, at reopenedAt.
	reopened   string
	reopenedAt time.Time
	// liveAt is when this watch last saw the supervisor's CLI of the term
	// liveTerm run.
	liveTerm string
	liveAt   time.Time
}

func (a *app) supervisorReopenCmd() *cobra.Command {
	var print bool
	c := &cobra.Command{
		Use:   "reopen",
		Short: "Open the recorded supervisor's desktop session, starting the app if needed (the login unit)",
		Long: `Open claude://code/continue?session=local_… for the recorded supervisor: it
starts the desktop app on the supervisor's row, or switches the running app
to it. Right after the app starts no CLI runs yet, so the focus starts the
supervisor's CLI; the standby watch then sees it back and tells it to
resume the role. The beekeeper-supervisor-open user unit runs this at
login. It does nothing when no supervisor is recorded or it has no desktop
session.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := a.store.Read()
			if err != nil {
				return err
			}
			if st.Supervisor == nil || st.Supervisor.HostSession == "" {
				_, err := fmt.Fprintln(a.out, "no supervisor with a desktop session is recorded: nothing to open")
				return err
			}
			url := continueURL(st.Supervisor.HostSession)
			if print {
				_, err := fmt.Fprintln(a.out, url)
				return err
			}
			t, err := plat.Machine.Processes()
			if err != nil {
				return err
			}
			if err := plat.Opener.Open(cmd.Context(), url, !plat.Opener.Running(t).IsZero()); err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.out, "opened %q: %s\n", st.Supervisor.Name, url)
			return err
		},
	}
	c.Flags().BoolVar(&print, "print", false, "print the link instead of opening it")
	return c
}

func continueURL(host string) string { return "claude://code/continue?session=" + host }

// startGrace is how long a relay may name a session no start of
// beekeeper's recorded: the start records it before it launches the session.
const startGrace = 2 * time.Minute

// relayPending reports whether r's open relay still stands for a
// successor: one whose start beekeeper recorded, or a moment ago. A relay a
// start never followed (the watch that opened it stopped) is left to the
// next successor, which replaces it.
func relayPending(st *state.State, r state.Role, now time.Time) bool {
	rel := r.Relay
	if !rel.Open(now) {
		return false
	}
	return !rel.Taken.IsZero() || now.Sub(rel.At) < startGrace || rel.To.Session == "" ||
		slices.ContainsFunc(st.Starts, func(s state.Start) bool { return s.Session == rel.To.Session })
}

// succeedFromWatch starts rl's next run for the standby watch, in the gone
// holder's folder.
func (a *app) succeedFromWatch(ctx context.Context, rl role, from state.Party) (state.Party, error) {
	to, _, err := a.startSuccessor(ctx, rl, from, watchParty, "")
	return to, err
}

// firstTurn reports whether p's session is between beekeeper's start or
// wake and its desktop CLI: its headless turn runs, or the reopen after it.
// Its CLI is gone meanwhile without the session being gone.
func (w *watcher) firstTurn(ctx context.Context, p state.Party) bool {
	return p.Session != "" && w.stand.turning != nil && w.stand.turning(ctx, p.Session)
}

// unitsTurning reports whether a start or wake unit of session id is
// active, starting or running its reopen (deactivating).
func unitsTurning(ctx context.Context, id string) bool {
	if len(id) < 8 {
		return false
	}
	return len(plat.Launcher.Running(ctx, true, "beekeeper-agent-"+id[:8]+".service", wakePrefix(id)+"*")) > 0
}

// guideGone says once when the guide's CLI stayed gone past its grace with
// no relay open, and starts its successor (standby watch).
func (w *watcher) guideGone(ctx context.Context, st *state.State, sessions []*claude.Session) {
	r := guideRole.get(st)
	sv := readHolder(r, sessions, w.now, w.cfg.Guide.RestartGrace.Duration)
	if !sv.down() || relayPending(st, r, w.now) || w.firstTurn(ctx, r.Holder.Party) {
		if sv.live {
			w.stand.guideGap = ""
			w.upAgain(guideRole, r.Holder.Party)
		}
		return
	}
	key := r.Holder.Name + "@" + r.Holder.Since.UTC().Format(time.RFC3339)
	successor := w.standIn(ctx, guideRole, st, r.Holder.Party, key)
	if w.stand.guideGap != key {
		w.stand.guideGap = key
		w.emitNow("guide", "GUIDE GONE: %q (guiding since %s) is gone since %s%s", r.Holder.Name, clock(w.now, r.Holder.Since), clock(w.now, sv.gone), successor)
	}
}

// Successors that do not come up are retried a bounded number of times:
// the next one starts successorBackoff after the first failed, twice that
// after the second, none after maxFailedSuccessors.
const (
	successorBackoff    = 5 * time.Minute
	maxFailedSuccessors = 3
)

// successorStarting is what the GONE line adds while a successor's start or
// a headless resume runs.
const successorStarting = "; its successor is starting"

// successorChain is what the standby watch remembers of a role's
// successors that did not come up: the holder term it resumed headless,
// the last failed one, how many failed, when the next may start, and the
// note it filed after the first.
type successorChain struct {
	revived, failed string
	failures        int
	next            time.Time
	note            int
}

// chain is rl's successorChain.
func (w *watcher) chain(rl role) *successorChain {
	if w.stand.chains == nil {
		w.stand.chains = map[string]*successorChain{}
	}
	c := w.stand.chains[rl.name]
	if c == nil {
		c = &successorChain{}
		w.stand.chains[rl.name] = c
	}
	return c
}

// upAgain forgets rl's failed successors once its live holder's CLI is no
// start's first turn: a successor whose desktop CLI or headless resume
// runs, or any holder that is no successor.
func (w *watcher) upAgain(rl role, holder state.Party) {
	if headlessTurn(w.table, holder.Session) == "first turn" {
		return
	}
	if c := w.stand.chains[rl.name]; c != nil && c.failures > 0 {
		w.emitNow(rl.name+"-successor", "%sSUCCESSOR UP: the %s runs again after %d successor(s) that did not come up", rl.tag, rl.name, c.failures)
	}
	delete(w.stand.chains, rl.name)
}

// standIn acts for rl's holder, gone past the grace with no relay open and
// no first turn running (standby watch), the holder's term being key. A
// successor beekeeper started, whose first turn took the role and ended
// and whose desktop CLI never came (the desktop did not import it, or did
// not warm it), is resumed headless once: that turn arms the role's watch
// and keeps its CLI, so no further successor starts while it runs. One that
// went down after that failed: one note after the first, and the next
// successor waits out the backoff; past maxFailedSuccessors none starts.
// Any other holder gets its successor at once. It returns what the GONE
// line adds.
func (w *watcher) standIn(ctx context.Context, rl role, st *state.State, holder state.Party, key string) string {
	if w.stand.starting.Load() {
		return successorStarting
	}
	c := w.chain(rl)
	if started(st, holder) {
		if c.revived != key && w.stand.revive != nil {
			c.revived = key
			return w.reviveGone(ctx, rl, holder)
		}
		if c.failed != key {
			c.failed, c.failures = key, c.failures+1
			c.next = w.now.Add(successorBackoff << (c.failures - 1))
			if c.failures == 1 {
				c.note = w.noteFailedSuccessor(rl, holder)
			}
			w.emitNow(rl.name+"-successor", "%sSUCCESSOR DOWN: %q did not come up%s", rl.tag, holder.Name, c.outlook(w.now))
		}
	}
	if c.failures >= maxFailedSuccessors || w.now.Before(c.next) {
		return c.outlook(w.now)
	}
	return w.succeedGone(ctx, rl, holder)
}

// outlook says what follows c's failed successors.
func (c *successorChain) outlook(now time.Time) string {
	note := ""
	if c.note > 0 {
		note = fmt.Sprintf(" (note #%d)", c.note)
	}
	if c.failures >= maxFailedSuccessors {
		return fmt.Sprintf("; %d successors did not come up, none further starts%s", c.failures, note)
	}
	return fmt.Sprintf("; %d successor(s) did not come up, the next starts at %s%s", c.failures, clock(now, c.next), note)
}

// started reports whether p's session is one of beekeeper's starts.
func started(st *state.State, p state.Party) bool {
	return p.Session != "" && slices.ContainsFunc(st.Starts, func(s state.Start) bool { return s.Session == p.Session })
}

// reviveGone resumes rl's gone holder headless outside the poll, as
// succeedGone starts a successor. It returns what the GONE line adds.
func (w *watcher) reviveGone(ctx context.Context, rl role, holder state.Party) string {
	if !w.stand.starting.CompareAndSwap(false, true) {
		return successorStarting
	}
	w.stand.inflight.Add(1)
	go func() {
		defer w.stand.inflight.Done()
		defer w.stand.starting.Store(false)
		msg := rl.resumeMessage("your first turn ended and the desktop runs no CLI of yours: this headless turn keeps " + rl.duty)
		if err := w.stand.revive(ctx, rl, holder, msg); err != nil {
			w.emitNow(rl.name+"-successor", "%sRESUME FAILED: %q has no desktop CLI and did not resume headless: %v", rl.tag, holder.Name, err)
			return
		}
		w.emitNow(rl.name+"-successor", "%sRESUME: %q has no desktop CLI: resumed it headless, its turn keeps %s", rl.tag, holder.Name, rl.duty)
	}()
	return "; resuming it headless (the desktop runs no CLI of it)"
}

// reviveFromWatch resumes rl's holder headless with msg for the standby
// watch.
func (a *app) reviveFromWatch(ctx context.Context, _ role, holder state.Party, msg string) error {
	q := holder.Session
	if q == "" {
		q = holder.Name
	}
	return a.wakeAgent(ctx, watchParty, q, msg, "")
}

// noteFailedSuccessor files the one note for the person on rl's first
// successor that did not come up, and returns its number (0: none filed).
func (w *watcher) noteFailedSuccessor(rl role, holder state.Party) int {
	person := w.cfg.Guide.Person
	if person == "" {
		return 0
	}
	d := noteDraft{
		Question: fmt.Sprintf("%q, the %s's successor beekeeper started, did not come up: no desktop CLI after its first turn, and its headless resume ended. Take the role over from a running desktop session (`beekeeper %s start --take-over`)?",
			holder.Name, rl.name, rl.name),
		StatusQuo: fmt.Sprintf("%s holds the role in name only: %s", holder.Name, rl.gone),
		Why:       "the desktop did not run the successor's CLI (`journalctl --user -u beekeeper-notify` and the desktop's main.log say why)",
		Options:   []string{"take over: the running session holds the role and arms its watch", "leave it: the standby watch retries"},
	}
	var id int
	_ = w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.NextNote++
		id = st.NextNote
		n := state.Note{ID: id, For: person, Text: d.text(), By: watchParty, At: w.now.UTC(),
			Default: fmt.Sprintf("the standby watch starts at most %d successors, %s apart and doubling", maxFailedSuccessors, dur(successorBackoff))}
		st.Notes = append(st.Notes, n)
		return []state.Event{event(watchParty, "note.add", "#%d %s", n.ID, n.Text)}, nil
	})
	return id
}

// succeedGone starts rl's next run once its holder's CLI stayed gone past
// the grace (standby watch): the successor's start takes the role through
// the relay beekeeper opens to it, so a repeated poll and a restarted watch
// see the relay and start none. It returns what the GONE line adds.
func (w *watcher) succeedGone(ctx context.Context, rl role, holder state.Party) string {
	if w.stand.succeed == nil {
		return ""
	}
	if !w.stand.starting.CompareAndSwap(false, true) {
		return successorStarting
	}
	w.stand.inflight.Add(1)
	go func() {
		defer w.stand.inflight.Done()
		defer w.stand.starting.Store(false)
		to, err := w.stand.succeed(ctx, rl, holder)
		if err != nil {
			w.emitNow(rl.name+"-successor", "%sSUCCESSOR FAILED: %q is gone and its successor did not start: %v", rl.tag, holder.Name, err)
			return
		}
		w.emitNow(rl.name+"-successor", "%sSUCCESSOR: started %q, whose `beekeeper %s start` takes the role from the gone %q", rl.tag, to.Name, rl.name, holder.Name)
	}()
	return "; starting its successor"
}

// reopenAfterAppStart opens the gone supervisor's row once when the desktop
// app started after its CLI stopped: an app restart, and a reboot, leave
// every CLI cold, so the focus starts it. It reports whether the row was
// reopened within reopenGrace, while the supervisor may still come back.
func (w *watcher) reopenAfterAppStart(ctx context.Context, st *state.State, gone time.Time, term string) bool {
	sup := st.Supervisor
	if w.stand.reopened == term {
		return w.now.Sub(w.stand.reopenedAt) < reopenGrace
	}
	if w.table == nil || sup.HostSession == "" {
		return false
	}
	liveAt := w.stand.liveAt
	if w.stand.liveTerm != term {
		liveAt = time.Time{}
	}
	if !reopenDue(plat.Opener.Running(w.table), gone, liveAt) {
		return false
	}
	w.stand.reopened, w.stand.reopenedAt = term, w.now
	if err := w.stand.open(ctx, continueURL(sup.HostSession), true); err != nil {
		w.emitNow("reopen", "REOPEN FAILED: %q: %v", sup.Name, err)
		return false
	}
	w.emitNow("reopen", "REOPENED: %q after the desktop app started", sup.Name)
	return true
}

// reopenDue says whether the desktop app, started at start (zero: it does
// not run), started after the supervisor's CLI stopped: after beekeeper
// first saw the CLI gone at gone, or with this watch never having seen the
// CLI run under it (liveAt, zero when never). After a reboot the app starts
// at login before the standby watch's first poll, which first sees gone a
// CLI that stopped with the machine.
func reopenDue(start, gone, liveAt time.Time) bool {
	return !start.IsZero() && (!start.Before(gone) || liveAt.Before(start))
}

// resumeRestarted records a CLI of rl's holder back under a new PID
// (standby watch) and tells it to resume the role: a restarted CLI has lost
// its watch. A fresh successor's desktop CLI, which follows the headless
// first turn that took the role, is such a restart. A headless CLI of
// beekeeper's is recorded and not told: a first turn ends once it took the
// role, and a headless resume's turn is the resume.
func (w *watcher) resumeRestarted(ctx context.Context, rl role, st *state.State, sessions []*claude.Session) {
	r := rl.get(st)
	if r.Holder == nil {
		return
	}
	if s, live := claude.Live(sessions, r.Holder.Party); !live || (r.CLI.Of(r.Holder) && r.CLI.PID == s.PID) {
		return
	}
	var restarted []state.Event
	var sup *state.Supervisor
	err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		_, evs := rl.observeCLI(st, sessions, w.now)
		restarted, sup = slices.DeleteFunc(slices.Clone(evs), func(e state.Event) bool { return e.Verb != rl.name+".restart" }), rl.get(st).Holder
		return evs, nil
	})
	if err != nil || len(restarted) == 0 || sup == nil {
		return
	}
	s, live := claude.Live(sessions, sup.Party)
	if !live || w.headless(s.PID) {
		return
	}
	_ = w.sendAsync(ctx, s.Name, rl.resumeMessage("your CLI restarted ("+restarted[0].Detail+") and its watch is gone"), func(err error) {
		if err != nil {
			w.emitNow(rl.name+"-resume", "%sRESUME FAILED: %q: %v", rl.tag, s.Name, err)
			return
		}
		w.emitNow(rl.name+"-resume", "%sRESUME: sent %q the hand-over from the command line (%s)", rl.tag, s.Name, restarted[0].Detail)
	})
}

// headless reports whether the CLI pid is a headless turn of beekeeper's
// (agents start's first turn, a wake) in the last poll's process table.
func (w *watcher) headless(pid int) bool {
	if w.table == nil {
		return false
	}
	p := w.table.ByPID[pid]
	return p != nil && printsTurn(p)
}

// sendAsync sends one message at a time from the command line, outside the
// poll: a send is a headless turn of 5 to 15 s. It reports whether the send
// started: one in flight drops it, and the next poll tries again.
func (w *watcher) sendAsync(ctx context.Context, to, msg string, done func(error)) bool {
	if !w.stand.busy.CompareAndSwap(false, true) {
		return false
	}
	go func() {
		defer w.stand.busy.Store(false)
		done(w.stand.send(ctx, to, msg))
	}()
	return true
}

// peerSend sends from the command line with the peer package.
func (a *app) peerSend(ctx context.Context, to, msg string) error {
	_, err := peer.Sender{Dir: a.cfg.StateDir}.Send(ctx, to, msg)
	if errors.Is(err, peer.ErrUnreachable) {
		return fmt.Errorf("%w: its CLI stopped", err)
	}
	return err
}
