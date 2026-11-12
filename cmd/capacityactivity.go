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
	type startedProvider struct {
		at       time.Time
		provider string
	}
	byName := map[string][]startedProvider{}
	starts := slices.Concat(st.Starts, st.EndedWorkers)
	for _, s := range starts {
		provider, _ := resolveAgentProviderBucket(k, s.Harness, s.Model)
		provider = cmp.Or(provider, s.Harness, claudeComm)
		byName[s.Name] = append(byName[s.Name], startedProvider{at: s.At, provider: provider})
		key := s.Session + "/" + s.HostSession
		if s.Session == "" && s.HostSession == "" {
			key = s.Name + "/" + s.Conversation + "/" + s.At.UTC().Format(time.RFC3339Nano)
		}
		if s.Execution != nil {
			key = string(s.Execution.Request.Ref.Execution)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		if !s.At.Before(now.Add(-24*time.Hour)) && !s.At.After(now) {
			get(provider).FreshStarts++
		}
	}
	for _, e := range events {
		if e.At.Before(now.Add(-24*time.Hour)) || e.At.After(now) {
			continue
		}
		provider := kindUnknown
		var latest time.Time
		for off := 0; ; {
			idx := strings.Index(e.Detail[off:], ": ")
			if idx < 0 {
				break
			}
			name := e.Detail[:off+idx]
			for _, s := range byName[name] {
				if s.at.After(e.At) || s.at.Before(latest) {
					continue
				}
				provider = s.provider
				latest = s.at
			}
			off += idx + 2
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
