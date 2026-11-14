package harness

// The vocabulary scenarios may use (contract §2, §4, §5). The loader rejects anything else, so a
// misspelt name fails to load instead of matching nothing.

// Concepts, in catalogue order.
var Concepts = []string{"board", "eligibility", "epic", "pick", "gate", "decision", "wait", "source", "budget", "webhook", "invariant", "history", "leader", "shadow"}

// EventTypes are the event table's types (contract §5.2).
var EventTypes = []string{"observed", "item.removed", "phase.changed", "eligibility.changed", "decision", "shadow.pick", "shadow.held", "shadow.action", "wait.started", "wait.woken", "wait.ended", "wait.deadline", "wait.reobserved", "wait.unobserved", "wait.overdue", "source.degraded", "source.fresh", "webhook.missed", "refusal"}

// Metrics are the engine's metric names (contract §5.3).
var Metrics = []string{"engine_github_requests_total", "engine_github_graphql_points_total", "engine_github_rate_remaining", "engine_webhook_deliveries_total", "engine_observations_total", "engine_webhook_missed_total", "engine_source_degraded", "engine_polling_active", "engine_waits", "engine_wait_wakes_total", "engine_items", "engine_decisions_total", "engine_refusals_total", "engine_invariant_violations", "engine_last_decision_timestamp_seconds", "engine_history_dropped_total", "engine_reconcile_errors_total"}

// Invariants are the engine_invariant_violations series (contract §1.5).
var Invariants = []string{"wip-has-run-or-wait", "wait-within-deadline", "stage-age-under-p90", "refusal-not-repeated", "source-fresh-or-degraded", "gate-not-holding-repair", "pending-run-placed", "merge-reaches-result-check", "leader-decides"}

// preKeys are the keys of pre_configuration (contract §4.1).
var preKeys = []string{"fixture", "config", "github", "engine", "history", "jev", "clock"}

// deliverModes are the string values of a github_* tool's deliver argument.
var deliverModes = []string{"delivered", "lost", "never", "duplicate"}

// enums are the closed argument values of tools.
var enums = map[string]map[string][]string{
	"github_fault":      {argKind: {"http_error", "secondary_rate_limit", "rate_limit", "deliveries", "clear"}, "api": {"rest", "graphql", "app"}, "mode": {"lost", "never"}},
	"engine_fault":      {argKind: {"kill_leader", "restart", "stop_timers", "stop_decisions", "stop_freshness", "drop_waits", "forget_refusals", "gate_everything", "resume"}},
	"postgres_fault":    {argKind: {"down", "up"}},
	"webhook_post":      {"signature": {"valid", "invalid", "missing"}},
	"github_pr_review":  {"state": {"approved", "changes_requested", "commented"}},
	"github_sub_issue":  {"op": {"add", "remove"}},
	"github_dependency": {"op": {"add", "remove"}},
	"github_calls":      {"api": {"rest", "graphql", "app"}},
	"jev_set":           {"error": {"timeout", "server"}},
}

func in(s string, list []string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
