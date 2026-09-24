package cmd

import (
	"fmt"
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
	st.Relay = nil
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

// relievedBy returns the taken relay that relieved me, nil when none did.
func relievedBy(st *state.State, me state.Party) *state.Relay {
	r := st.Relay
	if r == nil || r.Taken.IsZero() || !r.From.Is(me) || st.Supervisor == nil || !st.Supervisor.Is(r.To) {
		return nil
	}
	return r
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
	default:
		return nil, nil
	}
	r.Reported = now.UTC()
	return []string{line}, evs
}

// shiftOver reports whether st's supervisor runs, has served its shift and
// has no relay open: the moment the watch asks whether the machine is quiet.
func shiftOver(st *state.State, sessions []*claude.Session, now time.Time, shift time.Duration) bool {
	sup := st.Supervisor
	if shift <= 0 || sup == nil || st.Relay.Open(now) || now.Sub(sup.Since) < shift {
		return false
	}
	_, live := claude.Live(sessions, sup.Party)
	return live
}

// quietness is the watch's reading of the machine for a relay: checked once
// the shift is over, busy saying what keeps it from a quiet moment ("" when
// quiet).
type quietness struct {
	checked bool
	busy    string
}

// fireShift reports the relay due at the first quiet moment after the shift
// and again at the first quiet moment after a busy one. changed says that
// st changed without a line (a busy moment after the report).
func fireShift(st *state.State, q quietness, now time.Time, shift time.Duration) (lines []string, evs []state.Event, changed bool) {
	sup := st.Supervisor
	if !q.checked || sup == nil || st.Relay.Open(now) {
		return nil, nil, false
	}
	mine := st.Shift.Of(sup)
	switch {
	case q.busy != "":
		if mine && st.Shift.Quiet {
			st.Shift.Quiet = false
			return nil, nil, true
		}
		return nil, nil, false
	case mine && st.Shift.Quiet:
		return nil, nil, false
	}
	st.Shift = &state.Shift{Supervisor: sup.Party, Since: sup.Since, Reported: now.UTC(), Quiet: true}
	line := fmt.Sprintf("RELAY DUE: %q supervises since %s (shift %s) and the machine is quiet: start a successor from `beekeeper handover --prompt`, then `beekeeper supervisor relay <successor>`",
		sup.Name, clock(now, sup.Since), dur(shift))
	return []string{line}, []state.Event{event(watchParty, "supervisor.relay-due", "%s since %s", sup.Name, clock(now, sup.Since))}, true
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
