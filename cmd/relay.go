package cmd

import (
	"fmt"
	"slices"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The supervisor role moves in two steps: the running supervisor names its
// successor (relay), the successor takes the role with its own start. Both
// are one state update under the lock each, so the grant rule is in force
// throughout: the outgoing supervisor's until the start, the successor's
// from it.

// startRole makes me the supervisor in st: through a relay that names me,
// over a recorded supervisor whose session is gone, or with takeOver.
// prevLive says whether the recorded supervisor's session runs.
func startRole(st *state.State, me state.Party, prevLive, takeOver bool, now time.Time) (string, []state.Event, error) {
	prev := st.Supervisor
	how := ""
	switch {
	case prev == nil || prev.Is(me):
		// A restart of the same supervisor keeps an open relay.
	case st.Relay.Open(now) && st.Relay.From.Is(prev.Party) && st.Relay.To.Is(me):
		for i := range st.Grants {
			st.Grants[i].By = me
		}
		st.Relay.Taken = now.UTC()
		st.Relieved = append(dropRelief(st.Relieved, prev.Party),
			state.Relief{Party: prev.Party, By: me, At: st.Relay.At, Taken: st.Relay.Taken})
		how = fmt.Sprintf(" (relieving %q, relayed at %s; %d grant(s) move over)", prev.Name, clock(now, st.Relay.At), len(st.Grants))
	case prevLive && !takeOver:
		named := ""
		switch r := st.Relay; {
		case r.Open(now) && r.From.Is(prev.Party):
			named = fmt.Sprintf("; its relay names %q, not you", r.To.Name)
		case r != nil && r.Taken.IsZero() && r.From.Is(prev.Party) && r.To.Is(me):
			named = fmt.Sprintf("; its relay to you expired at %s", clock(now, r.Expires))
		}
		return "", nil, refused("%q supervises since %s and still runs%s: it relays the role with `beekeeper supervisor relay <you>`, or take over with --take-over",
			prev.Name, clock(now, prev.Since), named)
	default:
		st.Relay = nil
		how = fmt.Sprintf(" (taking over from %q)", prev.Name)
	}
	// Supervising again ends my relief; a relief nobody asked about for
	// reliefTTL is dropped.
	st.Relieved = slices.DeleteFunc(dropRelief(st.Relieved, me), func(r state.Relief) bool { return now.Sub(r.Taken) > reliefTTL })
	st.Supervisor = &state.Supervisor{Party: me, Since: now.UTC()}
	msg := fmt.Sprintf("%q supervises now%s", me.Name, how)
	return msg, []state.Event{event(me, "supervisor.start", "%s", msg)}, nil
}

// relayRole records the supervisor me naming its successor to; the relay
// stays open for ttl.
func relayRole(st *state.State, me, to state.Party, now time.Time, ttl time.Duration) (string, []state.Event, error) {
	if err := mustSupervise(st, me); err != nil {
		return "", nil, err
	}
	if to.Is(me) {
		return "", nil, usageErr("you supervise already: name the session that takes over")
	}
	replacing := ""
	if st.Relay.Open(now) && !st.Relay.To.Is(to) {
		replacing = fmt.Sprintf(", replacing the relay to %q", st.Relay.To.Name)
	}
	st.Relay = &state.Relay{From: me, To: to, At: now.UTC(), Expires: now.Add(ttl).UTC()}
	until := clock(now, st.Relay.Expires)
	msg := fmt.Sprintf("relayed to %q until %s%s: its `beekeeper supervisor start` takes the role and the grants; you supervise until then (`beekeeper supervisor status` exits %d once you are relieved)",
		to.Name, until, replacing, ExitRelieved)
	return msg, []state.Event{event(me, "supervisor.relay", "to %s until %s%s", to.Name, until, replacing)}, nil
}

// cancelRelay withdraws the supervisor's open relay.
func cancelRelay(st *state.State, me state.Party, now time.Time) (string, []state.Event, error) {
	if err := mustSupervise(st, me); err != nil {
		return "", nil, err
	}
	if !st.Relay.Open(now) {
		return "no relay is open: you supervise", nil, nil
	}
	to := st.Relay.To.Name
	st.Relay, st.RelayDue = nil, nil
	return fmt.Sprintf("relay to %q cancelled: you still supervise", to),
		[]state.Event{event(me, "supervisor.relay-cancel", "to %s", to)}, nil
}

func mustSupervise(st *state.State, me state.Party) error {
	switch {
	case st.Supervisor == nil:
		return refused("no supervisor is recorded: `beekeeper supervisor start` makes you one")
	case !st.Supervisor.Is(me):
		return refused("%q supervises, not you: only the supervisor relays its role", st.Supervisor.Name)
	}
	return nil
}

// reliefTTL is how long a relieved supervisor's status keeps exiting 4.
const reliefTTL = 7 * 24 * time.Hour

func dropRelief(rs []state.Relief, p state.Party) []state.Relief {
	return slices.DeleteFunc(rs, func(r state.Relief) bool { return r.Party.Is(p) })
}

// relievedBy returns the relief of a relay that relieved me, nil when none
// did or I supervise again. It survives the successor's own relays.
func relievedBy(st *state.State, me state.Party) *state.Relief {
	if st.Supervisor != nil && st.Supervisor.Is(me) {
		return nil
	}
	for i := range st.Relieved {
		if st.Relieved[i].Party.Is(me) {
			return &st.Relieved[i]
		}
	}
	return nil
}

// supervision is the recorded supervisor read against the running
// sessions and the grace a CLI restart has. Its grant rule is in force in
// every state: live, restarting and gone.
type supervision struct {
	sup  *state.Supervisor
	live bool
	// gone is when beekeeper first saw the CLI gone (now when no record
	// says so yet), zero while it runs; until is the end of the grace a
	// restart has, zero while it runs and once the grace has passed.
	gone, until time.Time
}

// readSupervision reads st's supervisor: live while its CLI runs,
// restarting for grace after beekeeper first saw its CLI gone, and gone
// after that.
func readSupervision(st *state.State, sessions []*claude.Session, now time.Time, grace time.Duration) supervision {
	v := supervision{sup: st.Supervisor}
	if v.sup == nil {
		return v
	}
	if _, v.live = claude.Live(sessions, v.sup.Party); v.live {
		return v
	}
	v.gone = now
	if c := st.SupervisorCLI; c.Of(v.sup) && !c.Gone.IsZero() {
		v.gone = c.Gone
	}
	if until := v.gone.Add(grace); now.Before(until) {
		v.until = until
	}
	return v
}

// restarting reports whether the supervisor's CLI is gone within the grace.
func (v supervision) restarting() bool { return !v.until.IsZero() }

// down reports whether the recorded supervisor's CLI stayed gone past the
// grace: its successor is due.
func (v supervision) down() bool { return v.sup != nil && !v.live && !v.restarting() }

// observeCLI records what the running sessions say of the supervisor's
// CLI in this term: the PID it runs as, and since when it is gone. The same
// session back with a new PID is a restart, logged once. It reports whether
// st changed.
func observeCLI(st *state.State, sessions []*claude.Session, now time.Time) (bool, []state.Event) {
	sup := st.Supervisor
	if sup == nil {
		changed := st.SupervisorCLI != nil
		st.SupervisorCLI = nil
		return changed, nil
	}
	changed := false
	c := st.SupervisorCLI
	if !c.Of(sup) {
		c = &state.CLI{Supervisor: sup.Party, Since: sup.Since}
		st.SupervisorCLI, changed = c, true
	}
	s, live := claude.Live(sessions, sup.Party)
	switch {
	case live && s.PID != c.PID:
		var evs []state.Event
		if c.PID != 0 {
			away := "unseen"
			if !c.Gone.IsZero() {
				away = "gone for " + dur(now.Sub(c.Gone))
			}
			evs = append(evs, event(sup.Party, "supervisor.restart", "CLI %d back as %d (%s)", c.PID, s.PID, away))
		}
		c.PID, c.Gone = s.PID, time.Time{}
		return true, evs
	case live && !c.Gone.IsZero():
		c.Gone = time.Time{}
		return true, nil
	case !live && c.Gone.IsZero():
		c.Gone = now.UTC()
		return true, nil
	}
	return changed, nil
}

// fireRelay reports a relay taken or expired, once.
func fireRelay(st *state.State, now time.Time) ([]string, []state.Event) {
	r := st.Relay
	if r == nil || !r.Reported.IsZero() {
		return nil, nil
	}
	var line string
	var evs []state.Event
	switch {
	case !r.Taken.IsZero():
		line = fmt.Sprintf("RELAY TAKEN: %q supervises since %s, relieving %q", r.To.Name, clock(now, r.Taken), r.From.Name)
	case !now.Before(r.Expires):
		line = fmt.Sprintf("RELAY EXPIRED: %q did not take the role from %q by %s (beekeeper supervisor relay <successor> opens another)",
			r.To.Name, r.From.Name, clock(now, r.Expires))
		evs = append(evs, event(watchParty, "supervisor.relay-expired", "to %s at %s", r.To.Name, clock(now, r.Expires)))
		st.RelayDue = nil
	default:
		return nil, nil
	}
	r.Reported = now.UTC()
	return []string{line}, evs
}

// relayContext is the running supervisor's context in tokens once it has
// reached relayAt, with no relay open and the relay due not reported yet to
// its term; 0 otherwise. Only then does the watch ask whether the machine is
// quiet.
func relayContext(st *state.State, sessions []*claude.Session, now time.Time, relayAt config.Tokens) int64 {
	sup := st.Supervisor
	if sup == nil || st.Relay.Open(now) || st.RelayDue.Of(sup) {
		return 0
	}
	if c := sessionContext(sessions, sup.Party, now); c >= int64(relayAt) {
		return c
	}
	return 0
}

// sessionContext is the context in tokens of p's running session, read from
// its transcript's last request (the CTX column); 0 when it does not run.
func sessionContext(sessions []*claude.Session, p state.Party, now time.Time) int64 {
	s, live := claude.Live(sessions, p)
	if !live || s.Transcript == "" {
		return 0
	}
	_, a := claude.ReadTranscript(s.Transcript, now)
	return a.Context
}

// quietness is the watch's reading of the machine for a relay: checked once
// the supervisor's context reached relayAt (context), busy saying what keeps
// it from a quiet moment ("" when quiet).
type quietness struct {
	checked bool
	busy    string
	context int64
}

// fireRelayDue reports the relay due at the first quiet moment after the
// supervisor's context reached relayAt, once per supervisor term.
func fireRelayDue(st *state.State, q quietness, now time.Time) ([]string, []state.Event) {
	sup := st.Supervisor
	if !q.checked || q.busy != "" || sup == nil || st.Relay.Open(now) || st.RelayDue.Of(sup) {
		return nil, nil
	}
	st.RelayDue = &state.RelayDue{Supervisor: sup.Party, Since: sup.Since, Reported: now.UTC(), Context: q.context}
	line := fmt.Sprintf("RELAY DUE: %q is at %s tokens of context: beekeeper handover --prompt", sup.Name, tokensText(q.context))
	return []string{line}, []state.Event{event(watchParty, "supervisor.relay-due", "%s at %s tokens", sup.Name, tokensText(q.context))}
}

// busyWith says what keeps the machine from a quiet moment for a relay: a
// gated merge running or settling, a grant not claimed yet or a claim
// queued; "" when it is quiet. alive says whether a gated merge's process
// runs, rolled whether a settling merge's release rolled on its lane's
// installation.
func busyWith(st *state.State, cfg *config.Config, held map[string]bool, now time.Time,
	alive func(pid int) bool, rolled func(state.Merge, config.Lane) bool,
) string {
	for _, m := range st.Merges {
		switch {
		case m.Phase == state.Running && alive(m.PID):
			return m.Key() + " merges"
		case m.Phase == state.Settling && settles(m, cfg, now, rolled):
			return m.Key() + " settles"
		}
	}
	seen := map[string]bool{}
	for _, g := range st.Grants {
		if seen[g.Resource] {
			continue
		}
		seen[g.Resource] = true
		q := lease.Pending(st, g.Resource, held[g.Resource], now, cfg.GrantTTL.Duration)
		switch {
		case len(q) == 0:
		case held[g.Resource]:
			return fmt.Sprintf("a claim of %s by %q is queued", g.Resource, q[0].To.Name)
		default:
			return fmt.Sprintf("%s is granted to %q and not claimed yet", g.Resource, q[0].To.Name)
		}
	}
	return ""
}

// settles reports whether a settling merge still holds its lane by the
// gate's rule: a lane without an installation waits for none; one whose
// merge settled longer than merge.settleTimeout ago is stuck, not settling;
// an unknown release settles for merge.settle, a known one until it rolled.
func settles(m state.Merge, cfg *config.Config, now time.Time, rolled func(state.Merge, config.Lane) bool) bool {
	lane, ok := cfg.LaneNamed(m.Lane)
	if !ok || lane.Installation == "" {
		return false
	}
	since := now.Sub(m.Finished)
	switch {
	case since > cfg.Merge.SettleTimeout.Duration:
		return false
	case m.Release == "":
		return since < cfg.Merge.Settle.Duration
	}
	return !rolled(m, lane)
}
