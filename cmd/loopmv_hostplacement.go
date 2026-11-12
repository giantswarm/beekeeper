package cmd

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

func HostCapacityReason(out string, code int, err error) string {
	if err != nil {
		return Cmd.OneLine(err.Error())
	}
	if code != 0 {
		return fmt.Sprintf("capacity exited %d: %s", code, Cmd.OneLine(out))
	}
	var verdict struct {
		Room    *int     `json:"room"`
		Verdict string   `json:"verdict"`
		Blocks  []string `json:"blocks"`
	}
	if json.Unmarshal([]byte(out), &verdict) != nil || verdict.Room == nil {
		return "capacity answer is unreadable"
	}
	if *verdict.Room > 0 {
		return ""
	}
	if verdict.Verdict != "" {
		return Cmd.OneLine(verdict.Verdict)
	}
	if len(verdict.Blocks) > 0 {
		return Cmd.OneLine(strings.Join(verdict.Blocks, "; "))
	}
	return "no capacity for this task kind"
}

func (l *Loop) RecordHostPlacement(selector, repo, reason string) {
	err := l.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
		if st.HostPlacements == nil {
			st.HostPlacements = map[string]state.HostPlacement{}
		}
		st.HostPlacements[selector] = state.HostPlacement{At: l.Host.TimeNow(), Repository: repo, Reason: Cmd.OneLine(reason)}
		return nil, nil
	})
	if err != nil {
		l.SayOnce("host placement record "+selector, "SUPERVISOR HOST placement not saved: %v", err)
	}
}

type hostPlacementView struct {
	Selector   string    `json:"selector"`
	At         time.Time `json:"at,omitzero"`
	Repository string    `json:"repository,omitempty"`
	Reason     string    `json:"reason"`
	Update     string    `json:"update,omitempty"`
}

func (v hostPlacementView) Line() string {
	line := fmt.Sprintf("host %s: %s", v.Selector, v.Reason)
	if !v.At.IsZero() {
		line += fmt.Sprintf(" (last placement %s, repository %s)", v.At.UTC().Format(time.RFC3339), v.Repository)
	}
	if v.Update != "" {
		line += "; update waits: " + v.Update
	}
	return line
}

// CheckHostUtilization retains one starvation episode across loop restarts and
// coordinator releases. Successful remote starts end it; an idle desk without
// local worker starts never opens one. No audit-history replay is needed.
func (l *Loop) CheckHostUtilization(snapshot *state.State) error {
	now := l.Host.TimeNow()
	remote := snapshot.HostLastRemoteStart
	starts := slices.Concat(snapshot.Starts, snapshot.EndedWorkers)
	for _, s := range starts {
		if x := s.Execution; x != nil && !x.StartUnsent && (x.Phase == execution.Running || !x.LastActive.IsZero()) && s.At.After(remote) && !s.At.After(now) {
			remote = s.At
		}
	}
	since := snapshot.HostIdleSince
	if !remote.Before(since) {
		since = time.Time{}
	}
	for _, s := range starts {
		if s.Execution == nil && s.Role == "" && (s.Kind == "" || s.Kind == workerRole) && s.At.After(remote) && !s.At.After(now) && (since.IsZero() || s.At.Before(since)) {
			since = s.At
		}
	}
	if len(l.Host.Cfg().Supervisor.Hosts) == 0 || snapshot.Stop != nil {
		since = time.Time{}
		remote = now // Do not reuse starts from before hosts were enabled again.
	}
	return l.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
		if !st.HostIdleSince.Equal(since) {
			st.HostIdleReported = false
		}
		st.HostIdleSince, st.HostLastRemoteStart = since, remote
		if since.IsZero() || now.Sub(since) < time.Hour || st.HostIdleReported {
			return nil, nil
		}
		st.HostIdleReported = true
		finding := "Configured execution hosts have taken no start for an hour while workers started on the coordinator; local starts since " + since.UTC().Format(time.RFC3339) + "."
		for _, v := range l.Host.HostPlacementViews(st) {
			finding += " " + v.Line() + "."
		}
		return []state.Event{Cmd.Event(LoopParty, "supervisor."+superloop.KindFile, "[host-starvation %s]%s%s", since.UTC().Format(time.RFC3339), FileReportSeparator, finding)}, nil
	})
}
