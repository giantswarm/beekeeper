package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Change states.
const (
	PROpen   = "open"
	PRClosed = "closed"
	PRMerged = "merged"

	CIPending = "pending"
	CIGreen   = "green"
	CIRed     = "red"
	CINone    = "none"

	ReviewNone             = "none"
	ReviewApproved         = "approved"
	ReviewChangesRequested = "changes-requested"
)

// ChangePR is the observed pull request.
type ChangePR struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	// State is open, closed or merged.
	State    string `json:"state"`
	Draft    bool   `json:"draft,omitempty"`
	Author   string `json:"author,omitempty"`
	HeadSHA  string `json:"headSha,omitempty"`
	MergeSHA string `json:"mergeSha,omitempty"`
	Base     string `json:"base,omitempty"`
	NodeID   string `json:"nodeId,omitempty"`
}

// ChangeCI is the CI verdict on a commit.
type ChangeCI struct {
	SHA string `json:"sha"`
	// State is pending, green, red or none.
	State string `json:"state"`
	// Relevant are the checks and statuses that decide the verdict (the required set).
	Relevant []string `json:"relevant,omitempty"`
	// Ignored are the checks and statuses seen on the commit that don't count.
	Ignored []string `json:"ignored,omitempty"`
}

// ChangeReview is the review verdict.
type ChangeReview struct {
	// State is none, approved or changes-requested.
	State          string `json:"state"`
	NeedsOtherTeam bool   `json:"needsOtherTeam"`
	Approvals      int    `json:"approvals,omitempty"`
	Required       int    `json:"required,omitempty"`
}

// ChangeStatus is everything the engine observed and decided about a PR.
type ChangeStatus struct {
	PR          *ChangePR     `json:"pr,omitempty"`
	Items       []string      `json:"items,omitempty"`
	CI          *ChangeCI     `json:"ci,omitempty"`
	Review      *ChangeReview `json:"review,omitempty"`
	MergeResult *ChangeCI     `json:"mergeResult,omitempty"`
	Wait        *Wait         `json:"wait,omitempty"`
	Refusal     *Refusal      `json:"refusal,omitempty"`
	Observed    *Observed     `json:"observed,omitempty"`
	// Facts are the raw observations the verdicts are derived from (decide.Change), kept so a 304
	// after a restart still has them.
	Facts *ChangeFacts `json:"facts,omitempty"`
	// ETags of the conditional reads, keyed by REST operation id and path; kept so a restart doesn't
	// cost a full re-read.
	ETags map[string]string `json:"etags,omitempty"`
}

// CheckFact is a check run or a commit status (Kind check or status) on a commit.
type CheckFact struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Status is queued, in_progress or completed (a status's pending is in_progress).
	Status string `json:"status"`
	// Conclusion is success, failure, neutral, cancelled, skipped, timed_out or action_required
	// (a status's success, failure or error map to success, failure, failure).
	Conclusion  string       `json:"conclusion,omitempty"`
	StartedAt   *metav1.Time `json:"startedAt,omitempty"`
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// ReviewFact is a submitted review.
type ReviewFact struct {
	By    string   `json:"by"`
	State string   `json:"state"`
	Teams []string `json:"teams,omitempty"`
}

// ChangeFacts are the raw observations of a Change.
type ChangeFacts struct {
	HeadSHA string      `json:"headSha,omitempty"`
	Head    []CheckFact `json:"head,omitempty"`
	// MergeSHA and Merge are the checks and statuses on the merge commit.
	MergeSHA          string       `json:"mergeSha,omitempty"`
	Merge             []CheckFact  `json:"merge,omitempty"`
	Reviews           []ReviewFact `json:"reviews,omitempty"`
	RequiredChecks    []string     `json:"requiredChecks,omitempty"`
	RequiredApprovals int          `json:"requiredApprovals,omitempty"`
	CodeOwnerTeams    []string     `json:"codeOwnerTeams,omitempty"`
	RepoPrivate       bool         `json:"repoPrivate,omitempty"`
	// OwnerTeam is the repo's owning team, when known.
	OwnerTeam string `json:"ownerTeam,omitempty"`
}

// ChangeSpec is empty: Changes are engine-owned.
type ChangeSpec struct{}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="PR",type=string,JSONPath=`.status.pr.state`
// +kubebuilder:printcolumn:name="CI",type=string,JSONPath=`.status.ci.state`
// +kubebuilder:printcolumn:name="Wait",type=string,JSONPath=`.status.wait.on`

// Change is a PR that closes a tracked item.
type Change struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ChangeSpec   `json:"spec,omitempty"`
	Status ChangeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ChangeList lists Changes.
type ChangeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Change `json:"items"`
}

func init() {
	register(&Change{}, &ChangeList{})
}
