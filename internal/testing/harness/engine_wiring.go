package harness

import (
	"net/http"
	"time"

	"github.com/giantswarm/beekeeper/internal/engine"
	"github.com/giantswarm/beekeeper/internal/forge/github"
	"github.com/giantswarm/beekeeper/internal/metrics"
)

// The engine is built as the binary builds it (cmd/engine.go): a read-only GitHub client whose calls
// feed the replica's metrics, Postgres history and inbox, and the JEV client (here the fake itself,
// which needs the rule's answer that HTTP would drop). The differences are the scenario's: the fake
// clock, the settle tracker, the injected faults and short leader election.
func init() {
	buildEngine = func(d ReplicaDeps) (Engine, error) {
		m := metrics.New(d.Registry)
		forge, err := github.New(github.Config{
			BaseURL:        d.ForgeURL,
			AppID:          d.AppID,
			InstallationID: d.InstallationID,
			PrivateKeyPEM:  d.AppKeyPEM,
			Clock:          d.Clock,
			HTTPClient:     &http.Client{Timeout: 30 * time.Second},
			UserAgent:      d.UserAgent,
			Reserve:        d.Spec.Budget.ObservationReserve,
			Observer:       engine.GitHubObserver(m),
			Tracker:        d.Tracker,
		})
		if err != nil {
			return nil, err
		}
		le := engine.LeaderElection{}
		if d.Leader {
			le = engine.LeaderElection{
				Enabled:       true,
				LeaseDuration: leaderElection.lease,
				RenewDeadline: leaderElection.renew,
				RetryPeriod:   leaderElection.retry,
			}
		}
		return engine.New(engine.Options{
			Namespace:     d.Namespace,
			Replica:       d.Name,
			Version:       "test",
			Rest:          d.Rest,
			Clock:         d.Clock,
			Tracker:       d.Tracker,
			Forge:         forge,
			History:       d.History,
			HistoryReader: d.History,
			Inbox:         d.Inbox,
			JEV:           d.JEV,
			Registry:      d.Registry,
			Metrics:       m,
			Faults:        d.Faults,
			Leader:        le,
			Logger:        d.Logger,
			MetricsAddr:   "0",
			HealthAddr:    "0",
		})
	}
}
