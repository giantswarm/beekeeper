package cmd

import (
	"cmp"
	"os"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

const goneActivityPrefix = "gone, last activity "

// liveSession reports whether p has a running CLI session in sessions. A
// persistent harness wrapper (omp, codex, agy) waiting idle on its inbox
// after its turn ended is not a running CLI.
func liveSession(sessions []*claude.Session, p state.Party) (*claude.Session, bool) {
	s, live := claude.Live(sessions, p)
	if !live || s.State == omp.StateIdle && len(s.Commands) == 0 {
		return nil, false
	}
	return s, true
}

func rosterActivity(st *state.State, ag state.Agent) time.Time {
	last := ag.Registered
	for _, at := range []time.Time{ag.AssignedAt, ag.IdleSince, ag.ActivityAt, ag.ResumedWait, ag.DesktopTurn, ag.GHAt} {
		if at.After(last) {
			last = at
		}
	}
	for i := range st.Records {
		r := &st.Records[i]
		if r.Session.Is(ag.Party) && r.At.After(last) {
			last = r.At
		}
	}
	if ag.Import != nil && ag.Import.Since.After(last) {
		last = ag.Import.Since
	}
	if ag.Keep != nil && ag.Keep.At.After(last) {
		last = ag.Keep.At
	}
	return last
}

// agentActivity follows both the roster and the transcript, including between
// turns when discovery has no running CLI. Unknown timestamps keep the slot.
func agentActivity(cfg *config.Config, st *state.State, ag state.Agent) time.Time {
	last := rosterActivity(st, ag)
	if cfg == nil {
		return last
	}
	if h, id, ok := harnessForHost(ag.HostSession); ok {
		if h.Name() == agy.Harness && cfg.StateDir != "" {
			if fi, err := os.Stat(agy.StreamPath(cfg.StateDir, id)); err == nil && fi.ModTime().After(last) {
				last = fi.ModTime()
			}
		}
		for _, s := range h.Ended(cfg, nil, last) {
			if ag.Is(s.Party()) && s.LastActive.After(last) {
				last = s.LastActive
			}
		}
		return last
	}
	id := ag.Session
	if r, ok := claude.ReadRecord(cfg, ag.HostSession); ok && r.CLISessionID != "" {
		id = r.CLISessionID
	}
	if path := transcriptOf(cfg, id); path != "" {
		if fi, err := os.Stat(path); err == nil && fi.ModTime().After(last) {
			last = fi.ModTime()
		}
	}
	return last
}

func agyAgentStartID(ag state.Agent) string {
	if id, ok := strings.CutPrefix(strings.TrimSpace(ag.HostSession), agy.HostPrefix); ok && id != "" {
		return id
	}
	if id, ok := strings.CutPrefix(strings.TrimSpace(ag.Session), agy.HostPrefix); ok && id != "" {
		return id
	}
	return ""
}

func agyQuotaError(cfg *config.Config, ag state.Agent) *agy.QuotaError {
	if cfg == nil || cfg.StateDir == "" {
		return nil
	}
	id := agyAgentStartID(ag)
	if id == "" {
		return nil
	}
	return agy.ReadQuotaError(cfg.StateDir, id)
}

// goneAgent is a local task holder with neither a running CLI nor recent
// transcript or roster activity, or an agy worker whose run ended with a
// quota error. Kept, parked, remote and role entries stay.
func goneAgent(cfg *config.Config, st *state.State, sessions []*claude.Session, ag state.Agent, staleAfter time.Duration, now time.Time) (time.Time, bool) {
	guide := st.GuideRole()
	if guide.Relay.Open(now) && guide.Relay.To.Is(ag.Party) {
		return time.Time{}, false
	}
	if ag.Execution != nil || ag.Task == "" || ag.Done || roleRun(st, ag.Party) || keptBy(st, ag, now) != "" || ag.Import.Pending(now) {
		return time.Time{}, false
	}
	if qerr := agyQuotaError(cfg, ag); qerr != nil {
		return cmp.Or(qerr.At, agentActivity(cfg, st, ag), now), true
	}
	if !ag.ContinuationExhausted.IsZero() && !ag.ContinuationExhausted.Before(ag.AssignedAt) {
		if _, live := liveSession(sessions, ag.Party); !live && !mergeInFlight(st, ag.Party, proc.Alive) {
			return ag.ContinuationExhausted, true
		}
	}
	if staleAfter <= 0 {
		return time.Time{}, false
	}
	if strings.HasPrefix(ag.Name, "Review ") && staleAfter > stewardQuiet {
		staleAfter = stewardQuiet
	}
	if _, live := liveSession(sessions, ag.Party); live || mergeInFlight(st, ag.Party, proc.Alive) {
		return time.Time{}, false
	}
	last := rosterActivity(st, ag)
	if s, ok := claude.Live(sessions, ag.Party); ok {
		for _, at := range []time.Time{s.Started, s.LastActive} {
			if at.After(last) {
				last = at
			}
		}
	}
	if !last.IsZero() && now.Sub(last) < staleAfter {
		return last, false
	}
	if act := agentActivity(cfg, st, ag); act.After(last) {
		last = act
	}
	return last, !last.IsZero() && now.Sub(last) >= staleAfter
}

func rosterConfig(cfg *config.Config) (*config.Config, time.Duration) {
	if cfg != nil {
		return cfg, cfg.Agents.StaleAfter.Duration
	}
	return nil, 24 * time.Hour
}
