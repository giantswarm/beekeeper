package cmd

import (
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A relayed role (the supervisor, the guide) moves in two steps: the
// relay opens to a fresh session beekeeper starts as the next run, whose
// own start takes the role. Both are one state update under the lock each, so the
// supervisor's grant rule is in force throughout: the outgoing supervisor's
// until the start, the successor's from it. A session holds one role at a
// time.

// role is one relayed role: how it reads and says itself, and where its
// record and configuration live.
type role struct {
	name string // supervisor, guide
	// title names its runs: "<title> run <n>".
	title string
	inf   string // supervise, guide
	verb  string // supervises, guides
	ing   string // supervising, guiding
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
		name: "supervisor", title: "Supervisor", inf: "supervise", verb: "supervises", ing: "supervising", duty: "the supervisor's watch", handover: "beekeeper handover --prompt", grants: true,
		gone: "claims wait for a successor's `beekeeper supervisor start`",
		get:  (*state.State).SupervisorRole, set: (*state.State).SetSupervisorRole,
		cfg: func(c *config.Config) config.Role { return c.Supervisor.Role },
	}
	guideRole = role{
		name: "guide", title: "Guide", inf: "guide", verb: "guides", ing: "guiding", tag: "GUIDE ", duty: "the guide's role", handover: "beekeeper guide handover --prompt",
		gone: "a successor's `beekeeper guide start` takes the role (beekeeper guide handover --prompt)",
		get:  (*state.State).GuideRole, set: (*state.State).SetGuideRole,
		cfg: func(c *config.Config) config.Role { return c.Guide.Role },
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
		// The relieved run's desktop session would hold a desktop CLI slot.
		archive := ""
		if host := oweArchive(st, prev.Party, fmt.Sprintf("relieved by %q", me.Name), now); host != "" {
			archive = fmt.Sprintf("; the doctor archives its desktop session %s once its CLI runs no turn", host)
		}
		// The relieved run's work ends with the relay: off the roster, nothing
		// resumes it or shows it again (a background wait of its own dies with
		// it).
		st.Agents = slices.DeleteFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(prev.Party) })
		how = fmt.Sprintf(" (relieving %q, relayed at %s%s%s)", prev.Name, clock(now, r.Relay.At), grants, archive)
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
	// Holding the role again ends my relief; a relief nobody asked about
	// for reliefTTL is dropped.
	r.Relieved = slices.DeleteFunc(dropRelief(r.Relieved, me), func(rf state.Relief) bool { return now.Sub(rf.Taken) > reliefTTL })
	restart, last := prev != nil && prev.Is(me), rl.lastRun(r)
	r.Holder = &state.Supervisor{Party: me, Since: now.UTC()}
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
	r.Run = max(rl.lastRun(r), rl.runOf(to.Name))
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
		if old := sup.Name; renameHolder(sup, sessions) {
			c.Supervisor.Name, changed = sup.Name, true
			evs = append(evs, event(sup.Party, rl.name+".rename", "%q is now %q", old, sup.Name))
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

// renameHolder gives sup the title its running session carries now: a
// holder renamed after its start is known by its current name, not the one
// recorded at the start. It reports whether the name changed.
func renameHolder(sup *state.Supervisor, sessions []*claude.Session) bool {
	if sup == nil {
		return false
	}
	s, live := claude.Live(sessions, sup.Party)
	// A CLI the desktop runs before it recorded the title is known by its
	// PID only: no rename.
	if !live || s.Name == "" || s.Name == sup.Name || s.Name == "pid "+strconv.Itoa(s.PID) {
		return false
	}
	sup.Name = s.Name
	return true
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

// relayContext is the running supervisor's context in tokens once it has
// reached relayAt, with no relay open and the relay due not said yet to its
// term at relayAt (relayDues.said); 0 otherwise. Only then does the watch ask
// whether the machine is quiet.
func relayContext(st *state.State, sessions []*claude.Session, now time.Time, relayAt config.Tokens, said relayDues) int64 {
	return roleContext(st.SupervisorRole(), sessions, now, relayAt, said)
}

// roleContext is the same for r's holder.
func roleContext(r state.Role, sessions []*claude.Session, now time.Time, relayAt config.Tokens, said relayDues) int64 {
	sup := r.Holder
	if sup == nil || r.Relay.Open(now) || said.said(r, relayAt, now) {
		return 0
	}
	if c := sessionContext(sessions, sup.Party); c >= int64(relayAt) {
		return c
	}
	return 0
}

// relayDues is what one watch process said of the relay dues: when it said
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
// the poll at now. Within one poll it is not said yet, so the poll's write
// under the lock says what its read found.
func (d relayDues) said(r state.Role, relayAt config.Tokens, now time.Time) bool {
	if !r.RelayDue.At(r.Holder, int64(relayAt)) {
		return false
	}
	if d == nil {
		return true
	}
	at, ok := d[relayDueKey(r.Holder, relayAt)]
	return ok && at.Before(now)
}

// say records that this process said the relay due to sup's term at
// relayAt at now.
func (d relayDues) say(sup *state.Supervisor, relayAt config.Tokens, now time.Time) {
	if d == nil {
		return
	}
	k := relayDueKey(sup, relayAt)
	if _, ok := d[k]; !ok {
		d[k] = now
	}
}

// sessionContext is the context in tokens of p's running session, read from
// its transcript's last request (the CTX column); 0 when it does not run.
func sessionContext(sessions []*claude.Session, p state.Party) int64 {
	s, live := claude.Live(sessions, p)
	if !live {
		return 0
	}
	return transcriptContext(s)
}

// transcriptContext is the context in tokens of s, read from its
// transcript's last request; 0 without a transcript.
func transcriptContext(s *claude.Session) int64 { return claude.Context(s.Transcript) }

// quietness is the watch's reading of the machine for a relay: checked once
// the holder's context reached relayAt (context), busy saying what keeps it
// from a quiet moment ("" when quiet).
type quietness struct {
	checked bool
	busy    string
	context int64
	relayAt config.Tokens
}

// fireRelayDue reports the supervisor's relay due (role.fireRelayDue).
func fireRelayDue(st *state.State, q quietness, said relayDues, now time.Time) ([]string, []state.Event) {
	return supervisorRole.fireRelayDue(st, q, said, now)
}

// fireRelayDue reports rl's relay due at the first quiet moment after its
// holder's context reached q.relayAt: once per term and relayAt in the
// state, and once more in a watch process that did not say it yet. The
// record keeps when it was first reported at that relayAt.
func (rl role) fireRelayDue(st *state.State, q quietness, said relayDues, now time.Time) (lines []string, evs []state.Event) {
	rl.update(st, func(r *state.Role) {
		sup := r.Holder
		if !q.checked || q.busy != "" || sup == nil || r.Relay.Open(now) || said.said(*r, q.relayAt, now) {
			return
		}
		if !r.RelayDue.At(sup, int64(q.relayAt)) {
			r.RelayDue = &state.RelayDue{Supervisor: sup.Party, Since: sup.Since, Reported: now.UTC(), Context: q.context, RelayAt: int64(q.relayAt)}
		}
		said.say(sup, q.relayAt, now)
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
