// Package metrics holds the engine's Prometheus metrics (contract §5.3) and the invariant evaluators
// (contract §1.5). Counters are incremented where things happen; the gauges derived from objects
// (waits, items, sources, invariants, the last decision) are computed at collection from a Snapshot the
// engine registers, so they are never stale and are exported by the leader only.
package metrics

import (
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/forge"
)

// Metric names (contract §5.3).
const (
	NameGitHubRequests        = "engine_github_requests_total"
	NameGitHubGraphQLPoints   = "engine_github_graphql_points_total"
	NameGitHubRateRemaining   = "engine_github_rate_remaining"
	NameWebhookDeliveries     = "engine_webhook_deliveries_total"
	NameObservations          = "engine_observations_total"
	NameWebhookMissed         = "engine_webhook_missed_total"
	NameSourceDegraded        = "engine_source_degraded"
	NamePollingActive         = "engine_polling_active"
	NameWaits                 = "engine_waits"
	NameWaitWakes             = "engine_wait_wakes_total"
	NameItems                 = "engine_items"
	NameDecisions             = "engine_decisions_total"
	NameRefusals              = "engine_refusals_total"
	NameInvariantViolations   = "engine_invariant_violations"
	NameLastDecisionTimestamp = "engine_last_decision_timestamp_seconds"
	NameHistoryDropped        = "engine_history_dropped_total"
	NameReconcileErrors       = "engine_reconcile_errors_total"
)

// Label names (contract §5.3).
const (
	labelAPI         = "api"
	labelClass       = "class"
	labelOperation   = "operation"
	labelCode        = "code"
	labelConditional = "conditional"
	labelEvent       = "event"
	labelOutcome     = "outcome"
	labelSource      = "source"
	labelKind        = "kind"
	labelOn          = "on"
	labelTrigger     = "trigger"
	labelPoint       = "point"
	labelMode        = "mode"
	labelController  = "controller"
	labelState       = "state"
	labelTeam        = "team"
	labelPhase       = "phase"
	labelInvariant   = "invariant"
)

// Webhook delivery outcomes (engine_webhook_deliveries_total{outcome}).
const (
	DeliveryAccepted         = "accepted"
	DeliveryBadSignature     = "bad_signature"
	DeliveryMissingSignature = "missing_signature"
	DeliveryDuplicate        = "duplicate"
	DeliveryTooLarge         = "too_large"
	DeliveryIgnored          = "ignored"
	DeliveryUnavailable      = "unavailable"
)

// Metrics are one replica's engine metrics. Counters and engine_github_rate_remaining are updated by
// the code that sees the event. The derived gauges (SourceDegraded, PollingActive, Waits, Items,
// InvariantViolations, LastDecisionTimestamp) are refreshed from the snapshot at every collection:
// don't set them, anything set is overwritten.
type Metrics struct {
	// GitHubRequests{api, class, operation, code, conditional}: every GitHub call (see ObserveGitHub).
	GitHubRequests *prometheus.CounterVec
	// GitHubGraphQLPoints{class}: GraphQL points spent.
	GitHubGraphQLPoints *prometheus.CounterVec
	// GitHubRateRemaining{api}: remaining calls as GitHub's rate-limit headers report them.
	GitHubRateRemaining *prometheus.GaugeVec
	// WebhookDeliveries{event, outcome}: deliveries the receiver answered (event: a subscribed event,
	// ping, or other; see DeliveryEvent).
	WebhookDeliveries *prometheus.CounterVec
	// Observations{source, kind}: observed changes, not reads.
	Observations *prometheus.CounterVec
	// WebhookMissed{event}: changes first seen by another source than a webhook.
	WebhookMissed *prometheus.CounterVec
	// WaitWakes{on, trigger}: waits woken (trigger: webhook, resync, deadline, poll).
	WaitWakes *prometheus.CounterVec
	// Decisions{point, mode, outcome}: decisions recorded with their outcome.
	Decisions *prometheus.CounterVec
	// Refusals{operation}: refusals recorded.
	Refusals *prometheus.CounterVec
	// HistoryDropped: history rows dropped while Postgres was down and the buffer full.
	HistoryDropped prometheus.Counter
	// ReconcileErrors{controller}: reconciles that returned an error.
	ReconcileErrors *prometheus.CounterVec

	// Derived at collection (leader only).
	SourceDegraded        *prometheus.GaugeVec // {source}
	PollingActive         *prometheus.GaugeVec // {source}
	Waits                 *prometheus.GaugeVec // {on, state}
	Items                 *prometheus.GaugeVec // {team, class, phase}
	InvariantViolations   *prometheus.GaugeVec // {invariant}
	LastDecisionTimestamp prometheus.Gauge

	mu       sync.Mutex // serialises collections of the derived gauges
	snapMu   sync.RWMutex
	snapshot func() Snapshot
}

// New creates the metrics and registers them with reg (nil registers nothing).
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		GitHubRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: NameGitHubRequests, Help: "GitHub API calls by API, priority class, operation, status code and conditional.",
		}, []string{labelAPI, labelClass, labelOperation, labelCode, labelConditional}),
		GitHubGraphQLPoints: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: NameGitHubGraphQLPoints, Help: "GraphQL points spent, by priority class.",
		}, []string{labelClass}),
		GitHubRateRemaining: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: NameGitHubRateRemaining, Help: "Remaining GitHub API calls as the rate-limit headers report them.",
		}, []string{labelAPI}),
		WebhookDeliveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: NameWebhookDeliveries, Help: "Webhook deliveries answered by the receiver, by event and outcome.",
		}, []string{labelEvent, labelOutcome}),
		Observations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: NameObservations, Help: "Observed changes (not reads), by source and kind.",
		}, []string{labelSource, labelKind}),
		WebhookMissed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: NameWebhookMissed, Help: "Changes first seen by a source other than a webhook, by event.",
		}, []string{labelEvent}),
		WaitWakes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: NameWaitWakes, Help: "Waits woken, by wait kind and trigger.",
		}, []string{labelOn, labelTrigger}),
		Decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: NameDecisions, Help: "Decisions recorded, by point, mode and outcome.",
		}, []string{labelPoint, labelMode, labelOutcome}),
		Refusals: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: NameRefusals, Help: "Reads GitHub refused, recorded once per refusal, by operation.",
		}, []string{labelOperation}),
		HistoryDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: NameHistoryDropped, Help: "History rows dropped because the buffer was full while Postgres was down.",
		}),
		ReconcileErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: NameReconcileErrors, Help: "Reconciles that returned an error, by controller.",
		}, []string{labelController}),

		SourceDegraded: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: NameSourceDegraded, Help: "1 when the observation source is degraded.",
		}, []string{labelSource}),
		PollingActive: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: NamePollingActive, Help: "1 while the source's waiters are polled because it is degraded.",
		}, []string{labelSource}),
		Waits: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: NameWaits, Help: "Waits held by Items and Changes, by kind and state.",
		}, []string{labelOn, labelState}),
		Items: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: NameItems, Help: "Tracked Items by team, class and phase.",
		}, []string{labelTeam, labelClass, labelPhase}),
		InvariantViolations: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: NameInvariantViolations, Help: "Objects violating each engine invariant (0 when it holds).",
		}, []string{labelInvariant}),
		LastDecisionTimestamp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: NameLastDecisionTimestamp, Help: "Unix time of the last decision pass.",
		}),
	}
	if reg != nil {
		reg.MustRegister(
			m.GitHubRequests, m.GitHubGraphQLPoints, m.GitHubRateRemaining, m.WebhookDeliveries,
			m.Observations, m.WebhookMissed, m.WaitWakes, m.Decisions, m.Refusals, m.HistoryDropped,
			m.ReconcileErrors, derived{m},
		)
	}
	return m
}

// SetSnapshot registers the function the derived gauges are computed from at each collection. It must
// return the zero Snapshot (Leader false) on a replica that doesn't lead, so sums across replicas don't
// double count. Until it is set, nothing derived is exported.
func (m *Metrics) SetSnapshot(f func() Snapshot) {
	m.snapMu.Lock()
	m.snapshot = f
	m.snapMu.Unlock()
}

// ObserveGitHub counts one GitHub call. Its signature matches the GitHub client's Observer callback:
// api (rest, graphql, app), class (observation, metrics), operation (REST operation id or GraphQL
// operation name), code (HTTP status; 0 when no response), conditional (If-None-Match sent), points
// (GraphQL points charged).
func (m *Metrics) ObserveGitHub(api, class, operation string, code int, conditional bool, points int) {
	m.GitHubRequests.WithLabelValues(api, class, operation, strconv.Itoa(code), strconv.FormatBool(conditional)).Inc()
	if api == forge.APIGraphQL {
		m.GitHubGraphQLPoints.WithLabelValues(class).Add(float64(max(points, 0)))
	}
}

// SetRateRemaining records an API's remaining calls from GitHub's rate-limit headers.
func (m *Metrics) SetRateRemaining(api string, remaining int) {
	m.GitHubRateRemaining.WithLabelValues(api).Set(float64(remaining))
}

// subscribedEvents are the webhook events the engine subscribes to (contract §1.3).
var subscribedEvents = map[string]bool{
	"projects_v2_item": true, "issues": true, "sub_issues": true, "issue_dependencies": true,
	"pull_request": true, "pull_request_review": true, "check_run": true, "check_suite": true,
	"status": true, "push": true, "installation_repositories": true, "ping": true,
}

// DeliveryEvent maps an X-GitHub-Event header to the event label: a subscribed event, ping, else other.
func DeliveryEvent(event string) string {
	if subscribedEvents[event] {
		return event
	}
	return "other"
}

// ObserveDelivery counts a delivery the receiver answered.
func (m *Metrics) ObserveDelivery(event, outcome string) {
	m.WebhookDeliveries.WithLabelValues(DeliveryEvent(event), outcome).Inc()
}

// derived is the collector of the gauges computed from the snapshot.
type derived struct{ m *Metrics }

func (d derived) Describe(ch chan<- *prometheus.Desc) {
	d.m.SourceDegraded.Describe(ch)
	d.m.PollingActive.Describe(ch)
	d.m.Waits.Describe(ch)
	d.m.Items.Describe(ch)
	d.m.InvariantViolations.Describe(ch)
	d.m.LastDecisionTimestamp.Describe(ch)
}

func (d derived) Collect(ch chan<- prometheus.Metric) {
	m := d.m
	m.snapMu.RLock()
	f := m.snapshot
	m.snapMu.RUnlock()
	var s Snapshot
	if f != nil {
		s = f()
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.SourceDegraded.Reset()
	m.PollingActive.Reset()
	m.Waits.Reset()
	m.Items.Reset()
	m.InvariantViolations.Reset()
	if s.Leader {
		m.fill(s)
	}
	m.SourceDegraded.Collect(ch)
	m.PollingActive.Collect(ch)
	m.Waits.Collect(ch)
	m.Items.Collect(ch)
	m.InvariantViolations.Collect(ch)
	if last := lastDecision(s); s.Leader && !last.IsZero() {
		m.LastDecisionTimestamp.Set(float64(last.UnixNano()) / float64(time.Second))
		m.LastDecisionTimestamp.Collect(ch)
	}
}

// fill sets the derived gauges from a leader's snapshot.
func (m *Metrics) fill(s Snapshot) {
	for _, src := range sourceNames(s) {
		var st v1alpha1.SourceStatus
		if s.Installation != nil {
			st = s.Installation.Status.Sources[src]
		}
		m.SourceDegraded.WithLabelValues(src).Set(b2f(st.State == v1alpha1.SourceDegraded))
		m.PollingActive.WithLabelValues(src).Set(b2f(st.Polling))
	}
	for i := range s.Items {
		it := &s.Items[i]
		if w := it.Status.Wait; w != nil {
			m.Waits.WithLabelValues(w.On, w.State).Inc()
		}
		if inScope(it) {
			m.Items.WithLabelValues(teamOf(it, s.Teams), it.Status.Class, it.Status.Phase).Inc()
		}
	}
	for i := range s.Changes {
		if w := s.Changes[i].Status.Wait; w != nil {
			m.Waits.WithLabelValues(w.On, w.State).Inc()
		}
	}
	for name, n := range Evaluate(s) {
		m.InvariantViolations.WithLabelValues(name).Set(float64(n))
	}
}

// teamOf names an Item's team: its team label, else the Team whose board option the item has.
func teamOf(it *v1alpha1.Item, teams []v1alpha1.Team) string {
	if t := it.Labels[v1alpha1.LabelTeam]; t != "" {
		return t
	}
	if it.Status.Board == nil || it.Status.Board.Team == "" {
		return ""
	}
	for i := range teams {
		if teams[i].Spec.BoardOption == it.Status.Board.Team {
			return teams[i].Name
		}
	}
	return ""
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
