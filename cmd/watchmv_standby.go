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

// ResumeMessage tells a role's holder to take its role up again in a new
// CLI: the handover prompt itself is too long to relay through the
// sender's turn.
func (rl role) ResumeMessage(why string) string {
	return "beekeeper: " + why + ". Run `" + rl.Handover + "` and follow it."
}

// standbyWatch is what the standby watch remembers of the messages it sent
// and the guide's successors it started; a restarted watch starts afresh,
// and the relay a successor's start opens is in the state.
type standbyWatch struct {
	Send func(ctx context.Context, to, msg string) error
	// succeed starts rl's next run after its holder from is gone
	// (app.startSuccessor).
	Succeed func(ctx context.Context, rl role, from state.Party) (state.Party, error)
	// revive resumes rl's holder headless with msg (app.wakeAgent): a
	// successor whose first turn ended and whose desktop CLI never came.
	Revive func(ctx context.Context, rl role, holder state.Party, msg string) error
	// chains are the successors of each role, by its name, that did not
	// come up.
	Chains map[string]*successorChain
	// turning reports whether a unit of beekeeper's start or wake of
	// session id runs its headless turn (unitsTurning); nil: none does.
	Turning func(ctx context.Context, id string) bool
	// reopening reports whether a unit of beekeeper's start or wake of
	// session id runs its turn, or a reopen of it runs (unitsReopening);
	// nil: none does.
	Reopening func(ctx context.Context, id string) bool
	Busy      atomic.Bool
	// guideGap is the term of the gone guide this watch said.
	guideGap string
	// starting is the role whose successor is being started, one at a time;
	// inflight waits for it (a watch --once).
	Starting atomic.Bool
	Inflight sync.WaitGroup
	// loopUnit is the supervisor loop's service (launchedLoop); nil: the
	// watch restarts nothing.
	LoopUnit *LoopService
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
	ImportRow func(ctx context.Context, id, name string) (string, error)
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
func (w *Watcher) firstTurn(ctx context.Context, st *state.State, p state.Party) bool {
	if p.Session == "" {
		return false
	}
	if i := st.StartIndex(p); i >= 0 && st.Starts[i].Background {
		return false // background starts/resumes have no headless service turn
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if w.Stand.Turning != nil && w.Stand.Turning(ctx, p.Session) {
		return true
	}
	if ctx.Err() != nil {
		return true // an unread unit is not evidence that its turn ended
	}
	i := agentOfSession(st, p.Session)
	reopening := i >= 0 && !st.Agents[i].DesktopTurn.IsZero() && w.Stand.Reopening != nil && w.Stand.Reopening(ctx, p.Session)
	return reopening || ctx.Err() != nil
}

// GuideGone says once when the guide's CLI stayed gone past its grace with
// no relay open, and starts its successor (standby watch).
func (w *Watcher) GuideGone(ctx context.Context, st *state.State, sessions []*claude.Session) {
	r := guideRole.Get(st)
	if goneUnrecorded(r, sessions) {
		// The guide's own watch, which records its CLI, went with it: the
		// standby records since when it is gone, or the grace would start
		// anew on every poll and no successor would ever start.
		err := w.Host.store.UpdateWithin(time.Second, func(st *state.State) ([]state.Event, error) {
			_, evs := guideRole.ObserveCLI(st, sessions, w.Host.now)
			r = guideRole.Get(st)
			return evs, nil
		})
		if err != nil {
			w.Fail("guide-observe", "cannot record the missing guide CLI: %v", err)
			return
		}
		w.Clear("guide-observe")
	}
	sv := readHolder(r, sessions, w.Host.now, w.Host.cfg.Guide.RestartGrace.Duration)
	if !sv.Down() || relayPending(st, r, w.Host.now) || w.firstTurn(ctx, st, r.Holder.Party) {
		if sv.Live {
			w.Stand.guideGap = ""
			w.upAgain(guideRole, r.Holder.Party)
		}
		return
	}
	key := r.Holder.Name + "@" + r.Holder.Since.UTC().Format(time.RFC3339)
	successor := w.standIn(ctx, guideRole, st, r.Holder.Party, key)
	if w.Stand.guideGap != key {
		w.Stand.guideGap = key
		w.EmitNow("guide", "GUIDE GONE: %q (guiding since %s) is gone since %s%s", r.Holder.Name, clock(w.Host.now, r.Holder.Since), clock(w.Host.now, sv.Gone), successor)
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
	Failures        int
	Next            time.Time
	note            int
}

// chain is rl's successorChain.
func (w *Watcher) chain(rl role) *successorChain {
	if w.Stand.Chains == nil {
		w.Stand.Chains = map[string]*successorChain{}
	}
	c := w.Stand.Chains[rl.Name]
	if c == nil {
		c = &successorChain{}
		w.Stand.Chains[rl.Name] = c
	}
	return c
}

// upAgain forgets rl's failed successors once its live holder's CLI is no
// start's first turn: a successor whose desktop CLI or headless resume
// runs, or any holder that is no successor.
func (w *Watcher) upAgain(rl role, holder state.Party) {
	if headlessTurn(w.Table, holder.Session) == "first turn" {
		return
	}
	if c := w.Stand.Chains[rl.Name]; c != nil && c.Failures > 0 {
		w.EmitNow(rl.Name+"-successor", "%sSUCCESSOR UP: the %s runs again after %d successor(s) that did not come up", rl.Tag, rl.Name, c.Failures)
	}
	delete(w.Stand.Chains, rl.Name)
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
func (w *Watcher) standIn(ctx context.Context, rl role, st *state.State, holder state.Party, key string) string {
	if st.Stop != nil {
		return "; " + stoppedNothing
	}
	if w.Stand.Starting.Load() {
		return successorStarting
	}
	c := w.chain(rl)
	if started(st, holder) {
		if c.revived != key && w.Stand.Revive != nil {
			c.revived = key
			return w.reviveGone(ctx, rl, holder, w.Host.backgroundAgent(ctx, st, holder))
		}
		if c.failed != key {
			c.failed, c.Failures = key, c.Failures+1
			c.Next = w.Host.now.Add(successorBackoff << (c.Failures - 1))
			if c.Failures == 1 {
				c.note = w.noteFailedSuccessor(rl, holder)
			}
			w.EmitNow(rl.Name+"-successor", "%sSUCCESSOR DOWN: %q did not come up%s", rl.Tag, holder.Name, c.outlook(w.Host.now))
		}
	}
	if c.Failures >= maxFailedSuccessors || w.Host.now.Before(c.Next) {
		return c.outlook(w.Host.now)
	}
	return w.succeedGone(ctx, rl, holder)
}

// outlook says what follows c's failed successors.
func (c *successorChain) outlook(now time.Time) string {
	note := ""
	if c.note > 0 {
		note = fmt.Sprintf(" (note #%d)", c.note)
	}
	if c.Failures >= maxFailedSuccessors {
		return fmt.Sprintf("; %d successors did not come up, none further starts%s", c.Failures, note)
	}
	return fmt.Sprintf("; %d successor(s) did not come up, the next starts at %s%s", c.Failures, clock(now, c.Next), note)
}

// reviveGone resumes rl's gone holder headless outside the poll, as
// succeedGone starts a successor; a background holder is continued by
// claude --bg --resume. It returns what the GONE line adds.
func (w *Watcher) reviveGone(ctx context.Context, rl role, holder state.Party, background bool) string {
	if !w.Stand.Starting.CompareAndSwap(false, true) {
		return successorStarting
	}
	w.Stand.Inflight.Add(1)
	if background {
		go func() {
			defer w.Stand.Inflight.Done()
			defer w.Stand.Starting.Store(false)
			msg := rl.ResumeMessage("your background session was stopped and runs again, continued by this message: this turn keeps " + rl.Duty)
			if err := w.Stand.Revive(ctx, rl, holder, msg); err != nil {
				w.EmitNow(rl.Name+"-successor", "%sRESUME FAILED: %q runs no CLI and claude --bg --resume did not continue it: %v", rl.Tag, holder.Name, err)
				return
			}
			w.EmitNow(rl.Name+"-successor", "%sRESUME: %q ran no CLI: claude --bg --resume continued its background session, its turn keeps %s", rl.Tag, holder.Name, rl.Duty)
		}()
		return "; continuing its background session (it runs no CLI)"
	}
	go func() {
		defer w.Stand.Inflight.Done()
		defer w.Stand.Starting.Store(false)
		msg := rl.ResumeMessage("the desktop runs your CLI now, started by this message: this desktop turn keeps " + rl.Duty)
		if err := w.Stand.Revive(ctx, rl, holder, msg); err != nil {
			w.EmitNow(rl.Name+"-successor", "%sRESUME FAILED: %q has no desktop CLI and the desktop's send did not start one: %v", rl.Tag, holder.Name, err)
			return
		}
		w.EmitNow(rl.Name+"-successor", "%sRESUME: %q had no desktop CLI: the desktop's send started one, its turn keeps %s", rl.Tag, holder.Name, rl.Duty)
	}()
	return "; having the desktop start its CLI (it runs none)"
}

// noteFailedSuccessor runs in the command, which holds what it needs (watchNoteFailedSuccessor).
func (w *Watcher) noteFailedSuccessor(rl role, holder state.Party) int {
	return watchNoteFailedSuccessor(w, rl, holder)
}

// succeedGone starts rl's next run once its holder's CLI stayed gone past
// the grace (standby watch): the successor's start takes the role through
// the relay beekeeper opens to it, so a repeated poll and a restarted watch
// see the relay and start none. It returns what the GONE line adds.
func (w *Watcher) succeedGone(ctx context.Context, rl role, holder state.Party) string {
	started := w.succeedAsync(ctx, rl, holder, func(to state.Party, err error) {
		if err != nil {
			w.EmitNow(rl.Name+"-successor", "%sSUCCESSOR FAILED: %q is gone and its successor did not start: %v", rl.Tag, holder.Name, err)
			return
		}
		w.EmitNow(rl.Name+"-successor", "%sSUCCESSOR: started %q, whose `beekeeper %s start` takes the role from the gone %q", rl.Tag, to.Name, rl.Name, holder.Name)
	})
	switch {
	case w.Stand.Succeed == nil:
		return ""
	case !started:
		return successorStarting
	}
	return "; starting its successor"
}

// succeedAsync starts rl's next run after holder outside the poll, one at a
// time, and hands done the outcome. It reports whether the start began:
// none does without stand.succeed or while another runs.
func (w *Watcher) succeedAsync(ctx context.Context, rl role, holder state.Party, done func(state.Party, error)) bool {
	if w.Stand.Succeed == nil || !w.Stand.Starting.CompareAndSwap(false, true) {
		return false
	}
	w.Stand.Inflight.Add(1)
	go func() {
		defer w.Stand.Inflight.Done()
		defer w.Stand.Starting.Store(false)
		done(w.Stand.Succeed(ctx, rl, holder))
	}()
	return true
}

// RelayOverdue relays rl's live holder that stayed over its relayAt for
// relayGrace after its relay due (standby watch): it starts the successor
// as the holder's own relay does, and the successor's start takes the role.
// The hand-over is no one's decision; a start that did not begin or failed
// is tried again after successorBackoff.
func (w *Watcher) RelayOverdue(ctx context.Context, rl role, st *state.State, sessions []*claude.Session) {
	cfg, r := rl.Cfg(w.Host.cfg), rl.Get(st)
	if st.Stop != nil || !PastRelayGrace(r, cfg, w.Host.now) || w.Host.now.Before(w.Stand.overdueNext[rl.Name]) {
		return
	}
	c := sessionContext(sessions, r.Holder.Party)
	if c < int64(cfg.RelayAt) {
		return
	}
	if w.Stand.overdueNext == nil {
		w.Stand.overdueNext = map[string]time.Time{}
	}
	w.Stand.overdueNext[rl.Name] = w.Host.now.Add(successorBackoff)
	holder, since := r.Holder.Party, clock(w.Host.now, r.RelayDue.Reported)
	w.succeedAsync(ctx, rl, holder, func(to state.Party, err error) {
		if err != nil {
			w.EmitNow(rl.Name+"-relayed", "%sRELAY FAILED: %q is at %s tokens of context, over %s since its relay due at %s, and its successor did not start: %v",
				rl.Tag, holder.Name, tokensText(c), tokensText(int64(cfg.RelayAt)), since, err)
			return
		}
		w.EmitNow(rl.Name+"-relayed", "%sRELAYED: %q is at %s tokens of context, over %s since its relay due at %s: started %q, whose `beekeeper %s start` takes the role",
			rl.Tag, holder.Name, tokensText(c), tokensText(int64(cfg.RelayAt)), since, to.Name, rl.Name)
	})
}

// PastRelayGrace reports whether r's holder was told its relay due at the
// configured relayAt at least relayGrace ago, with no relay open.
func PastRelayGrace(r state.Role, cfg config.Role, now time.Time) bool {
	return r.Holder != nil && !r.Relay.Open(now) && r.RelayDue.At(r.Holder, int64(cfg.RelayAt)) &&
		!now.Before(r.RelayDue.Reported.Add(cfg.RelayGrace.Duration))
}

// ResumeRestarted records a CLI of rl's holder back under a new PID
// (standby watch) and tells it to resume the role: a restarted CLI has lost
// its watch. A fresh successor's desktop CLI, which follows the headless
// first turn that took the role, is such a restart. A headless CLI of
// beekeeper's is recorded and not told: a first turn ends once it took the
// role, and a headless resume's turn is the resume.
func (w *Watcher) ResumeRestarted(ctx context.Context, rl role, st *state.State, sessions []*claude.Session) {
	r := rl.Get(st)
	if r.Holder == nil || st.Stop != nil {
		return
	}
	if s, live := claude.Live(sessions, r.Holder.Party); !live || (r.CLI.Of(r.Holder) && r.CLI.PID == s.PID) {
		return
	}
	var restarted []state.Event
	var sup *state.Supervisor
	err := w.Host.store.Update(func(st *state.State) ([]state.Event, error) {
		_, evs := rl.ObserveCLI(st, sessions, w.Host.now)
		restarted, sup = slices.DeleteFunc(slices.Clone(evs), func(e state.Event) bool { return e.Verb != rl.Name+".restart" }), rl.Get(st).Holder
		return evs, nil
	})
	if err != nil || len(restarted) == 0 || sup == nil {
		return
	}
	s, live := claude.Live(sessions, sup.Party)
	if !live || w.headless(s.PID) {
		return
	}
	_ = w.SendAsync(ctx, s.Name, rl.ResumeMessage("your CLI restarted ("+restarted[0].Detail+") and its watch is gone"), func(err error) {
		if err != nil {
			w.EmitNow(rl.Name+"-resume", "%sRESUME FAILED: %q: %v", rl.Tag, s.Name, err)
			return
		}
		w.EmitNow(rl.Name+"-resume", "%sRESUME: sent %q the hand-over from the command line (%s)", rl.Tag, s.Name, restarted[0].Detail)
	})
}

// headless reports whether the CLI pid is a headless turn of beekeeper's
// (agents start's first turn, a wake) in the last poll's process table.
func (w *Watcher) headless(pid int) bool {
	if w.Table == nil {
		return false
	}
	p := w.Table.ByPID[pid]
	return p != nil && printsTurn(p)
}

// SendAsync sends one message at a time from the command line, outside the
// poll: a send is a headless turn of 5 to 15 s. It reports whether the send
// started: one in flight drops it, and the next poll tries again.
func (w *Watcher) SendAsync(ctx context.Context, to, msg string, done func(error)) bool {
	if !w.Stand.Busy.CompareAndSwap(false, true) {
		return false
	}
	w.Stand.Inflight.Add(1)
	go func() {
		defer w.Stand.Inflight.Done()
		defer w.Stand.Busy.Store(false)
		done(w.Stand.Send(ctx, to, msg))
	}()
	return true
}
