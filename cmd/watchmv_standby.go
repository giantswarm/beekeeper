package cmd

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// resumeMessage tells a role's holder to take its role up again in a new
// CLI: the handover prompt itself is too long to relay through the
// sender's turn.
func (rl role) resumeMessage(why string) string {
	return "beekeeper: " + why + ". Run `" + rl.handover + "` and follow it."
}

// standbyWatch is what the standby watch remembers of the messages it sent
// and the guide's successors it started; a restarted watch starts afresh,
// and the relay a successor's start opens is in the state.
type standbyWatch struct {
	send func(ctx context.Context, to, msg string) error
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
	// session id runs its headless turn (unitsTurning); nil: none does.
	turning func(ctx context.Context, id string) bool
	// reopening reports whether a unit of beekeeper's start or wake of
	// session id runs its turn, or a reopen of it runs (unitsReopening);
	// nil: none does.
	reopening func(ctx context.Context, id string) bool
	busy      atomic.Bool
	// guideGap is the term of the gone guide this watch said.
	guideGap string
	// starting is the role whose successor is being started, one at a time;
	// inflight waits for it (a watch --once).
	starting atomic.Bool
	inflight sync.WaitGroup
	// loopUnit is the supervisor loop's service (launchedLoop); nil: the
	// watch restarts nothing.
	loopUnit *loopService
	// loopRestart is when the watch last restarted the supervisor loop's
	// service; loopDown is the loop's gap it said, since loopSince.
	loopRestart time.Time
	loopDown    bool
	loopSince   time.Time
	// overdueNext is when the next relay of a holder over its relayAt may
	// start, by role name.
	overdueNext map[string]time.Time
	// importRow gives a session that runs headless with no desktop row its
	// row (app.importRowFromWatch); nil: rows are not imported.
	importRow func(ctx context.Context, id, name string) (string, error)
	// importing holds the sessions whose import runs, by session id.
	importing sync.Map
}

// firstTurn reports whether p's session runs the headless turn of
// beekeeper's start or wake, before its CLI shows among the sessions. The
// reopen after the turn is none: it waits while the desktop's window has
// the focus, up to reopenAwayWait, so the holder's restart grace covers it
// and past the grace the standby resumes the holder headless; but a reopen
// of a holder that asked for a desktop turn (agents desktop) goes ahead at
// once, and counts.
func (w *watcher) firstTurn(ctx context.Context, st *state.State, p state.Party) bool {
	if p.Session == "" {
		return false
	}
	if i := st.StartIndex(p); i >= 0 && st.Starts[i].Background {
		return false // background starts/resumes have no headless service turn
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if w.stand.turning != nil && w.stand.turning(ctx, p.Session) {
		return true
	}
	if ctx.Err() != nil {
		return true // an unread unit is not evidence that its turn ended
	}
	i := agentOfSession(st, p.Session)
	reopening := i >= 0 && !st.Agents[i].DesktopTurn.IsZero() && w.stand.reopening != nil && w.stand.reopening(ctx, p.Session)
	return reopening || ctx.Err() != nil
}

// guideGone says once when the guide's CLI stayed gone past its grace with
// no relay open, and starts its successor (standby watch).
func (w *watcher) guideGone(ctx context.Context, st *state.State, sessions []*claude.Session) {
	r := guideRole.get(st)
	if goneUnrecorded(r, sessions) {
		// The guide's own watch, which records its CLI, went with it: the
		// standby records since when it is gone, or the grace would start
		// anew on every poll and no successor would ever start.
		err := w.a.store.UpdateWithin(time.Second, func(st *state.State) ([]state.Event, error) {
			_, evs := guideRole.observeCLI(st, sessions, w.a.now)
			r = guideRole.get(st)
			return evs, nil
		})
		if err != nil {
			w.fail("guide-observe", "cannot record the missing guide CLI: %v", err)
			return
		}
		w.clear("guide-observe")
	}
	sv := readHolder(r, sessions, w.a.now, w.a.cfg.Guide.RestartGrace.Duration)
	if !sv.down() || relayPending(st, r, w.a.now) || w.firstTurn(ctx, st, r.Holder.Party) {
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
		w.emitNow("guide", "GUIDE GONE: %q (guiding since %s) is gone since %s%s", r.Holder.Name, clock(w.a.now, r.Holder.Since), clock(w.a.now, sv.gone), successor)
	}
}

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
// not warm it: its reopen waits while the person works in the desktop's
// window), is resumed headless: that turn arms the role's watch and keeps
// its CLI, so no further successor starts while it runs, and a resume seen
// running ends the chain (upAgain), so the next gap resumes it again until
// the desktop runs its CLI. One whose resume never ran failed: one note
// after the first, and the next successor waits out the backoff; past
// maxFailedSuccessors none starts.
// Any other holder gets its successor at once. It returns what the GONE
// line adds.
func (w *watcher) standIn(ctx context.Context, rl role, st *state.State, holder state.Party, key string) string {
	if st.Stop != nil {
		return "; " + stoppedNothing
	}
	if w.stand.starting.Load() {
		return successorStarting
	}
	c := w.chain(rl)
	if started(st, holder) {
		if c.revived != key && w.stand.revive != nil {
			c.revived = key
			return w.reviveGone(ctx, rl, holder, w.a.backgroundAgent(ctx, st, holder))
		}
		if c.failed != key {
			c.failed, c.failures = key, c.failures+1
			c.next = w.a.now.Add(successorBackoff << (c.failures - 1))
			if c.failures == 1 {
				c.note = w.noteFailedSuccessor(rl, holder)
			}
			w.emitNow(rl.name+"-successor", "%sSUCCESSOR DOWN: %q did not come up%s", rl.tag, holder.Name, c.outlook(w.a.now))
		}
	}
	if c.failures >= maxFailedSuccessors || w.a.now.Before(c.next) {
		return c.outlook(w.a.now)
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

// reviveGone resumes rl's gone holder headless outside the poll, as
// succeedGone starts a successor; a background holder is continued by
// claude --bg --resume. It returns what the GONE line adds.
func (w *watcher) reviveGone(ctx context.Context, rl role, holder state.Party, background bool) string {
	if !w.stand.starting.CompareAndSwap(false, true) {
		return successorStarting
	}
	w.stand.inflight.Add(1)
	if background {
		go func() {
			defer w.stand.inflight.Done()
			defer w.stand.starting.Store(false)
			msg := rl.resumeMessage("your background session was stopped and runs again, continued by this message: this turn keeps " + rl.duty)
			if err := w.stand.revive(ctx, rl, holder, msg); err != nil {
				w.emitNow(rl.name+"-successor", "%sRESUME FAILED: %q runs no CLI and claude --bg --resume did not continue it: %v", rl.tag, holder.Name, err)
				return
			}
			w.emitNow(rl.name+"-successor", "%sRESUME: %q ran no CLI: claude --bg --resume continued its background session, its turn keeps %s", rl.tag, holder.Name, rl.duty)
		}()
		return "; continuing its background session (it runs no CLI)"
	}
	go func() {
		defer w.stand.inflight.Done()
		defer w.stand.starting.Store(false)
		msg := rl.resumeMessage("the desktop runs your CLI now, started by this message: this desktop turn keeps " + rl.duty)
		if err := w.stand.revive(ctx, rl, holder, msg); err != nil {
			w.emitNow(rl.name+"-successor", "%sRESUME FAILED: %q has no desktop CLI and the desktop's send did not start one: %v", rl.tag, holder.Name, err)
			return
		}
		w.emitNow(rl.name+"-successor", "%sRESUME: %q had no desktop CLI: the desktop's send started one, its turn keeps %s", rl.tag, holder.Name, rl.duty)
	}()
	return "; having the desktop start its CLI (it runs none)"
}

// noteFailedSuccessor runs in the command, which holds what it needs (watchNoteFailedSuccessor).
func (w *watcher) noteFailedSuccessor(rl role, holder state.Party) int {
	return watchNoteFailedSuccessor(w, rl, holder)
}

// succeedGone starts rl's next run once its holder's CLI stayed gone past
// the grace (standby watch): the successor's start takes the role through
// the relay beekeeper opens to it, so a repeated poll and a restarted watch
// see the relay and start none. It returns what the GONE line adds.
func (w *watcher) succeedGone(ctx context.Context, rl role, holder state.Party) string {
	started := w.succeedAsync(ctx, rl, holder, func(to state.Party, err error) {
		if err != nil {
			w.emitNow(rl.name+"-successor", "%sSUCCESSOR FAILED: %q is gone and its successor did not start: %v", rl.tag, holder.Name, err)
			return
		}
		w.emitNow(rl.name+"-successor", "%sSUCCESSOR: started %q, whose `beekeeper %s start` takes the role from the gone %q", rl.tag, to.Name, rl.name, holder.Name)
	})
	switch {
	case w.stand.succeed == nil:
		return ""
	case !started:
		return successorStarting
	}
	return "; starting its successor"
}

// succeedAsync starts rl's next run after holder outside the poll, one at a
// time, and hands done the outcome. It reports whether the start began:
// none does without stand.succeed or while another runs.
func (w *watcher) succeedAsync(ctx context.Context, rl role, holder state.Party, done func(state.Party, error)) bool {
	if w.stand.succeed == nil || !w.stand.starting.CompareAndSwap(false, true) {
		return false
	}
	w.stand.inflight.Add(1)
	go func() {
		defer w.stand.inflight.Done()
		defer w.stand.starting.Store(false)
		done(w.stand.succeed(ctx, rl, holder))
	}()
	return true
}

// relayOverdue relays rl's live holder that stayed over its relayAt for
// relayGrace after its relay due (standby watch): it starts the successor
// as the holder's own relay does, and the successor's start takes the role.
// The hand-over is no one's decision; a start that did not begin or failed
// is tried again after successorBackoff.
func (w *watcher) relayOverdue(ctx context.Context, rl role, st *state.State, sessions []*claude.Session) {
	cfg, r := rl.cfg(w.a.cfg), rl.get(st)
	if st.Stop != nil || !pastRelayGrace(r, cfg, w.a.now) || w.a.now.Before(w.stand.overdueNext[rl.name]) {
		return
	}
	c := sessionContext(sessions, r.Holder.Party)
	if c < int64(cfg.RelayAt) {
		return
	}
	if w.stand.overdueNext == nil {
		w.stand.overdueNext = map[string]time.Time{}
	}
	w.stand.overdueNext[rl.name] = w.a.now.Add(successorBackoff)
	holder, since := r.Holder.Party, clock(w.a.now, r.RelayDue.Reported)
	w.succeedAsync(ctx, rl, holder, func(to state.Party, err error) {
		if err != nil {
			w.emitNow(rl.name+"-relayed", "%sRELAY FAILED: %q is at %s tokens of context, over %s since its relay due at %s, and its successor did not start: %v",
				rl.tag, holder.Name, tokensText(c), tokensText(int64(cfg.RelayAt)), since, err)
			return
		}
		w.emitNow(rl.name+"-relayed", "%sRELAYED: %q is at %s tokens of context, over %s since its relay due at %s: started %q, whose `beekeeper %s start` takes the role",
			rl.tag, holder.Name, tokensText(c), tokensText(int64(cfg.RelayAt)), since, to.Name, rl.name)
	})
}

// pastRelayGrace reports whether r's holder was told its relay due at the
// configured relayAt at least relayGrace ago, with no relay open.
func pastRelayGrace(r state.Role, cfg config.Role, now time.Time) bool {
	return r.Holder != nil && !r.Relay.Open(now) && r.RelayDue.At(r.Holder, int64(cfg.RelayAt)) &&
		!now.Before(r.RelayDue.Reported.Add(cfg.RelayGrace.Duration))
}

// resumeRestarted records a CLI of rl's holder back under a new PID
// (standby watch) and tells it to resume the role: a restarted CLI has lost
// its watch. A fresh successor's desktop CLI, which follows the headless
// first turn that took the role, is such a restart. A headless CLI of
// beekeeper's is recorded and not told: a first turn ends once it took the
// role, and a headless resume's turn is the resume.
func (w *watcher) resumeRestarted(ctx context.Context, rl role, st *state.State, sessions []*claude.Session) {
	r := rl.get(st)
	if r.Holder == nil || st.Stop != nil {
		return
	}
	if s, live := claude.Live(sessions, r.Holder.Party); !live || (r.CLI.Of(r.Holder) && r.CLI.PID == s.PID) {
		return
	}
	var restarted []state.Event
	var sup *state.Supervisor
	err := w.a.store.Update(func(st *state.State) ([]state.Event, error) {
		_, evs := rl.observeCLI(st, sessions, w.a.now)
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
	w.stand.inflight.Add(1)
	go func() {
		defer w.stand.inflight.Done()
		defer w.stand.busy.Store(false)
		done(w.stand.send(ctx, to, msg))
	}()
	return true
}
