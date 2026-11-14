// Package forge is the engine's read-only view of GitHub. The dry run is built with a Reader: no
// mutating method exists on the type it gets (contract §1.6).
package forge

import (
	"context"
	"time"
)

// APIs, as the budget and the metrics name them.
const (
	APIREST    = "rest"
	APIGraphQL = "graphql"
	APIApp     = "app" // the App's own endpoints (webhook delivery log), authenticated with the App JWT
)

// Priority classes (design §10). Phase 1 spends only from observation and metrics.
const (
	ClassObservation = "observation"
	ClassMetrics     = "metrics"
)

// GraphQL operation names, as the fake GitHub matches them and the metrics label them.
const (
	OpBoardSchema    = "BoardSchema"
	OpBoardTeamItems = "BoardTeamItems"
	OpBoardItem      = "BoardItem"
	OpBoardProbe     = "BoardProbe"
	// OpBoardItemsByNode resolves board item node ids (nodes(ids:)); BoardItem and BoardItemsByNode
	// both send it, so the scenarios see one operation for "read the item a webhook named".
	OpBoardItemsByNode = "BoardItemsByNode"
	// OpPullClosingIssues reads the issues a PR closes (closingIssuesReferences), for a PR no Change owns yet.
	OpPullClosingIssues = "PullClosingIssues"
)

// REST operation ids (GitHub's OpenAPI description).
const (
	OpPullsGet              = "pulls/get"
	OpPullsListReviews      = "pulls/list-reviews"
	OpChecksListForRef      = "checks/list-for-ref"
	OpStatusesListForRef    = "repos/list-commit-statuses-for-ref"
	OpIssuesGet             = "issues/get"
	OpReposGet              = "repos/get"
	OpReposGetBranchRules   = "repos/get-branch-rules"
	OpListWebhookDeliveries = "apps/list-webhook-deliveries"
	OpGetWebhookDelivery    = "apps/get-webhook-delivery"
	OpCreateInstallToken    = "apps/create-installation-access-token" //nolint:gosec // G101: an operation name, not a credential
	OpListInstallationRepos = "apps/list-repos-accessible-to-installation"
)

// WithClass returns a context whose calls are charged to class.
func WithClass(ctx context.Context, class string) context.Context {
	return context.WithValue(ctx, classKey{}, class)
}

// ClassFrom returns the class of a context (observation when unset).
func ClassFrom(ctx context.Context) string {
	if c, ok := ctx.Value(classKey{}).(string); ok {
		return c
	}
	return ClassObservation
}

type classKey struct{}

// Reader is everything the engine reads from GitHub. Conditional reads take the ETag of the previous
// answer and report NotModified (a free 304) when nothing changed; the caller keeps its old value then.
type Reader interface {
	// BoardSchema reads the board's single-select fields and their options.
	BoardSchema(ctx context.Context, org string, project int) (BoardSchema, error)
	// BoardTeamItems runs the filtered team query (active statuses, open issues) and the team's Inbox
	// bugs as one GraphQL request. It never pages the whole board.
	BoardTeamItems(ctx context.Context, q TeamItemsQuery) (TeamItems, error)
	// BoardItem reads one board item by node id (e.g. to learn why a resync no longer returns it).
	// Found is false when the item was deleted from the board. It sends BoardItemsByNode.
	BoardItem(ctx context.Context, nodeID string) (item BoardItem, found bool, err error)
	// BoardItemsByNode resolves up to 100 board item node ids (from projects_v2_item webhooks) in one
	// GraphQL request. Ids that no longer resolve (deleted items) are missing from the map.
	BoardItemsByNode(ctx context.Context, nodeIDs []string) (map[string]BoardItem, error)
	// BoardProbe asks for the team's board items updated since a bound (1 point per 100 items).
	BoardProbe(ctx context.Context, q ProbeQuery) (Probe, error)

	Issue(ctx context.Context, ref string, etag string) (Issue, Meta, error)
	Repo(ctx context.Context, repo string, etag string) (Repo, Meta, error)
	PullRequest(ctx context.Context, ref string, etag string) (PullRequest, Meta, error)
	Reviews(ctx context.Context, ref string, etag string) ([]Review, Meta, error)
	CheckRuns(ctx context.Context, repo, sha string, etag string) ([]CheckRun, Meta, error)
	Statuses(ctx context.Context, repo, sha string, etag string) ([]CommitStatus, Meta, error)
	// BranchRules returns the rules that apply to a branch: required checks and approvals.
	BranchRules(ctx context.Context, repo, branch string, etag string) (BranchRules, Meta, error)
	// InstallationRepos lists the repos (owner/name) the App's installation covers. Repos outside it
	// send no webhooks, and a private one among them is refused.
	InstallationRepos(ctx context.Context, etag string) ([]string, Meta, error)

	// Deliveries lists the App's webhook deliveries newer than the cursor (the last id seen), oldest
	// first, and returns the new cursor.
	Deliveries(ctx context.Context, cursor string) ([]Delivery, string, error)
	// Delivery reads one logged delivery with its request payload.
	Delivery(ctx context.Context, id string) (DeliveryDetail, error)

	// PullClosingIssues reads the issues a PR closes (one small GraphQL query), so a new PR wakes only those
	// Items instead of a board resync.
	PullClosingIssues(ctx context.Context, ref string) ([]LinkedIssue, error)

	// Budget returns the rate limits as GitHub last reported them, and the reads the reserve deferred.
	Budget() Budget
}

// Meta describes a conditional REST answer.
type Meta struct {
	ETag        string
	NotModified bool
}

// BoardSchema holds the board's fields.
type BoardSchema struct {
	ProjectID string
	// Options maps a single-select field name (Status, Team, Kind) to its option names.
	Options map[string][]string
}

// TeamItemsQuery selects a team's tracked items.
type TeamItemsQuery struct {
	Org        string
	Project    int
	TeamOption string
	// Statuses are the active statuses (Up Next, In Progress, Blocked / Waiting, Validation).
	Statuses []string
	// InboxStatus and BugKind select the team's Inbox bugs in the same request.
	InboxStatus string
	BugKind     string
}

// TeamItems is the answer of BoardTeamItems.
type TeamItems struct {
	Active    []BoardItem
	InboxBugs []BoardItem
	// Points is the GraphQL cost of the request.
	Points int
}

// BoardItem is a board item with its issue.
type BoardItem struct {
	NodeID    string
	Archived  bool
	UpdatedAt time.Time
	Status    string
	Team      string
	Kind      string
	Priority  *int
	StatusAt  time.Time // when the status was set
	IsIssue   bool      // false for draft issues and PRs on the board
	Issue     Issue
	// Incomplete is true when GitHub answered errors about part of the item (e.g. a link it couldn't
	// resolve) beside the data: its links may miss entries, so conclude nothing from their absence.
	Incomplete bool
}

// Issue is an issue with the links the engine follows.
type Issue struct {
	Ref    string // owner/repo#n
	NodeID string
	Title  string
	Body   string
	// State is open or closed; StateReason is completed, not_planned or reopened.
	State       string
	StateReason string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Assignees   []string
	Labels      []string
	Private     bool
	Parent      string // ref of the parent issue
	SubIssues   []LinkedIssue
	BlockedBy   []LinkedIssue
	// SubIssuesTotal and BlockedByTotal are the exact totals (subIssuesSummary, issueDependenciesSummary);
	// when they exceed len(SubIssues) or len(BlockedBy) the lists are truncated.
	SubIssuesTotal int
	BlockedByTotal int
	// ClosedBy are the PRs that close the issue (closedByPullRequestsReferences, closed PRs included).
	ClosedBy []LinkedPR
}

// LinkedIssue is a linked issue with its state and, when it is on the board, its status.
type LinkedIssue struct {
	Ref         string
	State       string
	StateReason string
	Status      string
}

// LinkedPR is a PR that closes an issue.
type LinkedPR struct {
	Ref    string
	State  string // open, closed, merged
	Draft  bool
	Merged bool
}

// ProbeQuery asks for a team's most recently updated board items.
type ProbeQuery struct {
	Org        string
	Project    int
	TeamOption string
	// First is not used: the probe reads every item updated since UpdatedSince, 100 per page (1 point
	// each), since GitHub can't order a board's items by their update.
	First int
	// UpdatedSince bounds the items by their last update (the board filter has day granularity; the
	// client drops the older matches); zero means the last 24 hours.
	UpdatedSince time.Time
}

// Probe is the answer of BoardProbe: the items updated since the bound, newest first.
type Probe struct {
	Items []ProbeItem
	// Truncated is true when more items were updated since the bound than the probe reads (1,000).
	Truncated bool
}

// ProbeItem is a recently updated board item.
type ProbeItem struct {
	// IsIssue is false for draft issues and PRs on the board, which the engine doesn't track.
	IsIssue   bool
	NodeID    string
	IssueRef  string
	UpdatedAt time.Time
}

// Repo is a repository.
type Repo struct {
	FullName      string
	Private       bool
	DefaultBranch string
	Archived      bool
}

// PullRequest is a PR.
type PullRequest struct {
	Ref       string
	NodeID    string
	State     string // open, closed
	Merged    bool
	Draft     bool
	Author    string
	HeadSHA   string
	Base      string
	MergeSHA  string
	MergedAt  time.Time
	UpdatedAt time.Time
}

// Review is a submitted review.
type Review struct {
	By          string
	State       string // APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED
	SubmittedAt time.Time
	// ByTeams are the reviewer's teams in the org, when known (for another team's approval).
	ByTeams []string
}

// CheckRun is a check run on a commit.
type CheckRun struct {
	Name        string
	Status      string // queued, in_progress, completed
	Conclusion  string // success, failure, neutral, cancelled, skipped, timed_out, action_required
	StartedAt   time.Time
	CompletedAt time.Time
}

// CommitStatus is a commit status (the latest per context).
type CommitStatus struct {
	Context   string
	State     string // pending, success, failure, error
	CreatedAt time.Time
	UpdatedAt time.Time
}

// BranchRules are the merge requirements of a branch.
type BranchRules struct {
	RequiredChecks    []string
	RequiredApprovals int
	// CodeOwnerTeams are teams whose approval is required (CODEOWNERS with review required).
	CodeOwnerTeams []string
}

// Delivery is an entry of the App's webhook delivery log.
type Delivery struct {
	ID          string
	GUID        string // X-GitHub-Delivery
	DeliveredAt time.Time
	Event       string
	Action      string
	StatusCode  int
	Status      string // OK, or the failure
	Redelivery  bool
}

// Failed reports whether GitHub logged the delivery as failed.
func (d Delivery) Failed() bool { return d.StatusCode < 200 || d.StatusCode > 299 }

// DeliveryDetail is a logged delivery with its payload.
type DeliveryDetail struct {
	Delivery
	Payload []byte
}

// Budget is the client's view of the App's rate limits.
type Budget struct {
	REST, GraphQL, App APILimit
	DeferredREST       int
	DeferredGraphQL    int
	DeferredApp        int
}

// APILimit is one API's rate limit.
type APILimit struct {
	Limit     int
	Remaining int
	ResetAt   time.Time
	Used      int
	// Known is false until GitHub has reported this API's limit in a response's headers.
	Known bool
	// Paused is true while observation reads on this API are not being made: below the observation
	// reserve, or stopped by a primary or secondary rate limit, until PausedUntil.
	Paused      bool
	PausedUntil time.Time
}
