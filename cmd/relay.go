package cmd

import (
	"strconv"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A relayed role (the guide) moves in two steps: the relay opens to a
// fresh session beekeeper starts as the next run, whose own start takes the
// role. Both are one state update under the lock each. A session holds one
// role at a time. The supervisor is no such role: it is the supervisor
// loop, a service no session holds.

var (
	guideRole = role{
		Name: guideName, Title: "Guide", Inf: guideName, Verb: "guides", Ing: "guiding", Tag: "GUIDE ", Duty: "the guide's role", Handover: "beekeeper guide handover --prompt",
		Gone: "the standby watch starts its successor, or `beekeeper guide start --new` does now; the successor's `beekeeper guide start` takes the role",
		Get:  (*state.State).GuideRole, Set: (*state.State).SetGuideRole,
		Cfg: func(c *config.Config) config.Role { return c.Guide.Role },
	}
	roles = []role{guideRole}
)

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

// readHolder reads r's holder the same way.
func readHolder(r state.Role, sessions []*claude.Session, now time.Time, grace time.Duration) supervision {
	v := supervision{Sup: r.Holder}
	if v.Sup == nil {
		return v
	}
	if _, v.Live = claude.Live(sessions, v.Sup.Party); v.Live {
		return v
	}
	v.Gone = now
	if c := r.CLI; c.Of(v.Sup) && !c.Gone.IsZero() {
		v.Gone = c.Gone
	}
	if until := v.Gone.Add(grace); now.Before(until) {
		v.Until = until
	}
	return v
}

// renameHolder gives sup the title its running session carries now: a
// holder with a numbered run ignores desktop fallback names across imports
// and CLI restarts. A new run title can still replace an older one. It
// reports whether the name changed.
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
	for _, title := range []string{"Guide", "Supervisor"} {
		rl := role{Title: title}
		if rl.RunOf(sup.Name) > 0 && rl.RunOf(s.Name) == 0 {
			return false
		}
	}
	sup.Name = s.Name
	return true
}

// roleContext is the same for r's holder.
func roleContext(r state.Role, sessions []*claude.Session, now time.Time, relayAt config.Tokens, said relayDues) int64 {
	sup := r.Holder
	if sup == nil || r.Relay.Open(now) || said.Said(r, relayAt, now) {
		return 0
	}
	if c := sessionContext(sessions, sup.Party); c >= int64(relayAt) {
		return c
	}
	return 0
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

// idleRoleReading recommends a fresh run before a new feed line rewarms an
// hour-old context. An unknown activity time is no evidence of an idle hour.
func idleRoleReading(r state.Role, sessions []*claude.Session, now time.Time, relayAt config.Tokens, said relayDues) quietness {
	q := quietness{RelayAt: relayAt}
	if r.Holder == nil || r.Relay.Open(now) || said.Said(r, relayAt, now) {
		return q
	}
	s, live := claude.Live(sessions, r.Holder.Party)
	if !live {
		return q
	}
	last := s.LastActive
	if s.LastSubagent.After(last) {
		last = s.LastSubagent
	}
	if last.IsZero() || now.Sub(last) < time.Hour {
		return q
	}
	q.Context = transcriptContext(s)
	q.Checked = q.Context > int64(relayAt)/2
	q.Idle = q.Checked
	return q
}
