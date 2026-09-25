package cmd

import (
	"cmp"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// metrics are a session's figures: its transcript's activity and what the
// process table, the memcap scopes, the event log and the leases attribute
// to it. Reading them costs no GitHub request.
type metrics struct {
	claude.Activity
	// Idle is the time since the transcript last changed.
	Idle time.Duration `json:"idle"`
	// Scopes are the capped runs (beekeeper run) it holds now, with their
	// cgroup's memory.
	Scopes []runScope `json:"scopes,omitempty"`
	// GitHubProcesses are its gh and devctl processes now, as budget lists
	// them.
	GitHubProcesses int `json:"githubProcesses"`
	// Merges are the gate's merge events it caused, from the event log.
	Merges merges `json:"merges"`
	// LeaseTimes are the leases it holds and for how long.
	LeaseTimes []leaseTime `json:"leaseTimes,omitempty"`
}

type runScope struct {
	Unit    string `json:"unit"`
	Command string `json:"command"`
	MemMiB  int    `json:"memMiB"`
}

// merges count the gate's events: queued in a lane, merged, refused by the
// gate, failed in devctl.
type merges struct {
	Queued  int `json:"queued"`
	Merged  int `json:"merged"`
	Refused int `json:"refused"`
	Failed  int `json:"failed"`
}

func (m *merges) add(o merges) {
	m.Queued += o.Queued
	m.Merged += o.Merged
	m.Refused += o.Refused
	m.Failed += o.Failed
}

type leaseTime struct {
	Env  string        `json:"env"`
	Held time.Duration `json:"held"`
}

// metricsTotals are the machine's totals and the sessions that spent the
// most in the last hour.
type metricsTotals struct {
	Total           claude.Counts `json:"total"`
	LastHour        claude.Counts `json:"lastHour"`
	GitHubProcesses int           `json:"githubProcesses"`
	Merges          merges        `json:"merges"`
	Top             []topSession  `json:"top,omitempty"`
}

type topSession struct {
	Name        string        `json:"name"`
	LastHour    claude.Counts `json:"lastHour"`
	ContextFill float64       `json:"contextFill,omitempty"`
}

// topSessions is how many sessions the totals name.
const topSessions = 3

// sessionMetrics reads every session's transcript window once (in
// parallel) for its work and its activity, and attributes the processes,
// scopes, merge events and leases to it.
func (a *app) sessionMetrics(sessions []*claude.Session, t *proc.Table, holders []lease.Holder) ([]claude.Work, []*metrics) {
	work := make([]claude.Work, len(sessions))
	out := make([]*metrics, len(sessions))
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	for i, s := range sessions {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			m := &metrics{}
			work[i], m.Activity = claude.ReadTranscript(s.Transcript, a.now)
			m.Price(a.cfg.Metrics)
			if !s.LastActive.IsZero() {
				m.Idle = a.now.Sub(s.LastActive).Round(time.Second)
			}
			out[i] = m
		})
	}
	wg.Wait()

	index := func(p state.Party) int {
		return slices.IndexFunc(sessions, func(s *claude.Session) bool { return s.Party().Is(p) })
	}
	if t != nil {
		for _, p := range t.ByPID {
			if p.Comm != "gh" && p.Comm != merge.Tool {
				continue
			}
			if s, ok := claude.OwnerOf(sessions, p.PID); ok {
				out[slices.Index(sessions, s)].GitHubProcesses++
			}
		}
	}
	for _, h := range holders {
		if i := index(h.Party()); i >= 0 {
			lt := leaseTime{Env: h.Env}
			if since, err := time.Parse(time.RFC3339, h.Since); err == nil {
				lt.Held = a.now.Sub(since).Round(time.Second)
			}
			out[i].LeaseTimes = append(out[i].LeaseTimes, lt)
		}
	}
	events, _ := a.store.Events(0, func(e state.Event) bool {
		return strings.HasPrefix(e.Verb, "merge") || e.Verb == guard.VerbStart || e.Verb == guard.VerbEnd
	})
	open := map[string]state.Event{}
	for _, e := range events {
		switch e.Verb {
		case guard.VerbStart:
			open[guard.RunScope(e.Detail)] = e
			continue
		case guard.VerbEnd:
			delete(open, guard.RunScope(e.Detail))
			continue
		}
		i := index(e.By)
		if i < 0 {
			continue
		}
		switch e.Verb {
		case "merge.queued":
			out[i].Merges.Queued++
		case "merged":
			out[i].Merges.Merged++
		case "merge.refused":
			out[i].Merges.Refused++
		case "merge.failed":
			out[i].Merges.Failed++
		}
	}
	for unit, e := range open {
		i := index(e.By)
		path := machine.FindMemcapScope(unit)
		if i < 0 || path == "" {
			continue
		}
		out[i].Scopes = append(out[i].Scopes, runScope{Unit: unit, Command: guard.RunCommand(e.Detail), MemMiB: machine.ReadScope(path).CurrentMiB})
	}
	return work, out
}

// totals sums the sessions' figures and names the top spenders of the
// last hour: by cost, those without a price after, by their tokens.
func totals(sessions []*claude.Session, ms []*metrics) *metricsTotals {
	tot := &metricsTotals{}
	tot.Total.CostUSD, tot.LastHour.CostUSD = new(float64), new(float64)
	top := make([]topSession, 0, len(ms))
	for i, m := range ms {
		tot.Total.Add(m.Total)
		tot.LastHour.Add(m.LastHour)
		tot.GitHubProcesses += m.GitHubProcesses
		tot.Merges.add(m.Merges)
		if m.LastHour.Tokens.Sum() > 0 {
			top = append(top, topSession{Name: sessions[i].Name, LastHour: m.LastHour, ContextFill: m.ContextFill})
		}
	}
	slices.SortStableFunc(top, func(x, y topSession) int {
		switch {
		case x.LastHour.CostUSD != nil && y.LastHour.CostUSD != nil:
			return cmp.Compare(*y.LastHour.CostUSD, *x.LastHour.CostUSD)
		case x.LastHour.CostUSD != nil:
			return -1
		case y.LastHour.CostUSD != nil:
			return 1
		}
		return cmp.Compare(y.LastHour.Tokens.Sum(), x.LastHour.Tokens.Sum())
	})
	tot.Top = top[:min(topSessions, len(top))]
	return tot
}

// costText is a cost in dollars, or "cost unknown".
func costText(c claude.Counts) string {
	if c.CostUSD == nil {
		return "cost unknown"
	}
	return fmt.Sprintf("$%.2f", *c.CostUSD)
}

// hourText is a session's last hour in a few words.
func hourText(c claude.Counts) string {
	s := fmt.Sprintf("%d turns, %d calls", c.Turns, c.ToolCalls)
	if c.ToolErrors > 0 {
		s += fmt.Sprintf(", %d errors", c.ToolErrors)
	}
	if c.GitHubCalls > 0 {
		s += fmt.Sprintf(", %d GitHub", c.GitHubCalls)
	}
	return s + ", " + costText(c)
}

// contextText is how full a session's context is, or its size without a
// known window.
func contextText(m *metrics) string {
	switch {
	case m == nil || m.Context == 0:
		return "-"
	case m.ContextWindow > 0:
		return fmt.Sprintf("%.0f%%", 100*m.ContextFill)
	}
	return fmt.Sprintf("%dk", m.Context/1000)
}

// runaways are the figures of a session over the configured thresholds,
// each with the key the watch says it once under.
func (a *app) runaway(s *claude.Session, m *metrics) map[string]string {
	rw := a.cfg.Metrics.Runaway
	out := map[string]string{}
	if n := m.LastHour.GitHubCalls; rw.GitHubCallsPerHour > 0 && n > rw.GitHubCallsPerHour {
		out["github"] = fmt.Sprintf("RUNAWAY: %q made %d GitHub calls in the last hour (threshold %d)", s.Name, n, rw.GitHubCallsPerHour)
	}
	if n := m.LastHour.SameErrors; rw.SameErrorRepeats > 0 && n > rw.SameErrorRepeats {
		out["errors"] = fmt.Sprintf("RUNAWAY: %q failed the same tool call %d times in the last hour (threshold %d): %s",
			s.Name, n, rw.SameErrorRepeats, truncate(m.LastHour.SameError, 60))
	}
	if rw.ContextFill > 0 && m.ContextFill > rw.ContextFill {
		out["context"] = fmt.Sprintf("RUNAWAY: %q's context is at %.0f%% of its %d tokens (threshold %.0f%%)",
			s.Name, 100*m.ContextFill, m.ContextWindow, 100*rw.ContextFill)
	}
	return out
}
