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

// A relayed role (the supervisor, the guide) moves in two steps: the
// holder names its successor (relay), the successor takes the role with its
// own start. Both are one state update under the lock each, so the
// supervisor's grant rule is in force throughout: the outgoing supervisor's
// until the start, the successor's from it. A session holds one role at a
// time.

// role is one relayed role: how it reads and says itself, and where its
// record and configuration live.
type role struct {
	name string // supervisor, guide
	inf  string // supervise, guide
	verb string // supervises, guides
	ing  string // supervising, guiding
	// tag opens the role's watch lines ("" for the supervisor's).
	tag string
	// duty is what a successor takes over.
	duty string
	// handover is the command that prints the successor's prompt.
	handover string
	// gone says what follows once the holder's CLI stayed gone.
	gone string
	// grants: the role's start takes the grant queue.
	grants bool
	get    func(*state.State) state.Role
	set    func(*state.State, state.Role)
	cfg    func(*config.Config) config.Role
}

var (
	supervisorRole = role{
		name: "supervisor", inf: "supervise", verb: "supervises", ing: "supervising", duty: "the supervisor's watch", handover: "beekeeper handover --prompt", grants: true,
		gone: "claims wait for a successor's `beekeeper supervisor start`",
		get:  (*state.State).SupervisorRole, set: (*state.State).SetSupervisorRole,
		cfg: func(c *config.Config) config.Role { return c.Supervisor.Role },
	}
	guideRole = role{
		name: "guide", inf: "guide", verb: "guides", ing: "guiding", tag: "GUIDE ", duty: "the guide's role", handover: "beekeeper guide handover --prompt",
		gone: "a successor's `beekeeper guide start` takes the role (beekeeper guide handover --prompt)",
		get:  (*state.State).GuideRole, set: (*state.State).SetGuideRole,
		cfg: func(c *config.Config) config.Role { return c.Guide },
	}
	roles = []role{supervisorRole, guideRole}
)

// update applies f to rl's record in st.
func (rl role) update(st *state.State, f func(r *state.Role)) {
	r := rl.get(st)
	f(&r)
	rl.set(st, r)
}

// otherRole returns the role other than rl that p holds, if any.
func (rl role) otherRole(st *state.State, p state.Party) (role, bool) {
	for _, o := range roles {
		if h := o.get(st).Holder; o.name != rl.name && h != nil && h.Is(p) {
			return o, true
		}
	}
	return role{}, false
}

// startRole makes me the supervisor in st: through a relay that names me,
// over a recorded supervisor whose session is gone, or with takeOver.
// prevLive says whether the recorded supervisor's session runs.
func startRole(st *state.State, me state.Party, prevLive, takeOver bool, now time.Time) (string, []state.Event, error) {
	return supervisorRole.start(st, me, prevLive, takeOver, now)
}

// start makes me rl's holder in st: through a relay that names me, over a
// recorded holder whose session is gone, or with takeOver. prevLive says
// whether the recorded holder's session runs.
func (rl role) start(st *state.State, me state.Party, prevLive, takeOver bool, now time.Time) (string, []state.Event, error) {
	if o, ok := rl.otherRole(st, me); ok {
		return "", nil, refused("you are the %s: a session holds one role; `beekeeper %s stop` or relay it first", o.name, o.name)
	}
	r := rl.get(st)
	prev := r.Holder
	how := ""
	switch {
	case prev == nil || prev.Is(me):
		// A restart of the same holder keeps an open relay.
	case r.Relay.Open(now) && r.Relay.From.Is(prev.Party) && r.Relay.To.Is(me):
		r.Relay.Taken = now.UTC()
		r.Relieved = append(dropRelief(r.Relieved, prev.Party),
			state.Relief{Party: prev.Party, By: me, At: r.Relay.At, Taken: r.Relay.Taken})
		grants := ""
		if rl.grants {
			for i := range st.Grants {
				st.Grants[i].By = me
			}
			grants = fmt.Sprintf("; %d grant(s) move over", len(st.Grants))
		}
		how = fmt.Sprintf(" (relieving %q, relayed at %s%s)", prev.Name, clock(now, r.Relay.At), grants)
	case prevLive && !takeOver:
		named := ""
		switch rel := r.Relay; {
		case rel.Open(now) && rel.From.Is(prev.Party):
			named = fmt.Sprintf("; its relay names %q, not you", rel.To.Name)
		case rel != nil && rel.Taken.IsZero() && rel.From.Is(prev.Party) && rel.To.Is(me):
			named = fmt.Sprintf("; its relay to you expired at %s", clock(now, rel.Expires))
		}
		return "", nil, refused("%q %s since %s and still runs%s: it relays the role with `beekeeper %s relay <you>`, or take over with --take-over",
			prev.Name, rl.verb, clock(now, prev.Since), named, rl.name)
	default:
		r.Relay = nil
		how = fmt.Sprintf(" (taking over from %q)", prev.Name)
	}
	// Holding the role again ends my relief; a relief nobody asked about
	// for reliefTTL is dropped.
	r.Relieved = slices.DeleteFunc(dropRelief(r.Relieved, me), func(rf state.Relief) bool { return now.Sub(rf.Taken) > reliefTTL })
	r.Holder = &state.Supervisor{Party: me, Since: now.UTC()}
	rl.set(st, r)
	msg := fmt.Sprintf("%q %s now%s", me.Name, rl.verb, how)
	return msg, []state.Event{event(me, rl.name+".start", "%s", msg)}, nil
}

// relayRole records the supervisor me naming its successor to; the relay
// stays open for ttl.
func relayRole(st *state.State, me, to state.Party, now time.Time, ttl time.Duration) (string, []state.Event, error) {
	return supervisorRole.relay(st, me, to, now, ttl)
}

// relay records rl's holder me naming its successor to; the relay stays
// open for ttl.
func (rl role) relay(st *state.State, me, to state.Party, now time.Time, ttl time.Duration) (string, []state.Event, error) {
	r := rl.get(st)
	if err := rl.mustHold(r, me); err != nil {
		return "", nil, err
	}
	if to.Is(me) {
		return "", nil, usageErr("you %s already: name the session that takes over", rl.inf)
	}
	if o, ok := rl.otherRole(st, to); ok {
		return "", nil, refused("%q is the %s: a session holds one role", to.Name, o.name)
	}
	replacing := ""
	if r.Relay.Open(now) && !r.Relay.To.Is(to) {
		replacing = fmt.Sprintf(", replacing the relay to %q", r.Relay.To.Name)
	}
	r.Relay = &state.Relay{From: me, To: to, At: now.UTC(), Expires: now.Add(ttl).UTC()}
	rl.set(st, r)
	until := clock(now, r.Relay.Expires)
	grants := ""
	if rl.grants {
		grants = " and the grants"
	}
	msg := fmt.Sprintf("relayed to %q until %s%s: its `beekeeper %s start` takes the role%s; you %s until then (`beekeeper %s status` exits %d once you are relieved)",
		to.Name, until, replacing, rl.name, grants, rl.inf, rl.name, ExitRelieved)
	return msg, []state.Event{event(me, rl.name+".relay", "to %s until %s%s", to.Name, until, replacing)}, nil
}

// cancelRelay withdraws the supervisor's open relay.
func cancelRelay(st *state.State, me state.Party, now time.Time) (string, []state.Event, error) {
	return supervisorRole.cancelRelay(st, me, now)
}

// cancelRelay withdraws rl's open relay.
func (rl role) cancelRelay(st *state.State, me state.Party, now time.Time) (string, []state.Event, error) {
	r := rl.get(st)
	if err := rl.mustHold(r, me); err != nil {
		return "", nil, err
	}
	if !r.Relay.Open(now) {
		return "no relay is open: you " + rl.inf, nil, nil
	}
	to := r.Relay.To.Name
	r.Relay, r.RelayDue = nil, nil
	rl.set(st, r)
	return fmt.Sprintf("relay to %q cancelled: you still %s", to, rl.inf),
		[]state.Event{event(me, rl.name+".relay-cancel", "to %s", to)}, nil
}

func (rl role) mustHold(r state.Role, me state.Party) error {
	switch {
	case r.Holder == nil:
		return refused("no %s is recorded: `beekeeper %s start` makes you one", rl.name, rl.name)
	case !r.Holder.Is(me):
		return refused("%q %s, not you: only the %s relays its role", r.Holder.Name, rl.verb, rl.name)
	}
	return nil
}

// reliefTTL is how long a relieved holder's status keeps exiting 4.
const reliefTTL = 7 * 24 * time.Hour

func dropRelief(rs []state.Relief, p state.Party) []state.Relief {
	return slices.DeleteFunc(rs, func(r state.Relief) bool { return r.Party.Is(p) })
}

// relievedBy returns the relief of a supervisor relay that relieved me.
func relievedBy(st *state.State, me state.Party) *state.Relief {
	return relievedIn(st.SupervisorRole(), me)
}

// relievedIn returns the relief of a relay of r that relieved me, nil when
// none did or I hold the role again. It survives the successor's own relays.
func relievedIn(r state.Role, me state.Party) *state.Relief {
	if r.Holder != nil && r.Holder.Is(me) {
		return nil
	}
	for i := range r.Relieved {
		if r.Relieved[i].Party.Is(me) {
			return &r.Relieved[i]
		}
	}
	return nil
}

// supervision is a role's recorded holder read against the running
// sessions and the grace a CLI restart has. The supervisor's grant rule is
// in force in every state: live, restarting and gone.
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
	return readHolder(st.SupervisorRole(), sessions, now, grace)
}

// readHolder reads r's holder the same way.
func readHolder(r state.Role, sessions []*claude.Session, now time.Time, grace time.Duration) supervision {
	v := supervision{sup: r.Holder}
	if v.sup == nil {
		return v
	}
	if _, v.live = claude.Live(sessions, v.sup.Party); v.live {
		return v
	}
	v.gone = now
	if c := r.CLI; c.Of(v.sup) && !c.Gone.IsZero() {
		v.gone = c.Gone
	}
	if until := v.gone.Add(grace); now.Before(until) {
		v.until = until
	}
	return v
}

// restarting reports whether the holder's CLI is gone within the grace.
func (v supervision) restarting() bool { return !v.until.IsZero() }

// down reports whether the recorded holder's CLI stayed gone past the
// grace: its successor is due.
func (v supervision) down() bool { return v.sup != nil && !v.live && !v.restarting() }

// observeCLI records what the running sessions say of the supervisor's
// CLI in this term (observeRoleCLI).
func observeCLI(st *state.State, sessions []*claude.Session, now time.Time) (bool, []state.Event) {
	return supervisorRole.observeCLI(st, sessions, now)
}

// observeCLI records what the running sessions say of rl's holder's CLI in
// this term: the PID it runs as, and since when it is gone. The same
// session back with a new PID is a restart, logged once. It reports whether
// st changed.
func (rl role) observeCLI(st *state.State, sessions []*claude.Session, now time.Time) (changed bool, evs []state.Event) {
	rl.update(st, func(r *state.Role) {
		sup := r.Holder
		if sup == nil {
			changed = r.CLI != nil
			r.CLI = nil
			return
		}
		c := r.CLI
		if !c.Of(sup) {
			c = &state.CLI{Supervisor: sup.Party, Since: sup.Since}
			r.CLI, changed = c, true
		}
		s, live := claude.Live(sessions, sup.Party)
		switch {
		case live && s.PID != c.PID:
			if c.PID != 0 {
				away := "unseen"
				if !c.Gone.IsZero() {
					away = "gone for " + dur(now.Sub(c.Gone))
				}
				evs = append(evs, event(sup.Party, rl.name+".restart", "CLI %d back as %d (%s)", c.PID, s.PID, away))
			}
			c.PID, c.Gone = s.PID, time.Time{}
			changed = true
		case live && !c.Gone.IsZero():
			c.Gone, changed = time.Time{}, true
		case !live && c.Gone.IsZero():
			c.Gone, changed = now.UTC(), true
		}
	})
	return changed, evs
}

// fireRelay reports the supervisor's relay taken or expired, once.
func fireRelay(st *state.State, now time.Time) ([]string, []state.Event) {
	return supervisorRole.fireRelay(st, now)
}

// fireRelay reports rl's relay taken or expired, once.
func (rl role) fireRelay(st *state.State, now time.Time) (lines []string, evs []state.Event) {
	rl.update(st, func(ro *state.Role) {
		r := ro.Relay
		if r == nil || !r.Reported.IsZero() {
			return
		}
		switch {
		case !r.Taken.IsZero():
			lines = append(lines, fmt.Sprintf("%sRELAY TAKEN: %q %s since %s, relieving %q", rl.tag, r.To.Name, rl.verb, clock(now, r.Taken), r.From.Name))
		case !now.Before(r.Expires):
			lines = append(lines, fmt.Sprintf("%sRELAY EXPIRED: %q did not take the role from %q by %s (beekeeper %s relay <successor> opens another)",
				rl.tag, r.To.Name, r.From.Name, clock(now, r.Expires), rl.name))
			evs = append(evs, event(watchParty, rl.name+".relay-expired", "to %s at %s", r.To.Name, clock(now, r.Expires)))
			ro.RelayDue = nil
		default:
			return
		}
		r.Reported = now.UTC()
	})
	return lines, evs
}

// relayContext is the running supervisor's context in tokens once it has
// reached relayAt, with no relay open and the relay due not reported yet to
// its term; 0 otherwise. Only then does the watch ask whether the machine is
// quiet.
func relayContext(st *state.State, sessions []*claude.Session, now time.Time, relayAt config.Tokens) int64 {
	return roleContext(st.SupervisorRole(), sessions, now, relayAt)
}

// roleContext is the same for r's holder.
func roleContext(r state.Role, sessions []*claude.Session, now time.Time, relayAt config.Tokens) int64 {
	sup := r.Holder
	if sup == nil || r.Relay.Open(now) || r.RelayDue.Of(sup) {
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
// the holder's context reached relayAt (context), busy saying what keeps it
// from a quiet moment ("" when quiet).
type quietness struct {
	checked bool
	busy    string
	context int64
}

// fireRelayDue reports the supervisor's relay due (role.fireRelayDue).
func fireRelayDue(st *state.State, q quietness, now time.Time) ([]string, []state.Event) {
	return supervisorRole.fireRelayDue(st, q, now)
}

// fireRelayDue reports rl's relay due at the first quiet moment after its
// holder's context reached relayAt, once per term.
func (rl role) fireRelayDue(st *state.State, q quietness, now time.Time) (lines []string, evs []state.Event) {
	rl.update(st, func(r *state.Role) {
		sup := r.Holder
		if !q.checked || q.busy != "" || sup == nil || r.Relay.Open(now) || r.RelayDue.Of(sup) {
			return
		}
		r.RelayDue = &state.RelayDue{Supervisor: sup.Party, Since: sup.Since, Reported: now.UTC(), Context: q.context}
		lines = append(lines, fmt.Sprintf("%sRELAY DUE: %q is at %s tokens of context: %s", rl.tag, sup.Name, tokensText(q.context), rl.handover))
		evs = append(evs, event(watchParty, rl.name+".relay-due", "%s at %s tokens", sup.Name, tokensText(q.context)))
	})
	return lines, evs
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
