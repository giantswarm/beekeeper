package cmd

import (
	"os"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

const goneActivityPrefix = "gone, last activity "

// agentActivity follows both the roster and the transcript, including between
// turns when discovery has no running CLI. Unknown timestamps keep the slot.
func agentActivity(cfg *config.Config, st *state.State, ag state.Agent) time.Time {
	last := ag.Registered
	for _, at := range []time.Time{ag.AssignedAt, ag.IdleSince, ag.ActivityAt, ag.ResumedWait, ag.DesktopTurn, ag.GHAt} {
		if at.After(last) {
			last = at
		}
	}
	for _, r := range st.Records {
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
	if cfg == nil {
		return last
	}
	if h, _, ok := harnessForHost(ag.HostSession); ok {
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

// goneAgent is a local task holder with neither a running CLI nor recent
// transcript or roster activity. Kept, parked, remote and role entries stay.
func goneAgent(cfg *config.Config, st *state.State, sessions []*claude.Session, ag state.Agent, staleAfter time.Duration, now time.Time) (time.Time, bool) {
	sup, guide := st.SupervisorRole(), st.GuideRole()
	if sup.Relay.Open(now) && sup.Relay.To.Is(ag.Party) || guide.Relay.Open(now) && guide.Relay.To.Is(ag.Party) {
		return time.Time{}, false
	}
	if ag.Execution != nil || ag.Task == "" || ag.Done || roleRun(st, ag.Party) || keptBy(st, ag, now) != "" || ag.Import.Pending(now) || staleAfter <= 0 {
		return time.Time{}, false
	}
	if _, live := claude.Live(sessions, ag.Party); live || mergeInFlight(st, ag.Party, proc.Alive) {
		return time.Time{}, false
	}
	last := agentActivity(cfg, st, ag)
	return last, !last.IsZero() && now.Sub(last) >= staleAfter
}

func rosterConfig(configs []*config.Config) (*config.Config, time.Duration) {
	if len(configs) > 0 && configs[0] != nil {
		return configs[0], configs[0].Agents.StaleAfter.Duration
	}
	return nil, 24 * time.Hour
}
