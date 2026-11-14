package cmd

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// role is one relayed role: how it reads and says itself, and where its
// record and configuration live.
type role struct {
	name string // guide
	// title names its runs: "<title> run <n>".
	title string
	inf   string // guide
	verb  string // guides
	ing   string // guiding
	// tag opens the role's watch lines.
	tag string
	// duty is what a successor takes over.
	duty string
	// handover is the command that prints the successor's prompt.
	handover string
	// gone says what follows once the holder's CLI stayed gone.
	gone string
	get  func(*state.State) state.Role
	set  func(*state.State, state.Role)
	cfg  func(*config.Config) config.Role
}

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
	case prev == nil:
		if r.Relay != nil && r.Relay.From == (state.Party{}) && takesRelay(r.Relay.To, me) {
			me.HostSession = cmp.Or(me.HostSession, r.Relay.To.HostSession)
			if rl.runOf(me.Name) == 0 && rl.runOf(r.Relay.To.Name) > 0 {
				me.Name = r.Relay.To.Name
			}
			r.Relay = nil
		}
	case prev.Is(me):
		// A restart of the same holder keeps an open relay, its desktop id
		// and its numbered run title when the resumed CLI derived another.
		me.HostSession = cmp.Or(me.HostSession, prev.HostSession)
		if rl.runOf(me.Name) == 0 && rl.runOf(prev.Name) > 0 {
			me.Name = prev.Name
		}
	case r.Relay.Open(now) && r.Relay.From.Is(prev.Party) && takesRelay(r.Relay.To, me):
		// The start runs in my headless first turn, which knows no desktop
		// id: the relay's keeps the role addressable until my desktop CLI
		// runs.
		me.HostSession = cmp.Or(me.HostSession, r.Relay.To.HostSession)
		if rl.runOf(me.Name) == 0 && rl.runOf(r.Relay.To.Name) > 0 {
			me.Name = r.Relay.To.Name
		}
		r.Relay.Taken = now.UTC()
		r.Relieved = append(dropRelief(r.Relieved, prev.Party),
			state.Relief{Party: prev.Party, By: me, At: r.Relay.At, Taken: r.Relay.Taken})
		// The relieved run's work ends with the relay: off the roster, nothing
		// resumes it or shows it again (a background wait of its own dies with
		// it). Its session stays the person's: they decide when it goes.
		st.Agents = slices.DeleteFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(prev.Party) })
		how = fmt.Sprintf(" (relieving %q, relayed at %s; %s)", prev.Name, clock(now, r.Relay.At), keptSession(st, prev.Party))
	case prevLive && !takeOver:
		named := ""
		switch rel := r.Relay; {
		case rel.Open(now) && rel.From.Is(prev.Party):
			named = fmt.Sprintf("; its relay names %q, not you", rel.To.Name)
		case rel != nil && rel.Taken.IsZero() && rel.From.Is(prev.Party) && rel.To.Is(me):
			named = fmt.Sprintf("; its relay to you expired at %s", clock(now, rel.Expires))
		}
		return "", nil, refused("%q %s since %s and still runs%s: its `beekeeper %s relay` starts the successor, or take over with --take-over",
			prev.Name, rl.verb, clock(now, prev.Since), named, rl.name)
	default:
		r.Relay = nil
		how = fmt.Sprintf(" (taking over from %q)", prev.Name)
	}
	if s, ok := st.BypassStartOf(me); ok && s.Role == rl.name {
		me.HostSession = cmp.Or(me.HostSession, s.HostSession)
		if rl.runOf(me.Name) == 0 && rl.runOf(s.Name) > 0 {
			me.Name = s.Name
		}
	}
	// Holding the role again ends my relief; a relief nobody asked about
	// for reliefTTL is dropped.
	r.Relieved = slices.DeleteFunc(dropRelief(r.Relieved, me), func(rf state.Relief) bool { return now.Sub(rf.Taken) > reliefTTL })
	restart, last := prev != nil && prev.Is(me), rl.lastRun(r)
	r.Holder = &state.Supervisor{Party: me, Since: now.UTC()}
	if r.CLI != nil && r.CLI.Supervisor.Is(me) {
		r.CLI.Supervisor.Session = cmp.Or(me.Session, r.CLI.Supervisor.Session)
		r.CLI.Supervisor.HostSession = cmp.Or(me.HostSession, r.CLI.Supervisor.HostSession)
		if rl.runOf(me.Name) > 0 {
			r.CLI.Supervisor.Name = me.Name
		}
	}
	// The start that took the role keeps its session the person's to close
	// once relieved or taken over (keepsRole).
	if i := st.StartIndex(me); i >= 0 {
		st.Starts[i].Role = rl.name
	}
	// A run relayed to me, or named in my title, is mine; any other start
	// of the role is the next run.
	switch n := rl.runOf(me.Name); {
	case n > 0:
		r.Run = max(r.Run, n)
	case !restart || r.Run == 0:
		r.Run = last + 1
	}
	rl.set(st, r)
	as := ""
	if run := rl.runName(r.Run); run != me.Name {
		as = " as " + run
	}
	msg := fmt.Sprintf("%q %s now%s%s", me.Name, rl.verb, as, how)
	return msg, []state.Event{event(me, rl.name+".start", "%s", msg)}, nil
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
	r.Run = max(rl.lastRun(r), rl.runOf(to.Name))
	rl.set(st, r)
	until := clock(now, r.Relay.Expires)
	msg := fmt.Sprintf("relayed to %q until %s%s: its `beekeeper %s start` takes the role; you %s until then (`beekeeper %s status` exits %d once you are relieved)",
		to.Name, until, replacing, rl.name, rl.inf, rl.name, ExitRelieved)
	return msg, []state.Event{event(me, rl.name+".relay", "to %s until %s%s", to.Name, until, replacing)}, nil
}

// keptSession says where the relieved run p's conversation stays and how
// the person closes it: neither the watch nor the doctor stops or archives
// it.
func keptSession(st *state.State, p state.Party) string {
	if i := st.StartIndex(p); i >= 0 && !st.Starts[i].Background && st.Starts[i].HostSession != "" {
		return fmt.Sprintf("its desktop session %s stays in the person's session list until they archive it", st.Starts[i].HostSession)
	}
	if p.Session == "" {
		return "its session stays until the person closes it"
	}
	short := p.Session[:min(8, len(p.Session))]
	return fmt.Sprintf("its session %s stays until the person closes it (claude attach %s continues it, claude stop %s ends it)", p.Session, short, short)
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

// supervision is a role's recorded holder read against the running
// sessions and the grace a CLI restart has.
type supervision struct {
	sup  *state.Supervisor
	live bool
	// gone is when beekeeper first saw the CLI gone (now when no record
	// says so yet), zero while it runs; until is the end of the grace a
	// restart has, zero while it runs and once the grace has passed.
	gone, until time.Time
}

// restarting reports whether the holder's CLI is gone within the grace.
func (v supervision) restarting() bool { return !v.until.IsZero() }

// down reports whether the recorded holder's CLI stayed gone past the
// grace: its successor is due.
func (v supervision) down() bool { return v.sup != nil && !v.live && !v.restarting() }

// observeCLI records what the running sessions say of rl's holder's CLI in
// this term: the PID it runs as, and since when it is gone. The same
// session back with a new PID is a restart, logged once. It reports whether
// st changed.
func (rl role) observeCLI(st *state.State, sessions []*claude.Session, now time.Time) (changed bool, evs []state.Event) {
	var oldID, newID, hostID string
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
		if old := sup.Name; renameHolder(sup, sessions) {
			c.Supervisor.Name, changed = sup.Name, true
			evs = append(evs, event(sup.Party, rl.name+".rename", "%q is now %q", old, sup.Name))
		}
		s, live := claude.Live(sessions, sup.Party)
		if live && s.ID != "" && s.ID != sup.Session && (s.ResumedID == sup.Session || (s.JobID != "" && len(sup.Session) >= 8 && strings.HasPrefix(sup.Session, s.JobID))) {
			oldID, newID, hostID = sup.Session, s.ID, s.HostID
		}
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
	if oldID != "" && rebindSessionState(st, oldID, newID, hostID) {
		changed = true
	}
	return changed, evs
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
			lines = append(lines, fmt.Sprintf("%sRELAY TAKEN: %q %s since %s, relieving %q; %s", rl.tag, r.To.Name, rl.verb, clock(now, r.Taken), r.From.Name, keptSession(st, r.From)))
		case !now.Before(r.Expires):
			lines = append(lines, fmt.Sprintf("%sRELAY EXPIRED: %q did not take the role from %q by %s (beekeeper %s relay starts another)",
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

const relayDueRepeat = 5 * time.Minute

// relayDues is what one watch process said of the relay dues: when it last said
// each, by relayDueKey. A process that did not say a standing relay due
// says it once more; nil keeps no memory, and the state's record alone
// decides (a watch --once).
type relayDues map[string]time.Time

// relayDueKey names the relay due to sup's term at relayAt.
func relayDueKey(sup *state.Supervisor, relayAt config.Tokens) string {
	who := sup.Session
	if who == "" {
		who = sup.Name
	}
	return fmt.Sprintf("%s@%s@%d", who, sup.Since.UTC().Format(time.RFC3339Nano), relayAt)
}

// said reports whether the relay due to r's holder's term at relayAt is
// said: the state records it at relayAt, and this process said it before
// the poll at now, less than relayDueRepeat ago. Within one poll it is
// not said yet, so the poll's write
// under the lock says what its read found.
func (d relayDues) said(r state.Role, relayAt config.Tokens, now time.Time) bool {
	if !r.RelayDue.At(r.Holder, int64(relayAt)) {
		return false
	}
	if d == nil {
		return true
	}
	at, ok := d[relayDueKey(r.Holder, relayAt)]
	return ok && at.Before(now) && now.Sub(at) < relayDueRepeat
}

// say records that this process said the relay due to sup's term at
// relayAt at now.
func (d relayDues) say(sup *state.Supervisor, relayAt config.Tokens, now time.Time) {
	if d == nil {
		return
	}
	k := relayDueKey(sup, relayAt)
	d[k] = now
}

// quietness is the watch's reading of a holder's context for a relay:
// checked once it reached relayAt, or half of it after an idle hour.
type quietness struct {
	checked bool
	context int64
	idle    bool
	relayAt config.Tokens
}

// fireRelayDue reports rl's relay due once its holder's context reached
// q.relayAt, or a new line arrives after an idle hour above half of it:
// once per term and relayAt in the state, and every relayDueRepeat in a
// continuous watch until a relay opens. The
// record keeps when it was first reported at that relayAt.
func (rl role) fireRelayDue(st *state.State, q quietness, said relayDues, now time.Time) (lines []string, evs []state.Event) {
	rl.update(st, func(r *state.Role) {
		sup := r.Holder
		if !q.checked || sup == nil || r.Relay.Open(now) || said.said(*r, q.relayAt, now) {
			return
		}
		if !r.RelayDue.At(sup, int64(q.relayAt)) {
			r.RelayDue = &state.RelayDue{Supervisor: sup.Party, Since: sup.Since, Reported: now.UTC(), Context: q.context, RelayAt: int64(q.relayAt)}
		}
		said.say(sup, q.relayAt, now)
		reason := ""
		if q.idle {
			reason = " after an idle hour (cache expired)"
		}
		lines = append(lines, fmt.Sprintf("%sRELAY DUE: %q is at %s tokens of context%s: %s", rl.tag, sup.Name, tokensText(q.context), reason, rl.handover))
		evs = append(evs, event(watchParty, rl.name+".relay-due", "%s at %s tokens", sup.Name, tokensText(q.context)))
	})
	return lines, evs
}
