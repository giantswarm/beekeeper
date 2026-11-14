// Package engine wires one engine replica: the manager, the reconcilers, observation, the timer queue,
// history, metrics and leader election. The binary (cmd) and the scenario harness build it the same way.
package engine

import (
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/rest"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/faults"
	"github.com/giantswarm/beekeeper/internal/forge"
	"github.com/giantswarm/beekeeper/internal/history"
	"github.com/giantswarm/beekeeper/internal/inbox"
	"github.com/giantswarm/beekeeper/internal/jev"
	"github.com/giantswarm/beekeeper/internal/metrics"
	"github.com/giantswarm/beekeeper/internal/settle"
)

// Options configure one engine replica.
type Options struct {
	// Namespace holds every engine object (configuration and state).
	Namespace string
	// Replica is this replica's name (engine-0, engine-1); it is the forge User-Agent's comment.
	Replica string
	Version string
	Rest    *rest.Config
	Clock   clock.Clock
	// Tracker makes every asynchronous unit visible to the harness; nil in production.
	Tracker *settle.Tracker
	Forge   forge.Reader
	History history.Writer
	// HistoryReader rebuilds the decision keys and reads durations; replay uses it too.
	HistoryReader history.Reader
	Inbox         inbox.Drainer
	JEV           jev.Client
	// Registry is where the replica's metrics are registered (controller-runtime's own included).
	Registry *prometheus.Registry
	// Metrics are the replica's metrics when the caller made them already (to give the GitHub client
	// metrics.ObserveGitHub as its Observer); New makes them on Registry when nil.
	Metrics *metrics.Metrics
	// Workers is the number of concurrent Item and Change reconciles (default 4).
	Workers int
	// OnInstallation, when set, gets the Installation's spec when the leader first sees it and whenever
	// its generation changes (the binary applies the client's reserve and JEV's endpoint from it).
	OnInstallation func(*v1alpha1.InstallationSpec)
	Faults         *faults.Faults
	Leader         LeaderElection
	Logger         logr.Logger
	// MetricsAddr and HealthAddr are listen addresses; "0" disables them (scenarios).
	MetricsAddr string
	HealthAddr  string
}

// LeaderElection configures client-go leader election (real time, kept short in leader scenarios).
type LeaderElection struct {
	Enabled       bool
	ID            string
	LeaseDuration time.Duration
	RenewDeadline time.Duration
	RetryPeriod   time.Duration
}

// Stats are cumulative counters the harness diffs between steps.
type Stats interface {
	// Reconciles counts reconciles of one object (kind Item or Change, ref) by this replica.
	Reconciles(kind, ref string) int
	// Writes counts object writes (create, update, patch, delete) by this replica.
	Writes() int
	// IsLeader reports whether this replica leads.
	IsLeader() bool
}
