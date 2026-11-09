package cmd

import (
	"cmp"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// Counts cover the last 24 hours, including retired workers. Starts count
// session identities, not the several agents.start lines one launch writes.
type providerActivity struct {
	Wakes       int `json:"wakes"`
	Resumes     int `json:"resumes"`
	FreshStarts int `json:"freshStarts"`
}

func activityCounts(st *state.State, events []state.Event, k config.Capacity, now time.Time) map[string]*providerActivity {
	out := map[string]*providerActivity{}
	byName := map[string]string{}
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
	for _, s := range append(append([]state.Start{}, st.Starts...), st.EndedWorkers...) {
		provider, _ := resolveAgentProviderBucket(k, s.Harness, s.Model)
		provider = cmp.Or(provider, s.Harness, "claude")
		byName[s.Name] = provider
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
		name, _, _ := strings.Cut(e.Detail, ":")
		provider := cmp.Or(byName[name], "unknown")
		switch e.Verb {
		case "agents.wake":
			if strings.Contains(e.Detail, "refused") {
				continue
			}
			get(provider).Wakes++
			if strings.Contains(e.Detail, "resumed session") {
				get(provider).Resumes++
			}
		}
	}
	return out
}
