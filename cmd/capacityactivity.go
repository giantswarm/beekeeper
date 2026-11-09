package cmd

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

const transcriptResumeEvent = "agents.transcript-resume"

// Counts cover the last 24 hours, including retired workers. Starts count
// session identities, not the several agents.start lines one launch writes.
type providerActivity struct {
	Wakes       int `json:"wakes"`
	Resumes     int `json:"resumes"`
	FreshStarts int `json:"freshStarts"`
}

func activityCounts(st *state.State, events []state.Event, k config.Capacity, now time.Time) map[string]*providerActivity {
	out := map[string]*providerActivity{}
	seen := map[string]bool{}
	get := func(provider string) *providerActivity {
		if out[provider] == nil {
			out[provider] = &providerActivity{}
		}
		return out[provider]
	}
	for _, name := range k.ProviderNames() {
		get(name)
	}
	starts := slices.Concat(st.Starts, st.EndedWorkers)
	for _, s := range starts {
		provider, _ := resolveAgentProviderBucket(k, s.Harness, s.Model)
		provider = cmp.Or(provider, s.Harness, claudeComm)
		key := s.Session + "/" + s.HostSession
		if s.Execution != nil {
			key = string(s.Execution.Request.Ref.Execution)
		}
		if !seen[key] && !s.At.Before(now.Add(-24*time.Hour)) && !s.At.After(now) {
			get(provider).FreshStarts++
			seen[key] = true
		}
	}
	for _, e := range events {
		if e.At.Before(now.Add(-24*time.Hour)) || e.At.After(now) {
			continue
		}
		provider := "unknown"
		var latest time.Time
		for _, s := range starts {
			if !strings.HasPrefix(e.Detail, s.Name+": ") || s.At.After(e.At) || s.At.Before(latest) {
				continue
			}
			provider, _ = resolveAgentProviderBucket(k, s.Harness, s.Model)
			provider = cmp.Or(provider, s.Harness, claudeComm)
			latest = s.At
		}
		switch e.Verb {
		case transcriptResumeEvent:
			get(provider).Resumes++
		case resumedEvent:
			if strings.Contains(e.Detail, "SSH execution goes on in its own conversation") {
				get(provider).Resumes++
			}
		case agentWakeEvent:
			if strings.Contains(e.Detail, "refused") {
				continue
			}
			get(provider).Wakes++
			if strings.Contains(e.Detail, "resumed session") || strings.Contains(e.Detail, "resumed background session") {
				get(provider).Resumes++
			}
		}
	}
	return out
}
