package loop

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/capacity"
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

// PaceYield is why a host with room does not take a start under
// supervisor.hostsFirst: the coordinator's own pick (local, its capacity
// --json) is a subscription behind its weekly pace and the host's pick (host,
// capacity --host … --json) spends another provider, such as a subscription
// whose harness no host runs. Placed there, the start would leave the
// subscription furthest behind its pace unspent while the coordinator has
// room for it. "" when the host spends the coordinator's provider, or the
// coordinator's pick is not behind its pace.
func PaceYield(local, host string) string {
	pick, hostPick := capacityChoice(local), capacityChoice(host)
	behind, ok := pick.BehindPace()
	if !ok || hostPick != nil && strings.EqualFold(hostPick.Provider, pick.Provider) {
		return ""
	}
	return fmt.Sprintf("the coordinator's pick %s is %.1f points behind its weekly pace; this host's pick is %s", pick.Summary(), behind, hostPick.Summary())
}

// capacityChoice is the choice of a capacity --json answer, nil when it has
// none. The answer is stdout then stderr: the document is its first value.
func capacityChoice(out string) *capacity.ChoiceView {
	var v struct {
		Choice *capacity.ChoiceView `json:"choice"`
	}
	if json.NewDecoder(strings.NewReader(out)).Decode(&v) != nil {
		return nil
	}
	return v.Choice
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
