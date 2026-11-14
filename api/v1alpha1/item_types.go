package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Item classes and phases (contract §1.2, §5.1).
const (
	ClassImplementable = "Implementable"
	ClassEpic          = "Epic"
	ClassIncident      = "Incident"
	ClassIgnored       = "Ignored"

	PhaseBacklog      = "Backlog"
	PhaseReady        = "Ready"
	PhaseImplementing = "Implementing"
	PhasePublished    = "Published"
	PhaseFixing       = "Fixing"
	PhaseMerging      = "Merging"
	PhaseMerged       = "Merged"
	PhaseWaiting      = "Waiting"
	PhaseValidating   = "Validating"
	PhaseDone         = "Done"
	PhasePlanned      = "Planned" // epics
	PhaseActive       = "Active"  // epics

	IssueOpen             = "open"
	IssueClosed           = "closed"
	IssueClosedNotPlanned = "closed_not_planned"
)

// Eligibility reasons.
const (
	ReasonNotUpNext                = "NotUpNext"
	ReasonTeamNotOptedIn           = "TeamNotOptedIn"
	ReasonKindNotImplementable     = "KindNotImplementable"
	ReasonAssignedToNonParticipant = "AssignedToNonParticipant"
	ReasonNotReady                 = "NotReady"
	ReasonBlockedByOpenIssue       = "BlockedByOpenIssue"
	ReasonClosed                   = "Closed"
)

// Would-action types.
const (
	ActionAssign      = "assign"
	ActionSetStatus   = "set-status"
	ActionStartRun    = "start-run"
	ActionTodo        = "todo"
	ActionSetSeverity = "set-severity"
)

// ItemIssue is the issue behind a board item.
type ItemIssue struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	// State is open, closed or closed_not_planned.
	State string `json:"state"`
	Body  string `json:"body,omitempty"`
	// NodeID is the issue's GraphQL node id.
	NodeID    string       `json:"nodeId,omitempty"`
	CreatedAt *metav1.Time `json:"createdAt,omitempty"`
	Labels    []string     `json:"labels,omitempty"`
}

// ItemBoard holds the board fields of the item.
type ItemBoard struct {
	Status   string `json:"status,omitempty"`
	Kind     string `json:"kind,omitempty"`
	Team     string `json:"team,omitempty"`
	Priority *int   `json:"priority,omitempty"`
	// NodeID is the board item's GraphQL node id (PVTI_…).
	NodeID string `json:"nodeId,omitempty"`
	// UpdatedAt is the board item's last update as GitHub reports it.
	UpdatedAt *metav1.Time `json:"updatedAt,omitempty"`
}

// Blocker is an issue that blocks this item.
type Blocker struct {
	Ref   string `json:"ref"`
	State string `json:"state"`
}

// SubIssue is an epic's sub-issue with its state and board status.
type SubIssue struct {
	Ref    string `json:"ref"`
	State  string `json:"state"`
	Status string `json:"status,omitempty"`
	// Phase is the sub-issue's phase when it is an Item, else derived from its status and state.
	Phase string `json:"phase,omitempty"`
}

// Eligibility says whether the engine may start the item (design §6.1).
type Eligibility struct {
	Value   bool     `json:"value"`
	Reasons []string `json:"reasons,omitempty"`
}

// ShadowPick is the pick the engine would make.
type ShadowPick struct {
	Person string `json:"person"`
	Rank   int    `json:"rank"`
}

// ShadowHeld is the gate that holds the item for a person.
type ShadowHeld struct {
	Person string `json:"person"`
	Gate   string `json:"gate"`
	Detail string `json:"detail,omitempty"`
}

// ShadowAction is an action the engine would take.
type ShadowAction struct {
	Key  string            `json:"key"`
	Type string            `json:"type"`
	Args map[string]string `json:"args,omitempty"`
}

// Shadow holds what the engine would do with the item.
type Shadow struct {
	Pick    *ShadowPick    `json:"pick,omitempty"`
	HeldBy  *ShadowHeld    `json:"heldBy,omitempty"`
	Actions []ShadowAction `json:"actions,omitempty"`
	// Rank is the item's place in the team's order (1-based), when it is eligible.
	Rank int `json:"rank,omitempty"`
	// Ready is the ready? rule's answer, when asked.
	Ready string `json:"ready,omitempty"`
	// Severity is the bug-severity? answer, for bugs.
	Severity string `json:"severity,omitempty"`
}

// ItemStatus is everything the engine observed and decided about a board item.
type ItemStatus struct {
	Issue     *ItemIssue `json:"issue,omitempty"`
	Board     *ItemBoard `json:"board,omitempty"`
	Class     string     `json:"class,omitempty"`
	Phase     string     `json:"phase,omitempty"`
	Assignees []string   `json:"assignees,omitempty"`
	// Owner is the participant the item belongs to: its assignee, or the epic's owner for sub-items.
	Owner     string     `json:"owner,omitempty"`
	Parent    string     `json:"parent,omitempty"`
	SubItems  []string   `json:"subItems,omitempty"`
	SubIssues []SubIssue `json:"subIssues,omitempty"`
	BlockedBy []Blocker  `json:"blockedBy,omitempty"`
	// SubIssuesTotal and BlockedByTotal are GitHub's exact counts, set only when they exceed the lists
	// above (the read truncates them): an epic with sub-issues the engine hasn't seen isn't Done, and an
	// item with blockers it hasn't seen isn't unblocked.
	SubIssuesTotal int `json:"subIssuesTotal,omitempty"`
	BlockedByTotal int `json:"blockedByTotal,omitempty"`
	// TargetRepos are the repos the work lands in.
	TargetRepos        []string `json:"targetRepos,omitempty"`
	AcceptanceCriteria bool     `json:"acceptanceCriteria"`
	Footprint          []string `json:"footprint,omitempty"`
	// Changes are the PRs that close the item.
	Changes  []string     `json:"changes,omitempty"`
	Eligible *Eligibility `json:"eligible,omitempty"`
	Shadow   *Shadow      `json:"shadow,omitempty"`
	Wait     *Wait        `json:"wait,omitempty"`
	Refusal  *Refusal     `json:"refusal,omitempty"`
	Observed *Observed    `json:"observed,omitempty"`
	// LeftAt is when the item left the tracked set by its status; it is deleted after itemRetention.
	LeftAt *metav1.Time `json:"leftAt,omitempty"`
	// InScope is false for items read only for their links (e.g. a blocker on another team).
	InScope *bool `json:"inScope,omitempty"`
}

// ItemSpec is empty: Items are engine-owned.
type ItemSpec struct{}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.status.class`
// +kubebuilder:printcolumn:name="Eligible",type=boolean,JSONPath=`.status.eligible.value`

// Item is a tracked board item.
type Item struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ItemSpec   `json:"spec,omitempty"`
	Status ItemStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ItemList lists Items.
type ItemList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Item `json:"items"`
}

func init() {
	register(&Item{}, &ItemList{})
}
