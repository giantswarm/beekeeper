package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Modes.
const (
	ModeDryRun = "dry-run"

	DecisionModeShadow = "shadow"
	DecisionModeRule   = "rule"

	SourceWebhooks = "webhooks"
	SourceBoard    = "board"

	SourceFresh    = "fresh"
	SourceDegraded = "degraded"

	SourceReasonDeliveriesFailed = "deliveries-failed"
	SourceReasonLogUnreadable    = "delivery-log-unreadable"
	SourceReasonProbeGap         = "probe-gap"
	SourceReasonBudget           = "budget"
	SourceReasonFilterInvalid    = "filter-invalid"
	// SourceReasonBoardUnreadable: a scheduled board read (resync, probe, filter check) failed or stands
	// refused.
	SourceReasonBoardUnreadable = "board-unreadable"

	ConditionBoardFilterValid = "BoardFilterValid"
	ReasonUnknownTeamOption   = "UnknownTeamOption"
	ReasonBoardFilterValid    = "Valid"
)

// BoardSpec names the ProjectV2 board.
type BoardSpec struct {
	Project int `json:"project"`
}

// ResyncSpec holds the resync intervals.
type ResyncSpec struct {
	Board   metav1.Duration `json:"board"`
	Changes metav1.Duration `json:"changes"`
}

// FreshnessSpec holds the freshness check intervals.
type FreshnessSpec struct {
	DeliveryLog metav1.Duration `json:"deliveryLog"`
	BoardProbe  metav1.Duration `json:"boardProbe"`
}

// PollingSpec holds the polling interval while a source is degraded.
type PollingSpec struct {
	Interval metav1.Duration `json:"interval"`
}

// BudgetSpec holds the API budget's reserve for observation.
type BudgetSpec struct {
	// ObservationReserve is the fraction of the limit observation never spends.
	ObservationReserve float64 `json:"observationReserve"`
}

// RetrySpec is the transient-error backoff.
type RetrySpec struct {
	Initial metav1.Duration `json:"initial"`
	Cap     metav1.Duration `json:"cap"`
	Jitter  float64         `json:"jitter,omitempty"`
}

// ReceiverSpec configures the webhook receiver.
type ReceiverSpec struct {
	Path         string `json:"path"`
	MaxBodyBytes int64  `json:"maxBodyBytes"`
}

// JEVSpec configures the JEV client.
type JEVSpec struct {
	ConfidenceFloor float64 `json:"confidenceFloor"`
	// URL defaults to https://api.typesafe.ai/v1/systemone.
	URL string `json:"url,omitempty"`
	// Timeout of one JEV call; defaults to 30s.
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// CIWaitSpec configures ci waits.
type CIWaitSpec struct {
	DeadlineFactor  float64         `json:"deadlineFactor"`
	DefaultDeadline metav1.Duration `json:"defaultDeadline"`
	MinHistory      int             `json:"minHistory"`
	AtDeadline      string          `json:"atDeadline"`
}

// FixedWaitSpec configures a wait with a fixed deadline.
type FixedWaitSpec struct {
	Deadline   metav1.Duration `json:"deadline"`
	AtDeadline string          `json:"atDeadline"`
}

// WaitsSpec configures every wait.
type WaitsSpec struct {
	CI                CIWaitSpec      `json:"ci"`
	Review            FixedWaitSpec   `json:"review"`
	Dependency        FixedWaitSpec   `json:"dependency"`
	MergeResult       FixedWaitSpec   `json:"mergeResult"`
	Grace             metav1.Duration `json:"grace"`
	OverdueBackoffCap metav1.Duration `json:"overdueBackoffCap"`
}

// ReadySpec holds the ready? rule's patterns.
type ReadySpec struct {
	AcceptanceCriteria string   `json:"acceptanceCriteria"`
	TargetRepo         string   `json:"targetRepo"`
	PlanningRepos      []string `json:"planningRepos,omitempty"`
	Todo               string   `json:"todo"`
}

// InstallationSpec is the engine's configuration for one GitHub org and its board.
type InstallationSpec struct {
	Org            string            `json:"org"`
	Board          BoardSpec         `json:"board"`
	Mode           string            `json:"mode"`
	Resync         ResyncSpec        `json:"resync"`
	Freshness      FreshnessSpec     `json:"freshness"`
	Polling        PollingSpec       `json:"polling"`
	Budget         BudgetSpec        `json:"budget"`
	Retry          RetrySpec         `json:"retry"`
	ItemRetention  metav1.Duration   `json:"itemRetention"`
	Receiver       ReceiverSpec      `json:"receiver"`
	JEV            JEVSpec           `json:"jev"`
	DecisionPoints map[string]string `json:"decisionPoints,omitempty"`
	Waits          WaitsSpec         `json:"waits"`
	Ready          ReadySpec         `json:"ready"`
	// Footprint is the pattern of a footprint line in an issue body.
	Footprint string `json:"footprint"`
}

// SourceStatus is the freshness of one observation source.
type SourceStatus struct {
	// State is fresh or degraded.
	State   string       `json:"state"`
	Reason  string       `json:"reason,omitempty"`
	Since   *metav1.Time `json:"since,omitempty"`
	Polling bool         `json:"polling"`
	// CheckedAt is when the last freshness verdict was made, as of the last write of the status (an idle
	// check writes nothing, so it can be older than the verdict the leader holds in memory).
	CheckedAt *metav1.Time `json:"checkedAt,omitempty"`
	// LastGapAt is when the last gap was found; the source is fresh again after a full check interval
	// without one.
	LastGapAt *metav1.Time `json:"lastGapAt,omitempty"`
	// DeliveredSinceGap is true once a delivery succeeded after the last gap (webhooks).
	DeliveredSinceGap bool `json:"deliveredSinceGap,omitempty"`
	// Cursor is the source's high-water mark: the last delivery log id seen (webhooks) or the newest
	// board item update reported by webhooks (board).
	Cursor string `json:"cursor,omitempty"`
}

// InstallationStatus is the engine's view of its sources and leadership.
type InstallationStatus struct {
	Sources        map[string]SourceStatus `json:"sources,omitempty"`
	Leader         string                  `json:"leader,omitempty"`
	LastDecisionAt *metav1.Time            `json:"lastDecisionAt,omitempty"`
	Conditions     []metav1.Condition      `json:"conditions,omitempty"`
	// BoardResyncAt is when the last board resync ran.
	BoardResyncAt *metav1.Time `json:"boardResyncAt,omitempty"`
	// ObservedGeneration is the spec generation the status describes.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced

// Installation configures the engine for one GitHub org and board.
type Installation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   InstallationSpec   `json:"spec,omitempty"`
	Status InstallationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// InstallationList lists Installations.
type InstallationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Installation `json:"items"`
}

func init() {
	register(&Installation{}, &InstallationList{})
}
