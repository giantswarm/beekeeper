package engine

import (
	"github.com/giantswarm/beekeeper/internal/forge/github"
	"github.com/giantswarm/beekeeper/internal/metrics"
)

// GitHubObserver adapts a replica's metrics to the GitHub client's Observer: every request is counted
// in engine_github_requests_total (and its GraphQL points), and the rate-limit headers set
// engine_github_rate_remaining.
func GitHubObserver(m *metrics.Metrics) github.Observer {
	return func(r github.Request) {
		m.ObserveGitHub(r.API, r.Class, r.Operation, r.Code, r.Conditional, r.Points)
		if r.Limit != nil {
			m.SetRateRemaining(r.API, r.Limit.Remaining)
		}
	}
}
