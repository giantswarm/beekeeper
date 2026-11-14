// Package wire holds what goes over the wire between the GitHub client and GitHub: the REST and
// webhook JSON shapes, the GraphQL documents and their answers, and the REST routes with their
// operation ids. The client decodes these and the fake GitHub encodes them, so both speak the same
// subset of GitHub's API.
package wire

import (
	"encoding/json"
	"time"
)

// User is a GitHub account as REST and webhooks show it.
type User struct {
	Login string `json:"login"`
	ID    int64  `json:"id,omitempty"`
	Type  string `json:"type,omitempty"`
}

// Repository is a repository (repos/get, and the repository of webhook payloads).
type Repository struct {
	ID            int64  `json:"id"`
	NodeID        string `json:"node_id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Owner         User   `json:"owner"`
	Private       bool   `json:"private"`
	Visibility    string `json:"visibility,omitempty"`
	DefaultBranch string `json:"default_branch,omitempty"`
	Archived      bool   `json:"archived"`
}

// Label is an issue label.
type Label struct {
	Name string `json:"name"`
}

// Issue is an issue (issues/get, and webhook payloads).
type Issue struct {
	ID          int64      `json:"id"`
	NodeID      string     `json:"node_id"`
	Number      int        `json:"number"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	State       string     `json:"state"`                  // open, closed
	StateReason string     `json:"state_reason,omitempty"` // completed, not_planned, reopened
	User        User       `json:"user"`
	Assignees   []User     `json:"assignees"`
	Labels      []Label    `json:"labels"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ClosedAt    *time.Time `json:"closed_at"`
	// RepositoryURL is https://api.github.com/repos/{owner}/{repo}.
	RepositoryURL string `json:"repository_url,omitempty"`
}

// BranchRef is a PR's head or base.
type BranchRef struct {
	Ref  string      `json:"ref"`
	SHA  string      `json:"sha"`
	Repo *Repository `json:"repo,omitempty"`
}

// PullRequest is a pull request (pulls/get, and webhook payloads).
type PullRequest struct {
	ID             int64      `json:"id"`
	NodeID         string     `json:"node_id"`
	Number         int        `json:"number"`
	Title          string     `json:"title"`
	State          string     `json:"state"` // open, closed
	Draft          bool       `json:"draft"`
	Merged         bool       `json:"merged"`
	MergedAt       *time.Time `json:"merged_at"`
	MergeCommitSHA string     `json:"merge_commit_sha,omitempty"`
	MergedBy       *User      `json:"merged_by,omitempty"`
	User           User       `json:"user"`
	Head           BranchRef  `json:"head"`
	Base           BranchRef  `json:"base"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	ClosedAt       *time.Time `json:"closed_at"`
}

// Review is a submitted review (pulls/list-reviews).
type Review struct {
	ID          int64     `json:"id"`
	User        User      `json:"user"`
	State       string    `json:"state"` // APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED (lower case in webhooks)
	CommitID    string    `json:"commit_id"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// CheckRun is a check run (checks/list-for-ref, check_run webhooks).
type CheckRun struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	HeadSHA     string     `json:"head_sha"`
	Status      string     `json:"status"`     // queued, in_progress, completed
	Conclusion  string     `json:"conclusion"` // empty until completed
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
}

// CheckRunList is the answer of checks/list-for-ref.
type CheckRunList struct {
	TotalCount int        `json:"total_count"`
	CheckRuns  []CheckRun `json:"check_runs"`
}

// Status is a commit status (repos/list-commit-statuses-for-ref, status webhooks).
type Status struct {
	ID          int64     `json:"id"`
	Context     string    `json:"context"`
	State       string    `json:"state"` // pending, success, failure, error
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// CombinedStatus is the answer of repos/get-combined-status-for-ref.
type CombinedStatus struct {
	State      string   `json:"state"`
	SHA        string   `json:"sha"`
	TotalCount int      `json:"total_count"`
	Statuses   []Status `json:"statuses"`
}

// Rule types of repos/get-branch-rules.
const (
	RuleRequiredStatusChecks = "required_status_checks"
	RulePullRequest          = "pull_request"
)

// BranchRule is one rule that applies to a branch (repos/get-branch-rules).
type BranchRule struct {
	Type       string          `json:"type"`
	Parameters json.RawMessage `json:"parameters,omitempty"`
}

// RequiredStatusChecksParams are the parameters of a required_status_checks rule.
type RequiredStatusChecksParams struct {
	RequiredStatusChecks []struct {
		Context string `json:"context"`
	} `json:"required_status_checks"`
	StrictRequiredStatusChecksPolicy bool `json:"strict_required_status_checks_policy"`
}

// PullRequestRuleParams are the parameters of a pull_request rule.
type PullRequestRuleParams struct {
	RequiredApprovingReviewCount int  `json:"required_approving_review_count"`
	RequireCodeOwnerReview       bool `json:"require_code_owner_review"`
}

// InstallationRepositories is the answer of apps/list-repos-accessible-to-installation.
type InstallationRepositories struct {
	TotalCount   int          `json:"total_count"`
	Repositories []Repository `json:"repositories"`
}

// InstallationToken is the answer of apps/create-installation-access-token.
type InstallationToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// InstallationTokenRequest is the body of apps/create-installation-access-token. Without
// permissions the token gets every permission the App was granted.
type InstallationTokenRequest struct {
	Permissions map[string]string `json:"permissions,omitempty"`
}

// PermissionRead is the read level of an App permission.
const PermissionRead = "read"

// ReadPermissions are the permissions the engine's installation tokens ask for: read, of what it
// reads, and nothing else (contract §1.6).
func ReadPermissions() map[string]string {
	return map[string]string{
		"issues":                PermissionRead,
		"pull_requests":         PermissionRead,
		"checks":                PermissionRead,
		"statuses":              PermissionRead,
		"contents":              PermissionRead,
		"metadata":              PermissionRead,
		"organization_projects": PermissionRead,
	}
}

// HookDelivery is an entry of the App's webhook delivery log (apps/list-webhook-deliveries).
type HookDelivery struct {
	ID             int64     `json:"id"`
	GUID           string    `json:"guid"`
	DeliveredAt    time.Time `json:"delivered_at"`
	Redelivery     bool      `json:"redelivery"`
	Duration       float64   `json:"duration"`
	Status         string    `json:"status"`
	StatusCode     int       `json:"status_code"`
	Event          string    `json:"event"`
	Action         string    `json:"action,omitempty"`
	InstallationID int64     `json:"installation_id,omitempty"`
	RepositoryID   int64     `json:"repository_id,omitempty"`
}

// HookDeliveryDetail is one logged delivery with its request and response (apps/get-webhook-delivery).
type HookDeliveryDetail struct {
	HookDelivery
	URL      string              `json:"url"`
	Request  HookDeliveryRequest `json:"request"`
	Response HookDeliveryReply   `json:"response"`
}

// HookDeliveryRequest is the request GitHub sent.
type HookDeliveryRequest struct {
	Headers map[string]string `json:"headers"`
	Payload json.RawMessage   `json:"payload"`
}

// HookDeliveryReply is the receiver's answer as GitHub logged it.
type HookDeliveryReply struct {
	Headers map[string]string `json:"headers"`
	Payload string            `json:"payload"`
}

// ErrorBody is GitHub's REST error answer.
type ErrorBody struct {
	Message          string `json:"message"`
	DocumentationURL string `json:"documentation_url,omitempty"`
}

// Rate-limit headers.
const (
	HeaderRateLimit     = "X-Ratelimit-Limit"
	HeaderRateRemaining = "X-Ratelimit-Remaining"
	HeaderRateReset     = "X-Ratelimit-Reset" // epoch seconds
	HeaderRateUsed      = "X-Ratelimit-Used"
	HeaderRateResource  = "X-Ratelimit-Resource"
	HeaderRetryAfter    = "Retry-After" // seconds
)

// SecondaryRateLimitMessage starts the message of a secondary rate limit's 403.
const SecondaryRateLimitMessage = "You have exceeded a secondary rate limit"

// Webhook headers.
const (
	HeaderEvent     = "X-GitHub-Event"
	HeaderDelivery  = "X-GitHub-Delivery"
	HeaderSignature = "X-Hub-Signature-256"
	HeaderHookID    = "X-GitHub-Hook-ID"
)
