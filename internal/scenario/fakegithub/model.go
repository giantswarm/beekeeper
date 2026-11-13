//go:build scenario

package fakegithub

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The states of issues, pull requests, reviews and checks, as GraphQL
// names them.
const (
	stateOpen   = "OPEN"
	stateClosed = "CLOSED"
	stateMerged = "MERGED"

	reviewApproved  = "APPROVED"
	reviewChanges   = "CHANGES_REQUESTED"
	reviewCommented = "COMMENTED"
	reviewDismissed = "DISMISSED"
	reviewPending   = "PENDING"

	checkQueued     = "QUEUED"
	checkInProgress = "IN_PROGRESS"
	checkCompleted  = "COMPLETED"

	conclusionSuccess = "SUCCESS"
	conclusionFailure = "FAILURE"

	statusPending = "PENDING"
	statusSuccess = "SUCCESS"
	statusFailure = "FAILURE"
	statusError   = "ERROR"

	kindUser = "User"
	kindOrg  = "Organization"
	kindBot  = "Bot"

	defaultBranch = "main"
	webHost       = "https://github.com"
)

// statusStates are the states a commit status takes.
var statusStates = []string{statusPending, statusSuccess, statusFailure, statusError}

// Account is a user, an organization or a bot.
type Account struct {
	Login  string
	Kind   string // User, Organization or Bot
	Name   string
	ID     int
	NodeID string
}

// Label is a repository's label.
type Label struct {
	ID          int
	NodeID      string
	Name        string
	Color       string
	Description string
}

// Comment is a comment on an issue or a pull request.
type Comment struct {
	ID        int
	NodeID    string
	Author    string
	Body      string
	CreatedAt time.Time
	UpdatedAt time.Time
	issue     *Issue
	pull      *Pull
}

// Issue is an issue.
type Issue struct {
	Repo        *Repo
	Number      int
	ID          int
	NodeID      string
	Title       string
	Body        string
	State       string // OPEN or CLOSED
	StateReason string // COMPLETED, NOT_PLANNED or REOPENED
	Author      string
	Labels      []string
	Assignees   []string
	Comments    []*Comment
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ClosedAt    *time.Time
	Parent      *Issue
	Subs        []*Issue
	BlockedBy   []*Issue
	// ClosedBy is the pull request whose merge closed it.
	ClosedBy *Pull
	// Reopened is when it was last reopened.
	Reopened *time.Time
}

// Review is a pull request review.
type Review struct {
	ID          int
	NodeID      string
	Author      string
	State       string
	Body        string
	Commit      string
	SubmittedAt time.Time
	pull        *Pull
}

// PullFile is one changed file of a pull request, when the scenario
// declares them instead of an origin.
type PullFile struct {
	Path      string
	Additions int
	Deletions int
}

// Pull is a pull request.
type Pull struct {
	Repo      *Repo
	Number    int
	ID        int
	NodeID    string
	Title     string
	Body      string
	State     string // OPEN, CLOSED or MERGED
	Draft     bool
	Author    string
	Base      string
	Head      string
	Labels    []string
	Assignees []string
	Comments  []*Comment
	Reviews   []*Review
	CreatedAt time.Time
	UpdatedAt time.Time
	ClosedAt  *time.Time
	MergedAt  *time.Time
	MergedBy  string
	// headSHA is the head the scenario declared, or the head at the time
	// the pull request was merged or closed; an open one reads its branch.
	headSHA string
	// MergeCommit is the commit on the base a merge made.
	MergeCommit string
	// Mergeable and MergeState are what the scenario declared; empty
	// means computed.
	Mergeable  string
	MergeState string
	// Decision is the review decision the scenario declared; empty means
	// computed from the reviews and the base's protection.
	Decision string
	// closes are the issues the scenario declared it closes, beside those
	// its body names.
	closes []*Issue
	// Files are the declared files; nil reads the origin.
	Files     []PullFile
	AutoMerge string // "", MERGE, SQUASH or REBASE
	// commits are the commits kept when it merged.
	commits []CommitInfo
}

// CheckRun is a check run on a commit.
type CheckRun struct {
	ID          int
	NodeID      string
	SHA         string
	Name        string
	Status      string // QUEUED, IN_PROGRESS or COMPLETED
	Conclusion  string // SUCCESS, FAILURE, … once completed
	StartedAt   time.Time
	CompletedAt *time.Time
	Run         *WorkflowRun
	repo        *Repo
}

// Status is a commit status.
type Status struct {
	ID          int
	NodeID      string
	SHA         string
	Context     string
	State       string // PENDING, SUCCESS, FAILURE or ERROR
	Description string
	TargetURL   string
	CreatedAt   time.Time
}

// Workflow is an Actions workflow.
type Workflow struct {
	ID   int
	Name string
	Path string
}

// WorkflowRun is one run of a workflow on a commit.
type WorkflowRun struct {
	ID         int
	Workflow   *Workflow
	SHA        string
	Branch     string
	Event      string
	Status     string // queued, in_progress or completed (REST's lower case)
	Conclusion string // success, failure, … once completed
	Attempt    int
	Number     int
	CreatedAt  time.Time
	UpdatedAt  time.Time
	repo       *Repo
}

// Asset is a release asset.
type Asset struct {
	ID          int
	NodeID      string
	Name        string
	ContentType string
	Content     []byte
	// File, when set, holds the content instead of Content: a path read at
	// each download, so a binary is not kept in memory.
	File      string
	CreatedAt time.Time
}

// Size is the asset's size in bytes; 0 for a file that cannot be read.
func (a *Asset) Size() int {
	if a.File == "" {
		return len(a.Content)
	}
	fi, err := os.Stat(a.File)
	if err != nil {
		return 0
	}
	return int(fi.Size())
}

// Bytes is the asset's content.
func (a *Asset) Bytes() ([]byte, error) {
	if a.File == "" {
		return a.Content, nil
	}
	return os.ReadFile(a.File) //nolint:gosec // a file the scenario names
}

// Release is a release.
type Release struct {
	ID          int
	NodeID      string
	Tag         string
	Name        string
	Body        string
	Draft       bool
	Prerelease  bool
	Target      string
	Author      string
	CreatedAt   time.Time
	PublishedAt *time.Time
	Assets      []*Asset
	repo        *Repo
}

// Protection is a branch's protection.
type Protection struct {
	NodeID          string
	Branch          string
	RequiredReviews int
	RequiredChecks  []string
	Strict          bool
	MergeQueue      bool
	// EnforceAdmins holds an administrator to the protection too.
	EnforceAdmins bool
}

// Repo is a repository.
type Repo struct {
	Owner         string
	Name          string
	ID            int
	NodeID        string
	Private       bool
	Description   string
	DefaultBranch string
	Labels        []*Label
	Issues        []*Issue
	Pulls         []*Pull
	Releases      []*Release
	Checks        []*CheckRun
	Statuses      []*Status
	Workflows     []*Workflow
	Runs          []*WorkflowRun
	Protections   []*Protection
	// Branches are the heads the scenario declared for a repository
	// without an origin; Files its files on the default branch.
	Branches  map[string]string
	Tags      map[string]string
	Files     map[string]string
	CreatedAt time.Time
	PushedAt  time.Time
	next      int
}

// Full is owner/name.
func (r *Repo) Full() string { return r.Owner + "/" + r.Name }

// URL is the repository's page.
func (r *Repo) URL() string { return webHost + "/" + r.Full() }

// URL is the issue's page.
func (i *Issue) URL() string { return fmt.Sprintf("%s/issues/%d", i.Repo.URL(), i.Number) }

// Ref is owner/name#number.
func (i *Issue) Ref() string { return fmt.Sprintf("%s#%d", i.Repo.Full(), i.Number) }

// URL is the pull request's page.
func (p *Pull) URL() string { return fmt.Sprintf("%s/pull/%d", p.Repo.URL(), p.Number) }

// Ref is owner/name#number.
func (p *Pull) Ref() string { return fmt.Sprintf("%s#%d", p.Repo.Full(), p.Number) }

// ProjectOption is one option of a single-select field.
type ProjectOption struct {
	ID   string
	Name string
}

// ProjectField is a field of a board.
type ProjectField struct {
	NodeID   string
	Name     string
	DataType string // SINGLE_SELECT, TEXT, NUMBER, DATE, TITLE, …
	Options  []ProjectOption
}

// FieldValue is an item's value of a field.
type FieldValue struct {
	Field     *ProjectField
	Option    string // a single select's option name
	OptionID  string
	Text      string
	UpdatedAt time.Time
}

// ProjectItem is an item of a board: an issue, a pull request or a draft.
type ProjectItem struct {
	NodeID    string
	ID        int
	Project   *Project
	Issue     *Issue
	Pull      *Pull
	Draft     string // a draft's title
	Archived  bool
	Values    map[string]*FieldValue
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Project is a Projects v2 board.
type Project struct {
	Owner  string
	Number int
	ID     int
	NodeID string
	Title  string
	Closed bool
	Fields []*ProjectField
	Items  []*ProjectItem
}

// Field is the field of that name, case-insensitively as GitHub's filter
// reads it.
func (p *Project) Field(name string) *ProjectField {
	for _, f := range p.Fields {
		if strings.EqualFold(f.Name, name) {
			return f
		}
	}
	return nil
}

// Option is the field's option of that name.
func (f *ProjectField) Option(name string) (ProjectOption, bool) {
	for _, o := range f.Options {
		if strings.EqualFold(o.Name, name) {
			return o, true
		}
	}
	return ProjectOption{}, false
}

// Model is the GitHub of one scenario. It is not safe for concurrent use:
// the Server serializes every read and write of it.
type Model struct {
	// Clock is the scenario's clock.
	Clock func() time.Time
	// Viewer is the login every token acts as.
	Viewer string
	// Origins reads and writes the repositories' bare origins; nil for a
	// model of declared heads alone.
	Origins *Origins

	accounts map[string]*Account
	repos    map[string]*Repo
	projects []*Project
	nodes    map[string]any
	// members are each organization's members beside the viewer.
	members map[string][]string
	seq     int
}

// NewModel is an empty GitHub with the viewer's account.
func NewModel(clock func() time.Time, viewer string) *Model {
	if clock == nil {
		clock = time.Now
	}
	m := &Model{Clock: clock, Viewer: viewer, accounts: map[string]*Account{}, repos: map[string]*Repo{}, nodes: map[string]any{}, members: map[string][]string{}, seq: 1000}
	m.Account(viewer, kindUser)
	return m
}

func (m *Model) now() time.Time { return m.Clock().UTC().Truncate(time.Second) }

// id is the next database id and its node id.
func (m *Model) id(prefix string, obj any) (int, string) {
	m.seq++
	node := prefix + "_" + strconv.Itoa(m.seq)
	m.nodes[node] = obj
	return m.seq, node
}

// Node is the object of a node id.
func (m *Model) Node(id string) (any, bool) {
	obj, ok := m.nodes[id]
	return obj, ok
}

func key(s string) string { return strings.ToLower(s) }

// Account returns the account of login, created as kind when new ("" for
// a user; a login ending in [bot] is a bot).
func (m *Model) Account(login, kind string) *Account {
	if a, ok := m.accounts[key(login)]; ok {
		if kind != "" && kind != kindUser {
			a.Kind = kind
		}
		return a
	}
	if kind == "" {
		kind = kindUser
		if strings.HasSuffix(login, "[bot]") {
			kind = kindBot
		}
	}
	a := &Account{Login: login, Kind: kind}
	prefix := map[string]string{kindUser: "U", kindOrg: "O", kindBot: "BOT"}[kind]
	a.ID, a.NodeID = m.id(prefix, a)
	m.accounts[key(login)] = a
	return a
}

// LookupAccount is the account of login, if any.
func (m *Model) LookupAccount(login string) (*Account, bool) {
	a, ok := m.accounts[key(login)]
	return a, ok
}

// AddRepo adds the repository owner/name; its owner becomes an
// organization unless it is an account already.
func (m *Model) AddRepo(full string) (*Repo, error) {
	owner, name, ok := strings.Cut(full, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("repository %q is not owner/name", full)
	}
	if r, ok := m.repos[key(full)]; ok {
		return r, nil
	}
	if _, known := m.accounts[key(owner)]; !known {
		m.Account(owner, kindOrg)
	}
	r := &Repo{Owner: owner, Name: name, DefaultBranch: defaultBranch, Branches: map[string]string{}, Tags: map[string]string{}, Files: map[string]string{}, next: 1}
	r.ID, r.NodeID = m.id("R", r)
	r.CreatedAt, r.PushedAt = m.now(), m.now()
	m.repos[key(full)] = r
	return r, nil
}

// Repo is the repository owner/name.
func (m *Model) Repo(full string) (*Repo, bool) {
	r, ok := m.repos[key(full)]
	return r, ok
}

// Repos are the repositories, by name.
func (m *Model) Repos() []*Repo {
	out := make([]*Repo, 0, len(m.repos))
	for _, r := range m.repos {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b *Repo) int { return strings.Compare(key(a.Full()), key(b.Full())) })
	return out
}

// Issue is the repository's issue of that number.
func (r *Repo) Issue(n int) (*Issue, bool) {
	for _, i := range r.Issues {
		if i.Number == n {
			return i, true
		}
	}
	return nil, false
}

// Pull is the repository's pull request of that number.
func (r *Repo) Pull(n int) (*Pull, bool) {
	for _, p := range r.Pulls {
		if p.Number == n {
			return p, true
		}
	}
	return nil, false
}

// Label is the repository's label of that name.
func (r *Repo) Label(name string) (*Label, bool) {
	for _, l := range r.Labels {
		if strings.EqualFold(l.Name, name) {
			return l, true
		}
	}
	return nil, false
}

// Protection is the protection of branch, if any.
func (r *Repo) Protection(branch string) (*Protection, bool) {
	for _, p := range r.Protections {
		if p.Branch == branch {
			return p, true
		}
	}
	return nil, false
}

// number takes the next issue and pull request number, or n when the
// scenario names one.
func (r *Repo) number(n int) (int, error) {
	if n == 0 {
		n = r.next
	}
	if _, taken := r.Issue(n); taken {
		return 0, fmt.Errorf("%s#%d is an issue already", r.Full(), n)
	}
	if _, taken := r.Pull(n); taken {
		return 0, fmt.Errorf("%s#%d is a pull request already", r.Full(), n)
	}
	if n >= r.next {
		r.next = n + 1
	}
	return n, nil
}

// EnsureLabel returns the repository's label of that name, created when
// new.
func (m *Model) EnsureLabel(r *Repo, name, color, description string) *Label {
	if l, ok := r.Label(name); ok {
		return l
	}
	l := &Label{Name: name, Color: cmpOr(color, "ededed"), Description: description}
	l.ID, l.NodeID = m.id("LA", l)
	r.Labels = append(r.Labels, l)
	return l
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// IssueSpec is what a new issue starts with.
type IssueSpec struct {
	Number    int
	Title     string
	Body      string
	Author    string
	Labels    []string
	Assignees []string
}

// AddIssue opens an issue.
func (m *Model) AddIssue(r *Repo, s IssueSpec) (*Issue, error) {
	n, err := r.number(s.Number)
	if err != nil {
		return nil, err
	}
	i := &Issue{Repo: r, Number: n, Title: cmpOr(s.Title, fmt.Sprintf("Issue %d", n)), Body: s.Body, State: stateOpen,
		Author: m.Account(cmpOr(s.Author, m.Viewer), "").Login, CreatedAt: m.now(), UpdatedAt: m.now()}
	i.ID, i.NodeID = m.id("I", i)
	for _, l := range s.Labels {
		i.Labels = append(i.Labels, m.EnsureLabel(r, l, "", "").Name)
	}
	for _, a := range s.Assignees {
		i.Assignees = append(i.Assignees, m.Account(a, "").Login)
	}
	r.Issues = append(r.Issues, i)
	return i, nil
}

// PullSpec is what a new pull request starts with.
type PullSpec struct {
	Number  int
	Title   string
	Body    string
	Author  string
	Base    string
	Head    string
	HeadSHA string
	Draft   bool
	Labels  []string
}

// AddPull opens a pull request of head into base.
func (m *Model) AddPull(r *Repo, s PullSpec) (*Pull, error) {
	if s.Head == "" {
		return nil, fmt.Errorf("a pull request of %s needs a head branch", r.Full())
	}
	base := cmpOr(s.Base, r.DefaultBranch)
	if s.Number == 0 {
		for _, p := range r.Pulls {
			if p.State == stateOpen && p.Head == s.Head && p.Base == base {
				return nil, &apiError{Status: 422, Message: "Validation Failed",
					Errors: []string{fmt.Sprintf("A pull request already exists for %s:%s.", r.Owner, s.Head)}}
			}
		}
	}
	n, err := r.number(s.Number)
	if err != nil {
		return nil, err
	}
	p := &Pull{Repo: r, Number: n, Title: cmpOr(s.Title, fmt.Sprintf("Pull request %d", n)), Body: s.Body, State: stateOpen, Draft: s.Draft,
		Author: m.Account(cmpOr(s.Author, m.Viewer), "").Login, Base: base, Head: s.Head, headSHA: s.HeadSHA,
		CreatedAt: m.now(), UpdatedAt: m.now()}
	p.ID, p.NodeID = m.id("PR", p)
	for _, l := range s.Labels {
		p.Labels = append(p.Labels, m.EnsureLabel(r, l, "", "").Name)
	}
	r.Pulls = append(r.Pulls, p)
	return p, nil
}

// AddComment comments on an issue or a pull request (exactly one is set).
func (m *Model) AddComment(i *Issue, p *Pull, author, body string) *Comment {
	c := &Comment{Author: m.Account(cmpOr(author, m.Viewer), "").Login, Body: body, CreatedAt: m.now(), UpdatedAt: m.now(), issue: i, pull: p}
	c.ID, c.NodeID = m.id("IC", c)
	if i != nil {
		i.Comments = append(i.Comments, c)
		i.UpdatedAt = m.now()
	} else {
		p.Comments = append(p.Comments, c)
		p.UpdatedAt = m.now()
	}
	return c
}

// URL is the comment's page.
func (c *Comment) URL() string {
	if c.issue != nil {
		return fmt.Sprintf("%s#issuecomment-%d", c.issue.URL(), c.ID)
	}
	return fmt.Sprintf("%s#issuecomment-%d", c.pull.URL(), c.ID)
}

// CloseIssue closes an issue for reason (COMPLETED when empty).
func (m *Model) CloseIssue(i *Issue, reason string) {
	if i.State == stateClosed {
		return
	}
	t := m.now()
	i.State, i.StateReason, i.ClosedAt, i.UpdatedAt = stateClosed, cmpOr(reason, "COMPLETED"), &t, t
}

// ReopenIssue reopens a closed issue.
func (m *Model) ReopenIssue(i *Issue) {
	if i.State == stateOpen {
		return
	}
	t := m.now()
	i.State, i.StateReason, i.ClosedAt, i.ClosedBy, i.UpdatedAt, i.Reopened = stateOpen, "REOPENED", nil, nil, t, &t
}

// ClosePull closes a pull request without merging it.
func (m *Model) ClosePull(p *Pull) {
	if p.State != stateOpen {
		return
	}
	p.headSHA = m.HeadSHA(p)
	t := m.now()
	p.State, p.ClosedAt, p.UpdatedAt = stateClosed, &t, t
}

// ReopenPull reopens a closed, unmerged pull request.
func (m *Model) ReopenPull(p *Pull) error {
	if p.State == stateMerged {
		return &apiError{Status: 422, Message: "Validation Failed", Errors: []string{"state cannot be changed. The pull request is merged."}}
	}
	p.State, p.ClosedAt, p.UpdatedAt = stateOpen, nil, m.now()
	return nil
}

// AddReview submits a review on the pull request's current head.
func (m *Model) AddReview(p *Pull, author, state, body, commit string) *Review {
	rv := &Review{Author: m.Account(cmpOr(author, m.Viewer), "").Login, State: state, Body: body, Commit: cmpOr(commit, m.HeadSHA(p)), SubmittedAt: m.now(), pull: p}
	rv.ID, rv.NodeID = m.id("PRR", rv)
	p.Reviews = append(p.Reviews, rv)
	p.UpdatedAt = m.now()
	return rv
}

// URL is the review's page.
func (rv *Review) URL() string {
	return fmt.Sprintf("%s#pullrequestreview-%d", rv.pull.URL(), rv.ID)
}

// LatestReviews is each reviewer's last review that decides: an approval
// or a request for changes, or their last comment when they gave neither.
func (p *Pull) LatestReviews() []*Review {
	var order []string
	last := map[string]*Review{}
	for _, rv := range p.Reviews {
		if rv.State == reviewPending {
			continue
		}
		old, seen := last[rv.Author]
		if !seen {
			order = append(order, rv.Author)
		}
		decides := rv.State == reviewApproved || rv.State == reviewChanges || rv.State == reviewDismissed
		if !seen || decides || old.State == reviewCommented {
			last[rv.Author] = rv
		}
	}
	out := make([]*Review, 0, len(order))
	for _, a := range order {
		out = append(out, last[a])
	}
	return out
}

// ReviewDecision is the declared decision, or the one the reviews and the
// base's protection give: CHANGES_REQUESTED, APPROVED, REVIEW_REQUIRED or
// "" where no review is required and none was given.
func (p *Pull) ReviewDecision() string {
	if p.Decision != "" {
		return p.Decision
	}
	approved := 0
	for _, rv := range p.LatestReviews() {
		switch rv.State {
		case reviewChanges:
			return reviewChanges
		case reviewApproved:
			approved++
		}
	}
	required := 0
	if prot, ok := p.Repo.Protection(p.Base); ok {
		required = prot.RequiredReviews
	}
	switch {
	case required > 0 && approved < required:
		return "REVIEW_REQUIRED"
	case approved > 0:
		return reviewApproved
	}
	return ""
}

// HeadSHA is the pull request's head: its branch's while it is open, the
// last one it had once it is merged or closed.
func (m *Model) HeadSHA(p *Pull) string {
	if p.State == stateOpen {
		if sha, ok := m.Branch(p.Repo, p.Head); ok {
			return sha
		}
	}
	return p.headSHA
}

// BaseSHA is the head of the pull request's base branch.
func (m *Model) BaseSHA(p *Pull) string {
	sha, _ := m.Branch(p.Repo, p.Base)
	return sha
}

// Branch is the head of a branch: the origin's when the repository has
// one, else the declared one.
func (m *Model) Branch(r *Repo, name string) (string, bool) {
	if m.Origins.Has(r) {
		return m.Origins.Resolve(r, "refs/heads/"+name)
	}
	sha, ok := r.Branches[name]
	return sha, ok
}

// BranchNames are the repository's branches, sorted.
func (m *Model) BranchNames(r *Repo) []string {
	if m.Origins.Has(r) {
		return m.Origins.Refs(r, "refs/heads/")
	}
	out := make([]string, 0, len(r.Branches))
	for b := range r.Branches {
		out = append(out, b)
	}
	slices.Sort(out)
	return out
}

// Tag is the commit of a tag.
func (m *Model) Tag(r *Repo, name string) (string, bool) {
	if m.Origins.Has(r) {
		return m.Origins.Resolve(r, "refs/tags/"+name)
	}
	sha, ok := r.Tags[name]
	return sha, ok
}

// TagNames are the repository's tags, sorted.
func (m *Model) TagNames(r *Repo) []string {
	if m.Origins.Has(r) {
		return m.Origins.Refs(r, "refs/tags/")
	}
	out := make([]string, 0, len(r.Tags))
	for t := range r.Tags {
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}

// Commit resolves a branch, a tag or a commit id to a commit id.
func (m *Model) Commit(r *Repo, ref string) (string, bool) {
	if ref == "HEAD" {
		ref = r.DefaultBranch
	}
	if m.Origins.Has(r) {
		if sha, ok := m.Origins.Resolve(r, "refs/heads/"+ref); ok {
			return sha, true
		}
		if sha, ok := m.Origins.Resolve(r, "refs/tags/"+ref); ok {
			return sha, true
		}
		return m.Origins.Resolve(r, ref)
	}
	if sha, ok := r.Branches[ref]; ok {
		return sha, true
	}
	if sha, ok := r.Tags[ref]; ok {
		return sha, true
	}
	if shaRE.MatchString(ref) {
		return ref, true
	}
	return "", false
}

var shaRE = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// closingRE finds the issues a pull request's body closes: a closing
// keyword before #n, owner/repo#n or an issue's URL.
var closingRE = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\b:?\s+(?:([\w.-]+/[\w.-]+)?#(\d+)|https://github\.com/([\w.-]+/[\w.-]+)/issues/(\d+))`)

// Closes are the issues the pull request closes when it merges into its
// repository's default branch: those the scenario declared and those its
// body names with a closing keyword.
func (m *Model) Closes(p *Pull) []*Issue {
	out := slices.Clone(p.closes)
	for _, match := range closingRE.FindAllStringSubmatch(p.Body, -1) {
		repo, num := match[1], match[2]
		if match[4] != "" {
			repo, num = match[3], match[4]
		}
		r := p.Repo
		if repo != "" {
			other, ok := m.Repo(repo)
			if !ok {
				continue
			}
			r = other
		}
		n, _ := strconv.Atoi(num)
		if i, ok := r.Issue(n); ok && !slices.Contains(out, i) {
			out = append(out, i)
		}
	}
	return out
}

// ClosedByPulls are the pull requests that close the issue.
func (m *Model) ClosedByPulls(i *Issue) []*Pull {
	var out []*Pull
	for _, r := range m.Repos() {
		for _, p := range r.Pulls {
			if slices.Contains(m.Closes(p), i) {
				out = append(out, p)
			}
		}
	}
	return out
}

// Contexts are the check runs and statuses of a commit, the latest of
// each name: what a pull request's status check rollup holds.
func (r *Repo) Contexts(sha string) ([]*CheckRun, []*Status) {
	var runs []*CheckRun
	seenRun := map[string]int{}
	for _, c := range r.Checks {
		if c.SHA != sha {
			continue
		}
		if at, ok := seenRun[c.Name]; ok {
			runs[at] = c
			continue
		}
		seenRun[c.Name] = len(runs)
		runs = append(runs, c)
	}
	var statuses []*Status
	seenStatus := map[string]int{}
	for _, s := range r.Statuses {
		if s.SHA != sha {
			continue
		}
		if at, ok := seenStatus[s.Context]; ok {
			statuses[at] = s
			continue
		}
		seenStatus[s.Context] = len(statuses)
		statuses = append(statuses, s)
	}
	return runs, statuses
}

// Rollup is a commit's rollup state: "" without any check, else PENDING,
// FAILURE or SUCCESS.
func (r *Repo) Rollup(sha string) string {
	runs, statuses := r.Contexts(sha)
	if len(runs)+len(statuses) == 0 {
		return ""
	}
	pending, failed := false, false
	for _, c := range runs {
		switch {
		case c.Status != checkCompleted:
			pending = true
		case c.Conclusion != conclusionSuccess && c.Conclusion != "NEUTRAL" && c.Conclusion != "SKIPPED":
			failed = true
		}
	}
	for _, s := range statuses {
		switch s.State {
		case statusPending:
			pending = true
		case statusFailure, statusError:
			failed = true
		}
	}
	switch {
	case failed:
		return statusFailure
	case pending:
		return statusPending
	}
	return statusSuccess
}

// MergeableState is the declared mergeability, MERGEABLE unless declared.
func (p *Pull) MergeableState() string {
	if p.State != stateOpen {
		return "UNKNOWN"
	}
	return cmpOr(p.Mergeable, "MERGEABLE")
}

// MergeStateStatus is the declared merge state, or the one the pull
// request's draft flag, conflicts, protection and checks give.
func (m *Model) MergeStateStatus(p *Pull) string {
	if p.MergeState != "" {
		return p.MergeState
	}
	if p.State != stateOpen {
		return "UNKNOWN"
	}
	head := m.HeadSHA(p)
	rollup := p.Repo.Rollup(head)
	prot, protected := p.Repo.Protection(p.Base)
	switch {
	case p.Draft:
		return "DRAFT"
	case p.MergeableState() == "CONFLICTING":
		return "DIRTY"
	case protected && prot.RequiredReviews > 0 && p.ReviewDecision() != reviewApproved:
		return "BLOCKED"
	case protected && !m.requiredGreen(p.Repo, prot, head):
		return "BLOCKED"
	case protected && prot.Strict && m.Behind(p) > 0:
		return "BEHIND"
	case rollup == statusFailure || rollup == statusPending:
		return "UNSTABLE"
	}
	return "CLEAN"
}

func (m *Model) requiredGreen(r *Repo, prot *Protection, sha string) bool {
	runs, statuses := r.Contexts(sha)
	for _, name := range prot.RequiredChecks {
		green := false
		for _, c := range runs {
			if c.Name == name && c.Status == checkCompleted && (c.Conclusion == conclusionSuccess || c.Conclusion == "NEUTRAL" || c.Conclusion == "SKIPPED") {
				green = true
			}
		}
		for _, s := range statuses {
			if s.Context == name && s.State == statusSuccess {
				green = true
			}
		}
		if !green {
			return false
		}
	}
	return true
}

// Behind is how many commits of the base the pull request's head lacks;
// 0 without an origin.
func (m *Model) Behind(p *Pull) int {
	if !m.Origins.Has(p.Repo) {
		return 0
	}
	_, behind, err := m.Origins.AheadBehind(p.Repo, m.BaseSHA(p), m.HeadSHA(p))
	if err != nil {
		return 0
	}
	return behind
}

// CheckSpec sets a check run on a commit.
type CheckSpec struct {
	Name       string
	Workflow   string
	Status     string // QUEUED, IN_PROGRESS or COMPLETED
	Conclusion string
	Branch     string
	Event      string
}

// SetCheck creates or updates the check run of that name on sha, and the
// workflow run it belongs to: a new attempt of the workflow's run on that
// commit when the check had completed.
func (m *Model) SetCheck(r *Repo, sha string, s CheckSpec) *CheckRun {
	wfName := cmpOr(s.Workflow, "CI")
	var wf *Workflow
	for _, w := range r.Workflows {
		if w.Name == wfName {
			wf = w
		}
	}
	if wf == nil {
		wf = &Workflow{Name: wfName, Path: ".github/workflows/" + strings.ToLower(strings.ReplaceAll(wfName, " ", "-")) + ".yaml"}
		wf.ID, _ = m.id("W", wf)
		r.Workflows = append(r.Workflows, wf)
	}
	var run *WorkflowRun
	for _, x := range r.Runs {
		if x.Workflow == wf && x.SHA == sha {
			run = x
		}
	}
	if run == nil {
		run = &WorkflowRun{Workflow: wf, SHA: sha, Branch: s.Branch, Event: cmpOr(s.Event, "pull_request"), Attempt: 1, CreatedAt: m.now(), repo: r}
		run.ID, _ = m.id("WFR", run)
		run.Number = len(r.Runs) + 1
		r.Runs = append(r.Runs, run)
	}
	var check *CheckRun
	for _, c := range r.Checks {
		if c.SHA == sha && c.Name == s.Name && c.Run == run {
			check = c
		}
	}
	if check == nil {
		check = &CheckRun{SHA: sha, Name: s.Name, StartedAt: m.now(), Run: run, repo: r}
		check.ID, check.NodeID = m.id("CR", check)
		r.Checks = append(r.Checks, check)
	}
	check.Status, check.Conclusion, check.CompletedAt = s.Status, "", nil
	if s.Status == checkCompleted {
		t := m.now()
		check.Conclusion, check.CompletedAt = cmpOr(s.Conclusion, conclusionSuccess), &t
	}
	m.settleRun(r, run)
	return check
}

// settleRun derives a workflow run's status from its check runs.
func (m *Model) settleRun(r *Repo, run *WorkflowRun) {
	status, conclusion := "completed", "success"
	for _, c := range r.Checks {
		if c.Run != run {
			continue
		}
		switch {
		case c.Status == checkQueued && status != "in_progress":
			status = "queued"
		case c.Status != checkCompleted:
			status = "in_progress"
		case c.Conclusion != conclusionSuccess && c.Conclusion != "NEUTRAL" && c.Conclusion != "SKIPPED":
			conclusion = strings.ToLower(c.Conclusion)
		}
	}
	run.Status, run.Conclusion, run.UpdatedAt = status, "", m.now()
	if status == "completed" {
		run.Conclusion = conclusion
	}
}

// URL is the run's page.
func (run *WorkflowRun) URL() string {
	return fmt.Sprintf("%s/actions/runs/%d", run.repo.URL(), run.ID)
}

// URL is the check run's page, a job of its workflow run.
func (c *CheckRun) URL() string {
	return fmt.Sprintf("%s/job/%d", c.Run.URL(), c.ID)
}

// SetStatus sets the commit status of a context on sha.
func (m *Model) SetStatus(r *Repo, sha, context, state, description, target string) *Status {
	s := &Status{SHA: sha, Context: context, State: strings.ToUpper(state), Description: description, TargetURL: target, CreatedAt: m.now()}
	s.ID, s.NodeID = m.id("SC", s)
	r.Statuses = append(r.Statuses, s)
	return s
}

// AddRelease adds a release of tag.
func (m *Model) AddRelease(r *Repo, rel Release) (*Release, error) {
	for _, old := range r.Releases {
		if old.Tag == rel.Tag {
			return nil, &apiError{Status: 422, Message: "Validation Failed", Errors: []string{"tag_name already_exists"}}
		}
	}
	out := &rel
	out.repo = r
	out.Author = m.Account(cmpOr(rel.Author, m.Viewer), "").Login
	out.Name = cmpOr(rel.Name, rel.Tag)
	out.Target = cmpOr(rel.Target, r.DefaultBranch)
	if out.CreatedAt.IsZero() {
		out.CreatedAt = m.now()
	}
	if !out.Draft && out.PublishedAt == nil {
		t := out.CreatedAt
		out.PublishedAt = &t
	}
	out.ID, out.NodeID = m.id("RE", out)
	r.Releases = append(r.Releases, out)
	return out, nil
}

// Release is the release of tag.
func (r *Repo) Release(tag string) (*Release, bool) {
	for _, rel := range r.Releases {
		if rel.Tag == tag {
			return rel, true
		}
	}
	return nil, false
}

// URL is the release's page.
func (rel *Release) URL() string {
	return fmt.Sprintf("%s/releases/tag/%s", rel.repo.URL(), rel.Tag)
}

// AddAsset attaches an asset to a release.
func (m *Model) AddAsset(rel *Release, name, contentType string, content []byte) *Asset {
	a := &Asset{Name: name, ContentType: cmpOr(contentType, "application/octet-stream"), Content: content, CreatedAt: m.now()}
	a.ID, a.NodeID = m.id("RA", a)
	rel.Assets = append(rel.Assets, a)
	return a
}

// AddProject adds a board with its Title field.
func (m *Model) AddProject(owner string, number int, title string) (*Project, error) {
	if _, ok := m.Project(owner, number); ok {
		return nil, fmt.Errorf("project %s/%d exists already", owner, number)
	}
	if _, known := m.accounts[key(owner)]; !known {
		m.Account(owner, kindOrg)
	}
	p := &Project{Owner: owner, Number: number, Title: cmpOr(title, fmt.Sprintf("Board %d", number))}
	p.ID, p.NodeID = m.id("PVT", p)
	m.projects = append(m.projects, p)
	m.AddField(p, "Title", "TITLE", nil)
	return p, nil
}

// Project is the board of owner and number.
func (m *Model) Project(owner string, number int) (*Project, bool) {
	for _, p := range m.projects {
		if strings.EqualFold(p.Owner, owner) && p.Number == number {
			return p, true
		}
	}
	return nil, false
}

// Projects are the boards.
func (m *Model) Projects() []*Project { return m.projects }

// AddField adds a field to a board, or the options a field lacks.
func (m *Model) AddField(p *Project, name, dataType string, options []string) *ProjectField {
	f := p.Field(name)
	if f == nil {
		f = &ProjectField{Name: name, DataType: dataType}
		prefix := "PVTF"
		if dataType == "SINGLE_SELECT" {
			prefix = "PVTSSF"
		}
		_, f.NodeID = m.id(prefix, f)
		p.Fields = append(p.Fields, f)
	}
	for _, o := range options {
		if _, ok := f.Option(o); !ok {
			m.seq++
			f.Options = append(f.Options, ProjectOption{ID: fmt.Sprintf("opt%d", m.seq), Name: o})
		}
	}
	return f
}

// AddItem puts an issue or a pull request on a board; the item it has
// already when it is there.
func (m *Model) AddItem(p *Project, i *Issue, pr *Pull) *ProjectItem {
	for _, it := range p.Items {
		if (i != nil && it.Issue == i) || (pr != nil && it.Pull == pr) {
			it.Archived = false
			return it
		}
	}
	it := &ProjectItem{Project: p, Issue: i, Pull: pr, Values: map[string]*FieldValue{}, CreatedAt: m.now(), UpdatedAt: m.now()}
	it.ID, it.NodeID = m.id("PVTI", it)
	p.Items = append(p.Items, it)
	return it
}

// SetItemOption sets an item's single-select field to the option of that
// name; an empty option clears the field.
func (m *Model) SetItemOption(it *ProjectItem, field, option string) error {
	f := it.Project.Field(field)
	if f == nil {
		return fmt.Errorf("board %s/%d has no field %q", it.Project.Owner, it.Project.Number, field)
	}
	if option == "" {
		delete(it.Values, key(f.Name))
		it.UpdatedAt = m.now()
		return nil
	}
	o, ok := f.Option(option)
	if !ok {
		return fmt.Errorf("field %q of board %s/%d has no option %q", f.Name, it.Project.Owner, it.Project.Number, option)
	}
	it.Values[key(f.Name)] = &FieldValue{Field: f, Option: o.Name, OptionID: o.ID, UpdatedAt: m.now()}
	it.UpdatedAt = m.now()
	return nil
}

// Value is the item's value of a field.
func (it *ProjectItem) Value(field string) (*FieldValue, bool) {
	v, ok := it.Values[key(field)]
	return v, ok
}

// Items are the board items of an issue or a pull request.
func (m *Model) Items(i *Issue, pr *Pull) []*ProjectItem {
	var out []*ProjectItem
	for _, p := range m.projects {
		for _, it := range p.Items {
			if (i != nil && it.Issue == i) || (pr != nil && it.Pull == pr) {
				out = append(out, it)
			}
		}
	}
	return out
}

// MergeSpec is how a pull request is merged.
type MergeSpec struct {
	Method string // merge, squash or rebase
	Title  string
	Body   string
	// SHA is the head the caller expects; empty for any.
	SHA string
	// Admin says an administrator merges: the base's protection does not
	// hold them unless it enforces administrators. Force lifts it always.
	Admin bool
	Force bool
	By    string
}

// Merge merges an open pull request into its base: it refuses a draft, a
// head other than the expected one and, without Admin, a pull request its
// base's protection blocks. The origin gets the merge commit; the issues
// the pull request closes are closed when the base is the default branch.
func (m *Model) Merge(p *Pull, s MergeSpec) error {
	notAllowed := func(format string, args ...any) error {
		return &apiError{Status: 405, Message: fmt.Sprintf(format, args...)}
	}
	if p.State == stateMerged {
		return notAllowed("Pull Request is not mergeable")
	}
	if p.State != stateOpen {
		return notAllowed("Pull Request is not mergeable")
	}
	if p.Draft {
		return notAllowed("Pull Request is still a draft")
	}
	head := m.HeadSHA(p)
	if s.SHA != "" && s.SHA != head {
		return &apiError{Status: 409, Message: "Head branch was modified. Review and try the merge again."}
	}
	if p.MergeableState() == "CONFLICTING" {
		return notAllowed("Pull Request is not mergeable")
	}
	if prot, ok := p.Repo.Protection(p.Base); ok && !s.Force && (!s.Admin || prot.EnforceAdmins) {
		if prot.RequiredReviews > 0 && p.ReviewDecision() != reviewApproved {
			return notAllowed("At least %d approving review is required by reviewers with write access.", prot.RequiredReviews)
		}
		if !m.requiredGreen(p.Repo, prot, head) {
			return notAllowed("Required status check %q is expected.", strings.Join(prot.RequiredChecks, ", "))
		}
		if prot.Strict && m.Behind(p) > 0 {
			return notAllowed("Head branch is out of date. Review and try the merge again.")
		}
	}
	method := cmpOr(strings.ToLower(s.Method), "merge")
	title := cmpOr(s.Title, fmt.Sprintf("%s (#%d)", p.Title, p.Number))
	if method == "merge" && s.Title == "" {
		title = fmt.Sprintf("Merge pull request #%d from %s/%s", p.Number, p.Repo.Owner, p.Head)
	}
	files, commits := m.PullFiles(p), m.PullCommits(p)
	if files == nil {
		files = []PullFile{}
	}
	commit := head
	if m.Origins.Has(p.Repo) {
		sha, err := m.Origins.Merge(p.Repo, p.Base, head, method, title, s.Body)
		if err != nil {
			return &apiError{Status: 405, Message: "Pull Request is not mergeable", cause: err}
		}
		commit = sha
	} else {
		p.Repo.Branches[p.Base] = head
	}
	t := m.now()
	p.Files, p.commits = files, commits
	p.headSHA, p.State, p.MergeCommit = head, stateMerged, commit
	p.MergedAt, p.ClosedAt, p.UpdatedAt, p.MergedBy = &t, &t, t, m.Account(cmpOr(s.By, m.Viewer), "").Login
	p.Repo.PushedAt = t
	if p.Base == p.Repo.DefaultBranch {
		for _, i := range m.Closes(p) {
			if i.State == stateOpen {
				m.CloseIssue(i, "COMPLETED")
				i.ClosedBy = p
			}
		}
	}
	return nil
}

// DeleteBranch removes a branch.
func (m *Model) DeleteBranch(r *Repo, name string) error {
	if m.Origins.Has(r) {
		return m.Origins.DeleteRef(r, "refs/heads/"+name)
	}
	if _, ok := r.Branches[name]; !ok {
		return &apiError{Status: 422, Message: "Reference does not exist"}
	}
	delete(r.Branches, name)
	return nil
}

// SetParent makes sub a sub-issue of parent; a nil parent removes it from
// the one it has.
func (m *Model) SetParent(sub, parent *Issue) {
	if old := sub.Parent; old != nil {
		old.Subs = slices.DeleteFunc(old.Subs, func(i *Issue) bool { return i == sub })
	}
	sub.Parent = parent
	if parent != nil && !slices.Contains(parent.Subs, sub) {
		parent.Subs = append(parent.Subs, sub)
	}
}

// UpdateBranch brings a pull request's head up to date with its base: a
// merge of the base into it, or with rebase its changes on top of the
// base. Without an origin the declared head stays as it is.
func (m *Model) UpdateBranch(p *Pull, expected string, rebase bool) error {
	if p.State != stateOpen {
		return &apiError{Status: 422, Message: "Validation Failed", Errors: []string{"pull request is not open"}}
	}
	if expected != "" && expected != m.HeadSHA(p) {
		return &apiError{Status: 422, Message: "expected head sha didn't match current head ref."}
	}
	if !m.Origins.Has(p.Repo) {
		return nil
	}
	if m.Behind(p) == 0 {
		return &apiError{Status: 422, Message: "There are no new commits on the base branch."}
	}
	if _, err := m.Origins.UpdateBranch(p.Repo, p.Base, p.Head, rebase); err != nil {
		return &apiError{Status: 422, Message: "merge conflict between base and head", cause: err}
	}
	p.UpdatedAt = m.now()
	return nil
}

// AddMember makes login a member of an organization.
func (m *Model) AddMember(org, login string) {
	m.Account(org, kindOrg)
	if a := m.Account(login, ""); !containsFold(m.members[key(org)], a.Login) {
		m.members[key(org)] = append(m.members[key(org)], a.Login)
	}
}

// Members are an organization's members: the viewer and those added.
func (m *Model) Members(org string) []string {
	out := []string{m.Viewer}
	for _, l := range m.members[key(org)] {
		if !containsFold(out, l) {
			out = append(out, l)
		}
	}
	return out
}

// AddProtection protects a branch.
func (m *Model) AddProtection(r *Repo, p Protection) *Protection {
	out := &p
	_, out.NodeID = m.id("BPR", out)
	r.Protections = append(r.Protections, out)
	return out
}
