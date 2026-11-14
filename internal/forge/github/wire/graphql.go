package wire

import (
	"encoding/json"
	"time"
)

// GraphQLRequest is the body of POST /graphql.
type GraphQLRequest struct {
	Query         string         `json:"query"`
	OperationName string         `json:"operationName,omitempty"`
	Variables     map[string]any `json:"variables,omitempty"`
}

// GraphQLResponse is GraphQL's answer. GitHub answers a primary rate limit with 200 and an error of
// type RATE_LIMITED.
type GraphQLResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []GraphQLError  `json:"errors,omitempty"`
}

// GraphQL error types GitHub uses.
const (
	ErrorTypeRateLimited = "RATE_LIMITED"
	ErrorTypeNotFound    = "NOT_FOUND"
	ErrorTypeForbidden   = "FORBIDDEN"
)

// GraphQLError is one GraphQL error.
type GraphQLError struct {
	Type    string `json:"type,omitempty"`
	Message string `json:"message"`
	Path    []any  `json:"path,omitempty"`
}

// RateLimit is GraphQL's rateLimit object; every document asks for it to learn its cost.
type RateLimit struct {
	Cost      int       `json:"cost"`
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	Used      int       `json:"used"`
	ResetAt   time.Time `json:"resetAt"`
}

// boardItemFields selects what the engine needs of a board item and its issue. Its nested
// connections (assignees, labels, sub-issues, blockers, closing PRs) are what a page of items costs:
// GitHub charges one request per item for each, so a page of n items costs about (1 + 5n)/100 points.
// Sub-issues and blockers come with their state only: their board status would need one more
// connection per item and sub-issue. The summaries are plain objects (free): their totals tell a
// truncated list (more than 20 sub-issues, more than 10 blockers) from a complete one.
const boardItemFields = `
fragment BoardItemFields on ProjectV2Item {
  id
  type
  isArchived
  updatedAt
  project { number }
  status: fieldValueByName(name: "Status") { ... on ProjectV2ItemFieldSingleSelectValue { name optionId updatedAt } }
  team: fieldValueByName(name: "Team") { ... on ProjectV2ItemFieldSingleSelectValue { name optionId } }
  kind: fieldValueByName(name: "Kind") { ... on ProjectV2ItemFieldSingleSelectValue { name optionId } }
  priority: fieldValueByName(name: "Priority") { ... on ProjectV2ItemFieldNumberValue { number } }
  content {
    __typename
    ... on Issue {
      id
      number
      title
      body
      state
      stateReason
      createdAt
      updatedAt
      repository { nameWithOwner isPrivate }
      assignees(first: 10) { nodes { login } }
      labels(first: 20) { nodes { name } }
      parent { number repository { nameWithOwner } }
      subIssuesSummary { total completed }
      subIssues(first: 20) { nodes { number state stateReason repository { nameWithOwner } } }
      issueDependenciesSummary { blockedBy totalBlockedBy }
      blockedBy(first: 10) { nodes { number state stateReason repository { nameWithOwner } } }
      closedByPullRequestsReferences(first: 10, includeClosedPrs: true) {
        nodes { number state isDraft merged repository { nameWithOwner } }
      }
    }
    ... on PullRequest { number repository { nameWithOwner } }
  }
}
`

// BoardSchemaQuery reads the board's single-select fields and their options (1 point).
const BoardSchemaQuery = `query BoardSchema($org: String!, $number: Int!) {
  rateLimit { cost limit remaining used resetAt }
  organization(login: $org) {
    projectV2(number: $number) {
      id
      fields(first: 50) {
        nodes {
          ... on ProjectV2SingleSelectField { id name options { id name } }
        }
      }
    }
  }
}
`

// BoardTeamItemsQuery reads a team's active items and its Inbox bugs as two aliased, filtered
// items(query:) connections in one request. A follow-up page leaves out the alias that is done.
const BoardTeamItemsQuery = `query BoardTeamItems(
  $org: String!, $number: Int!,
  $activeQuery: String!, $activeFirst: Int!, $activeAfter: String, $withActive: Boolean!,
  $inboxQuery: String!, $inboxFirst: Int!, $inboxAfter: String, $withInbox: Boolean!
) {
  rateLimit { cost limit remaining used resetAt }
  organization(login: $org) {
    projectV2(number: $number) {
      id
      active: items(first: $activeFirst, after: $activeAfter, query: $activeQuery) @include(if: $withActive) {
        totalCount
        pageInfo { hasNextPage endCursor }
        nodes { ...BoardItemFields }
      }
      inbox: items(first: $inboxFirst, after: $inboxAfter, query: $inboxQuery) @include(if: $withInbox) {
        totalCount
        pageInfo { hasNextPage endCursor }
        nodes { ...BoardItemFields }
      }
    }
  }
}
` + boardItemFields

// BoardItemsByNodeQuery resolves board item node ids (1 point).
const BoardItemsByNodeQuery = `query BoardItemsByNode($ids: [ID!]!) {
  rateLimit { cost limit remaining used resetAt }
  nodes(ids: $ids) {
    __typename
    ... on ProjectV2Item { ...BoardItemFields }
  }
}
` + boardItemFields

// BoardProbeQuery asks for a team's recently updated items, with no nested connection (1 point per
// page). GitHub orders items(query:) by board position only (ProjectV2ItemOrder has no update time),
// so the client bounds the items with an updated: filter and pages through all of them.
const BoardProbeQuery = `query BoardProbe($org: String!, $number: Int!, $query: String!, $first: Int!, $after: String) {
  rateLimit { cost limit remaining used resetAt }
  organization(login: $org) {
    projectV2(number: $number) {
      items(first: $first, after: $after, query: $query) {
        totalCount
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          type
          updatedAt
          content {
            __typename
            ... on Issue { number repository { nameWithOwner } }
          }
        }
      }
    }
  }
}
`

// PullClosingIssuesQuery reads the issues a PR closes (closing keywords and manual Development links),
// with their state (1 point per page: one connection, nothing nested).
const PullClosingIssuesQuery = `query PullClosingIssues($owner: String!, $name: String!, $number: Int!, $first: Int!, $after: String) {
  rateLimit { cost limit remaining used resetAt }
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      closingIssuesReferences(first: $first, after: $after) {
        totalCount
        pageInfo { hasNextPage endCursor }
        nodes { number state stateReason repository { nameWithOwner } }
      }
    }
  }
}
`

// Page sizes of BoardTeamItems: (1 + 5×30) + (1 + 5×15) = 227 requests, 2 points per page.
const (
	ActivePageSize = 30
	InboxPageSize  = 15
	// ProbePageSize items without nested connections cost 1 point.
	ProbePageSize = 100
	// ClosingIssuesPageSize closing issues without nested connections cost 1 point.
	ClosingIssuesPageSize = 50
)

// BoardSchemaData is the data of BoardSchema.
type BoardSchemaData struct {
	RateLimit    *RateLimit `json:"rateLimit"`
	Organization *SchemaOrg `json:"organization"`
}

// SchemaOrg is the organization of BoardSchema.
type SchemaOrg struct {
	ProjectV2 *SchemaProject `json:"projectV2"`
}

// SchemaProject is the board of BoardSchema.
type SchemaProject struct {
	ID     string    `json:"id"`
	Fields FieldConn `json:"fields"`
}

// FieldConn is a connection of board fields.
type FieldConn struct {
	Nodes []SchemaField `json:"nodes"`
}

// SchemaField is a board field; only single-select fields carry a name and options in the answer.
type SchemaField struct {
	ID      string         `json:"id,omitempty"`
	Name    string         `json:"name,omitempty"`
	Options []SelectOption `json:"options,omitempty"`
}

// SelectOption is an option of a single-select field.
type SelectOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// BoardTeamItemsData is the data of BoardTeamItems.
type BoardTeamItemsData struct {
	RateLimit    *RateLimit    `json:"rateLimit"`
	Organization *TeamItemsOrg `json:"organization"`
}

// TeamItemsOrg is the organization of BoardTeamItems.
type TeamItemsOrg struct {
	ProjectV2 *TeamItemsProject `json:"projectV2"`
}

// TeamItemsProject is the board of BoardTeamItems with its two aliased item connections.
type TeamItemsProject struct {
	ID     string          `json:"id"`
	Active *ItemConnection `json:"active,omitempty"`
	Inbox  *ItemConnection `json:"inbox,omitempty"`
}

// ItemConnection is a page of board items.
type ItemConnection struct {
	TotalCount int           `json:"totalCount"`
	PageInfo   PageInfo      `json:"pageInfo"`
	Nodes      []ProjectItem `json:"nodes"`
}

// PageInfo is a connection's paging state.
type PageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor,omitempty"`
}

// Board item types.
const (
	ItemTypeIssue       = "ISSUE"
	ItemTypePullRequest = "PULL_REQUEST"
	ItemTypeDraftIssue  = "DRAFT_ISSUE"
	ItemTypeRedacted    = "REDACTED"
)

// ProjectItem is a board item as BoardItemFields selects it.
type ProjectItem struct {
	Typename   string             `json:"__typename,omitempty"`
	ID         string             `json:"id"`
	Type       string             `json:"type"`
	IsArchived bool               `json:"isArchived"`
	UpdatedAt  time.Time          `json:"updatedAt"`
	Project    *ProjectRef        `json:"project,omitempty"`
	Status     *SingleSelectValue `json:"status"`
	Team       *SingleSelectValue `json:"team"`
	Kind       *SingleSelectValue `json:"kind"`
	Priority   *NumberValue       `json:"priority"`
	Content    *ItemContent       `json:"content"`
}

// ProjectRef names an item's board.
type ProjectRef struct {
	Number int `json:"number"`
}

// SingleSelectValue is a single-select field's value.
type SingleSelectValue struct {
	Name      string     `json:"name"`
	OptionID  string     `json:"optionId"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}

// NumberValue is a number field's value.
type NumberValue struct {
	Number float64 `json:"number"`
}

// GraphQL states.
const (
	StateOpen   = "OPEN"
	StateClosed = "CLOSED"
	StateMerged = "MERGED"

	StateReasonCompleted  = "COMPLETED"
	StateReasonNotPlanned = "NOT_PLANNED"
	StateReasonReopened   = "REOPENED"
)

// ItemContent is an item's content: an Issue, a PullRequest or a DraftIssue (Typename says which).
type ItemContent struct {
	Typename    string     `json:"__typename"`
	ID          string     `json:"id,omitempty"`
	Number      int        `json:"number,omitempty"`
	Title       string     `json:"title,omitempty"`
	Body        string     `json:"body,omitempty"`
	State       string     `json:"state,omitempty"`
	StateReason string     `json:"stateReason,omitempty"`
	CreatedAt   *time.Time `json:"createdAt,omitempty"`
	UpdatedAt   *time.Time `json:"updatedAt,omitempty"`
	Repository  *RepoRef   `json:"repository,omitempty"`
	Assignees   *UserConn  `json:"assignees,omitempty"`
	Labels      *LabelConn `json:"labels,omitempty"`
	Parent      *IssueRef  `json:"parent,omitempty"`
	SubIssues   *IssueConn `json:"subIssues,omitempty"`
	BlockedBy   *IssueConn `json:"blockedBy,omitempty"`
	ClosedBy    *PullConn  `json:"closedByPullRequestsReferences,omitempty"`
	// SubIssuesSummary and DependenciesSummary count every link, also those the lists leave out.
	SubIssuesSummary    *SubIssuesSummary    `json:"subIssuesSummary,omitempty"`
	DependenciesSummary *DependenciesSummary `json:"issueDependenciesSummary,omitempty"`
}

// SubIssuesSummary is an issue's subIssuesSummary.
type SubIssuesSummary struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
}

// DependenciesSummary is an issue's issueDependenciesSummary: BlockedBy counts the open blockers,
// TotalBlockedBy every blocker.
type DependenciesSummary struct {
	BlockedBy      int `json:"blockedBy"`
	TotalBlockedBy int `json:"totalBlockedBy"`
}

// RepoRef is a repository as GraphQL names it.
type RepoRef struct {
	NameWithOwner string `json:"nameWithOwner"`
	IsPrivate     bool   `json:"isPrivate,omitempty"`
}

// UserConn is a connection of users.
type UserConn struct {
	Nodes []UserNode `json:"nodes"`
}

// UserNode is a user in a connection.
type UserNode struct {
	Login string `json:"login"`
}

// LabelConn is a connection of labels.
type LabelConn struct {
	Nodes []LabelNode `json:"nodes"`
}

// LabelNode is a label in a connection.
type LabelNode struct {
	Name string `json:"name"`
}

// IssueRef is a linked issue with its state.
type IssueRef struct {
	Number      int     `json:"number"`
	State       string  `json:"state,omitempty"`
	StateReason string  `json:"stateReason,omitempty"`
	Repository  RepoRef `json:"repository"`
}

// IssueConn is a connection of linked issues.
type IssueConn struct {
	Nodes []IssueRef `json:"nodes"`
}

// PullRef is a PR that closes an issue.
type PullRef struct {
	Number     int     `json:"number"`
	State      string  `json:"state"`
	IsDraft    bool    `json:"isDraft"`
	Merged     bool    `json:"merged"`
	Repository RepoRef `json:"repository"`
}

// PullConn is a connection of closing PRs.
type PullConn struct {
	Nodes []PullRef `json:"nodes"`
}

// BoardItemsByNodeData is the data of BoardItemsByNode: nil for ids that don't resolve.
type BoardItemsByNodeData struct {
	RateLimit *RateLimit     `json:"rateLimit"`
	Nodes     []*ProjectItem `json:"nodes"`
}

// BoardProbeData is the data of BoardProbe.
type BoardProbeData struct {
	RateLimit    *RateLimit `json:"rateLimit"`
	Organization *ProbeOrg  `json:"organization"`
}

// ProbeOrg is the organization of BoardProbe.
type ProbeOrg struct {
	ProjectV2 *ProbeProject `json:"projectV2"`
}

// ProbeProject is the board of BoardProbe.
type ProbeProject struct {
	Items ItemConnection `json:"items"`
}

// PullClosingIssuesData is the data of PullClosingIssues: Repository is nil when the repo can't be
// read, PullRequest when there is no such PR.
type PullClosingIssuesData struct {
	RateLimit  *RateLimit   `json:"rateLimit"`
	Repository *ClosingRepo `json:"repository"`
}

// ClosingRepo is the repository of PullClosingIssues.
type ClosingRepo struct {
	PullRequest *ClosingPull `json:"pullRequest"`
}

// ClosingPull is the PR of PullClosingIssues.
type ClosingPull struct {
	ClosingIssues ClosingIssueConn `json:"closingIssuesReferences"`
}

// ClosingIssueConn is a page of the issues a PR closes.
type ClosingIssueConn struct {
	TotalCount int        `json:"totalCount"`
	PageInfo   PageInfo   `json:"pageInfo"`
	Nodes      []IssueRef `json:"nodes"`
}
