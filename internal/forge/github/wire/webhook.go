package wire

import "time"

// Webhook events the engine subscribes to.
const (
	EventProjectsV2Item           = "projects_v2_item"
	EventIssues                   = "issues"
	EventSubIssues                = "sub_issues"
	EventIssueDependencies        = "issue_dependencies"
	EventPullRequest              = "pull_request"
	EventPullRequestReview        = "pull_request_review"
	EventCheckRun                 = "check_run"
	EventCheckSuite               = "check_suite"
	EventStatus                   = "status"
	EventPush                     = "push"
	EventInstallationRepositories = "installation_repositories"
	EventPing                     = "ping"
)

// Installation names the App installation a delivery is for.
type Installation struct {
	ID     int64  `json:"id"`
	NodeID string `json:"node_id,omitempty"`
}

// Organization names the org a delivery is for.
type Organization struct {
	Login  string `json:"login"`
	ID     int64  `json:"id,omitempty"`
	NodeID string `json:"node_id,omitempty"`
}

// Envelope is what every delivery carries besides its event's own fields. Payload content is never
// trusted beyond these ids (contract §1.3).
type Envelope struct {
	Action       string        `json:"action,omitempty"`
	Installation *Installation `json:"installation,omitempty"`
	Organization *Organization `json:"organization,omitempty"`
	Repository   *Repository   `json:"repository,omitempty"`
	Sender       *User         `json:"sender,omitempty"`
}

// ChangeFrom is an edited value's previous value.
type ChangeFrom struct {
	From string `json:"from"`
}

// IssueChanges are the changes of an issues.edited delivery.
type IssueChanges struct {
	Title *ChangeFrom `json:"title,omitempty"`
	Body  *ChangeFrom `json:"body,omitempty"`
}

// IssuesEvent is an issues delivery.
type IssuesEvent struct {
	Envelope
	Issue    Issue         `json:"issue"`
	Assignee *User         `json:"assignee,omitempty"`
	Label    *Label        `json:"label,omitempty"`
	Changes  *IssueChanges `json:"changes,omitempty"`
}

// ProjectsV2Item is the item of a projects_v2_item delivery: node ids only, no issue number.
type ProjectsV2Item struct {
	ID            int64      `json:"id"`
	NodeID        string     `json:"node_id"`
	ProjectNodeID string     `json:"project_node_id"`
	ContentNodeID string     `json:"content_node_id"`
	ContentType   string     `json:"content_type"` // Issue, PullRequest, DraftIssue
	Creator       *User      `json:"creator,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	ArchivedAt    *time.Time `json:"archived_at"`
}

// SelectOptionChange is a single-select value in a field_value change.
type SelectOptionChange struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// FieldValueChange says which field of an item changed (projects_v2_item.edited). From and To are a
// SelectOptionChange for single-select fields and a number for number fields.
type FieldValueChange struct {
	FieldNodeID   string `json:"field_node_id"`
	FieldType     string `json:"field_type"` // single_select, number, ...
	FieldName     string `json:"field_name"`
	ProjectNumber int    `json:"project_number"`
	From          any    `json:"from"`
	To            any    `json:"to"`
}

// ProjectsV2ItemChanges are the changes of a projects_v2_item.edited delivery.
type ProjectsV2ItemChanges struct {
	FieldValue *FieldValueChange `json:"field_value,omitempty"`
}

// ProjectsV2ItemEvent is a projects_v2_item delivery (org-level: no repository).
type ProjectsV2ItemEvent struct {
	Envelope
	ProjectsV2Item ProjectsV2Item         `json:"projects_v2_item"`
	Changes        *ProjectsV2ItemChanges `json:"changes,omitempty"`
}

// SubIssuesEvent is a sub_issues delivery, sent from the parent's side.
type SubIssuesEvent struct {
	Envelope
	ParentIssueID   int64      `json:"parent_issue_id"`
	ParentIssue     Issue      `json:"parent_issue"`
	ParentIssueRepo Repository `json:"parent_issue_repo"`
	SubIssueID      int64      `json:"sub_issue_id"`
	SubIssue        Issue      `json:"sub_issue"`
	SubIssueRepo    Repository `json:"sub_issue_repo"`
}

// IssueDependenciesEvent is an issue_dependencies delivery, sent from the blocked issue's side.
type IssueDependenciesEvent struct {
	Envelope
	BlockedIssueID    int64      `json:"blocked_issue_id"`
	BlockedIssue      Issue      `json:"blocked_issue"`
	BlockingIssueID   int64      `json:"blocking_issue_id"`
	BlockingIssue     Issue      `json:"blocking_issue"`
	BlockingIssueRepo Repository `json:"blocking_issue_repo"`
}

// PullRequestEvent is a pull_request delivery.
type PullRequestEvent struct {
	Envelope
	Number      int         `json:"number"`
	PullRequest PullRequest `json:"pull_request"`
	Before      string      `json:"before,omitempty"` // synchronize
	After       string      `json:"after,omitempty"`  // synchronize
}

// PullRequestReviewEvent is a pull_request_review delivery (review.state in lower case).
type PullRequestReviewEvent struct {
	Envelope
	Review      Review      `json:"review"`
	PullRequest PullRequest `json:"pull_request"`
}

// CheckSuiteRef is the suite of a check run.
type CheckSuiteRef struct {
	HeadSHA string `json:"head_sha"`
}

// PullRequestRef is a PR a check run belongs to.
type PullRequestRef struct {
	Number int       `json:"number"`
	Head   BranchRef `json:"head"`
	Base   BranchRef `json:"base"`
}

// CheckRunPayload is the check run of a check_run delivery.
type CheckRunPayload struct {
	CheckRun
	CheckSuite   CheckSuiteRef    `json:"check_suite"`
	PullRequests []PullRequestRef `json:"pull_requests"`
}

// CheckRunEvent is a check_run delivery.
type CheckRunEvent struct {
	Envelope
	CheckRun CheckRunPayload `json:"check_run"`
}

// StatusEvent is a status delivery (no action).
type StatusEvent struct {
	Envelope
	ID        int64     `json:"id"`
	SHA       string    `json:"sha"`
	Name      string    `json:"name"` // the repo's full name
	Context   string    `json:"context"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Pusher is who pushed.
type Pusher struct {
	Name string `json:"name"`
}

// PushCommit is the head commit of a push.
type PushCommit struct {
	ID string `json:"id"`
}

// PushEvent is a push delivery (no action).
type PushEvent struct {
	Envelope
	Ref        string       `json:"ref"`
	Before     string       `json:"before"`
	After      string       `json:"after"`
	Pusher     Pusher       `json:"pusher"`
	Commits    []PushCommit `json:"commits"`
	HeadCommit *PushCommit  `json:"head_commit"`
}

// RepoBrief is a repository in installation_repositories.
type RepoBrief struct {
	ID       int64  `json:"id"`
	NodeID   string `json:"node_id"`
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
}

// InstallationRepositoriesEvent is an installation_repositories delivery.
type InstallationRepositoriesEvent struct {
	Envelope
	RepositorySelection string      `json:"repository_selection"`
	RepositoriesAdded   []RepoBrief `json:"repositories_added"`
	RepositoriesRemoved []RepoBrief `json:"repositories_removed"`
}
