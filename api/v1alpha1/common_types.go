package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Observation sources (contract §1.3, §5.2).
const (
	SourceWebhook   = "webhook"
	SourceResync    = "resync"
	SourceReobserve = "reobserve"
	SourceStartup   = "startup"
	SourceDeadline  = "deadline"
	SourceEngine    = "engine"
)

// Wait kinds, states and reasons (contract §1.2, §5.1).
const (
	WaitOnDependency = "dependency"
	WaitOnCI         = "ci"
	WaitOnReview     = "review"

	WaitWatching   = "watching"
	WaitOverdue    = "overdue"
	WaitUnobserved = "unobserved"

	WaitReasonBudget         = "budget"
	WaitReasonSourceDegraded = "source-degraded"
	// WaitReasonHTTPPrefix is followed by the status code, e.g. "http-502".
	WaitReasonHTTPPrefix = "http-"

	AtDeadlineReobserve = "re-observe"
)

// Wait is what an Item or Change expects to observe next (design §11).
type Wait struct {
	// On is dependency, ci or review.
	On string `json:"on"`
	// Ref is the wake ref: issue:<ref>, ci:<repo>@<sha> or pr:<ref>.
	Ref   string      `json:"ref"`
	Since metav1.Time `json:"since"`
	// Deadline is when the engine re-observes once.
	Deadline   metav1.Time `json:"deadline"`
	AtDeadline string      `json:"atDeadline"`
	// State is watching, overdue or unobserved.
	State string `json:"state"`
	// Reason says why the wait is not watching: budget, source-degraded or http-<code>.
	Reason string `json:"reason,omitempty"`

	// NextCheck is when the engine next re-observes: the deadline while watching, the overdue backoff
	// while overdue, the transient retry while unobserved.
	NextCheck *metav1.Time `json:"nextCheck,omitempty"`
	// Backoff is the current overdue or retry backoff.
	Backoff *metav1.Duration `json:"backoff,omitempty"`
	// DeadlineHandled is true once the deadline's single re-observe has been made.
	DeadlineHandled bool `json:"deadlineHandled,omitempty"`
}

// Refusal is a read GitHub refused (404, 403 without rate limit, 410, 422). It is not retried until its
// inputs change.
type Refusal struct {
	Operation string      `json:"operation"`
	Code      int         `json:"code"`
	Since     metav1.Time `json:"since"`
	// Inputs is a hash of what would change the answer (installation repos, the object's last webhook,
	// configuration); the read is retried only when it differs.
	Inputs string `json:"inputs,omitempty"`
	// Retries counts reads made with unchanged inputs (the refusal-not-repeated invariant).
	Retries int `json:"retries,omitempty"`
}

// Observed is the last observed change and its source.
type Observed struct {
	At     metav1.Time `json:"at"`
	Source string      `json:"source"`
}

// WIP counts items and epics.
type WIP struct {
	Items int `json:"items"`
	Epics int `json:"epics"`
}
