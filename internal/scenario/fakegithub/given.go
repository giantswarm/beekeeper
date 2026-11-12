//go:build scenario

package fakegithub

import (
	"crypto/sha1" //nolint:gosec // a commit id's shape, not a secret
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Given is the GitHub a scenario starts with (given.github).
type Given struct {
	// Viewer is the login the gh CLI and every token act as (desk).
	Viewer        string         `yaml:"viewer"`
	Users         []GivenUser    `yaml:"users"`
	Organizations []GivenOrg     `yaml:"organizations"`
	Repositories  []GivenRepo    `yaml:"repositories"`
	Projects      []GivenProject `yaml:"projects"`
	Budgets       GivenBudgets   `yaml:"budgets"`
	// Costs declare what GraphQL operations cost; any other costs what
	// GitHub's formula gives it.
	Costs  []Cost  `yaml:"costs"`
	Faults []Fault `yaml:"faults"`
}

// GivenUser is an account: a user, or with type Bot or Organization one
// of those. An account a scenario only names (an author, a reviewer)
// needs no entry.
type GivenUser struct {
	Login string `yaml:"login"`
	Type  string `yaml:"type"`
	Name  string `yaml:"name"`
}

// GivenOrg is an organization's members beside the viewer.
type GivenOrg struct {
	Login   string   `yaml:"login"`
	Members []string `yaml:"members"`
}

// GivenRepo is a repository.
type GivenRepo struct {
	Name          string            `yaml:"name"`
	Private       bool              `yaml:"private"`
	Description   string            `yaml:"description"`
	DefaultBranch string            `yaml:"default_branch"`
	Labels        []GivenLabel      `yaml:"labels"`
	Issues        []GivenIssue      `yaml:"issues"`
	Pulls         []GivenPull       `yaml:"pulls"`
	Releases      []GivenRelease    `yaml:"releases"`
	Protection    []GivenProtection `yaml:"protection"`
}

// GivenLabel is a label.
type GivenLabel struct {
	Name        string `yaml:"name"`
	Color       string `yaml:"color"`
	Description string `yaml:"description"`
}

// IssueRef names an issue: a number in the same repository, or
// owner/name#number.
type IssueRef struct {
	Repo   string
	Number int
}

var issueRefRE = regexp.MustCompile(`^([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#([0-9]+)$`)

// UnmarshalYAML reads a number or owner/name#number.
func (r *IssueRef) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	if num, err := strconv.Atoi(s); err == nil && num > 0 {
		r.Number = num
		return nil
	}
	m := issueRefRE.FindStringSubmatch(s)
	if m == nil {
		return fmt.Errorf("line %d: %q is neither an issue number nor owner/name#number", n.Line, s)
	}
	r.Repo = m[1]
	r.Number, _ = strconv.Atoi(m[2])
	return nil
}

// GivenComment is a comment.
type GivenComment struct {
	Author string `yaml:"author"`
	Body   string `yaml:"body"`
}

// GivenIssue is an issue. Board puts it on the scenario's board with
// that Status; Fields sets the board's other single-select fields.
type GivenIssue struct {
	Number    int               `yaml:"number"`
	Title     string            `yaml:"title"`
	Body      string            `yaml:"body"`
	State     string            `yaml:"state"` // open (default) or closed
	Author    string            `yaml:"author"`
	Labels    []string          `yaml:"labels"`
	Assignees []string          `yaml:"assignees"`
	Parent    *IssueRef         `yaml:"parent"`
	BlockedBy []IssueRef        `yaml:"blocked_by"`
	Board     string            `yaml:"board"`
	Fields    map[string]string `yaml:"fields"`
	Comments  []GivenComment    `yaml:"comments"`
}

// GivenReview is a review: approved, changes_requested or commented.
type GivenReview struct {
	Author string `yaml:"author"`
	State  string `yaml:"state"`
	Body   string `yaml:"body"`
}

// GivenCheck is a check run on a pull request's head: pending, green or
// red.
type GivenCheck struct {
	Name     string `yaml:"name"`
	State    string `yaml:"state"`
	Workflow string `yaml:"workflow"`
}

// GivenFile is a changed file of a pull request whose repository has no
// origin.
type GivenFile struct {
	Path      string `yaml:"path"`
	Additions int    `yaml:"additions"`
	Deletions int    `yaml:"deletions"`
}

// GivenPull is a pull request of Head into Base (the default branch). In
// a repository with an origin a head branch that does not exist yet is
// created with one commit.
type GivenPull struct {
	Number         int               `yaml:"number"`
	Title          string            `yaml:"title"`
	Body           string            `yaml:"body"`
	State          string            `yaml:"state"` // open (default), closed or merged
	Draft          bool              `yaml:"draft"`
	Author         string            `yaml:"author"`
	Head           string            `yaml:"head"`
	Base           string            `yaml:"base"`
	Labels         []string          `yaml:"labels"`
	Closes         []IssueRef        `yaml:"closes"`
	Reviews        []GivenReview     `yaml:"reviews"`
	Checks         []GivenCheck      `yaml:"checks"`
	Mergeable      string            `yaml:"mergeable"`       // MERGEABLE, CONFLICTING or UNKNOWN
	MergeState     string            `yaml:"merge_state"`     // CLEAN, BLOCKED, BEHIND, DIRTY, UNSTABLE, …
	ReviewDecision string            `yaml:"review_decision"` // APPROVED, CHANGES_REQUESTED or REVIEW_REQUIRED
	Board          string            `yaml:"board"`
	Fields         map[string]string `yaml:"fields"`
	Comments       []GivenComment    `yaml:"comments"`
	Files          []GivenFile       `yaml:"files"`
}

// GivenAsset is a release asset.
type GivenAsset struct {
	Name        string `yaml:"name"`
	Content     string `yaml:"content"`
	ContentType string `yaml:"content_type"`
}

// GivenRelease is a release.
type GivenRelease struct {
	Tag        string       `yaml:"tag"`
	Name       string       `yaml:"name"`
	Body       string       `yaml:"body"`
	Draft      bool         `yaml:"draft"`
	Prerelease bool         `yaml:"prerelease"`
	Target     string       `yaml:"target"`
	Assets     []GivenAsset `yaml:"assets"`
}

// GivenProtection protects a branch.
type GivenProtection struct {
	Branch          string   `yaml:"branch"`
	RequiredReviews int      `yaml:"required_reviews"`
	RequiredChecks  []string `yaml:"required_checks"`
	Strict          bool     `yaml:"strict"`
	MergeQueue      bool     `yaml:"merge_queue"`
	EnforceAdmins   bool     `yaml:"enforce_admins"`
}

// GivenField is a single-select field of a board with its options, or
// with type TEXT a text field.
type GivenField struct {
	Name    string   `yaml:"name"`
	Type    string   `yaml:"type"`
	Options []string `yaml:"options"`
}

// GivenProject is a Projects v2 board. The first is the scenario's board:
// the one an issue's board and fields go to.
type GivenProject struct {
	Owner  string       `yaml:"owner"`
	Number int          `yaml:"number"`
	Title  string       `yaml:"title"`
	Fields []GivenField `yaml:"fields"`
}

// GivenBudget sets a rate limit bucket.
type GivenBudget struct {
	Limit     *int     `yaml:"limit"`
	Remaining *int     `yaml:"remaining"`
	ResetIn   Duration `yaml:"reset_in"`
}

func (b GivenBudget) spec() BudgetSpec {
	return BudgetSpec{Limit: b.Limit, Remaining: b.Remaining, ResetIn: b.ResetIn.Duration}
}

// GivenSecondary is the secondary rate limit: more than Max requests
// within Per are refused with a Retry-After of RetryAfter.
type GivenSecondary struct {
	Max        int      `yaml:"max"`
	Per        Duration `yaml:"per"`
	RetryAfter Duration `yaml:"retry_after"`
}

// GivenBudgets are the buckets a scenario sets; the others are GitHub's:
// 5000 an hour for core and GraphQL, 30 and 10 a minute for search and
// code search.
type GivenBudgets struct {
	Core       *GivenBudget    `yaml:"core"`
	GraphQL    *GivenBudget    `yaml:"graphql"`
	Search     *GivenBudget    `yaml:"search"`
	CodeSearch *GivenBudget    `yaml:"code_search"`
	Secondary  *GivenSecondary `yaml:"secondary"`
}

func (b GivenBudgets) each() map[string]*GivenBudget {
	return map[string]*GivenBudget{bucketCore: b.Core, bucketGraphQL: b.GraphQL, bucketSearch: b.Search, bucketCodeSearch: b.CodeSearch}
}

// Options are what the harness gives the scenario's GitHub.
type Options struct {
	// Clock is the scenario's clock; the system's when nil.
	Clock func() time.Time
	// Origins are the repositories' bare origins; nil for none.
	Origins *Origins
	// Log receives every call as a JSON line.
	Log io.Writer
	// BoardOwner and BoardNumber are the desk's board: the board a
	// scenario that declares no project gets for its issues' board.
	BoardOwner  string
	BoardNumber int
	// Repositories are the scenario's repositories beside given.github's.
	Repositories []string
}

// DefaultViewer is the login of a scenario that names none.
const DefaultViewer = "desk"

// defaultStatuses are the Status options of a board the scenario does
// not declare, before those its issues name.
var defaultStatuses = []string{"Todo", "Ready", "In Progress", "Blocked", "Done"}

const (
	statusField  = "Status"
	singleSelect = "SINGLE_SELECT"
)

// fakeSHA is a commit id for a head of a repository without an origin.
func fakeSHA(parts ...string) string {
	sum := sha1.Sum([]byte(strings.Join(parts, "\x00"))) //nolint:gosec // a commit id's shape, not a secret
	return hex.EncodeToString(sum[:])
}

// Validate checks given.github as far as it can without the origins: it
// builds the model once and drops it.
func (g *Given) Validate() error {
	_, err := New(g, Options{})
	return err
}

// RepositoryNames are the repositories given.github names.
func (g *Given) RepositoryNames() []string {
	out := make([]string, 0, len(g.Repositories))
	for _, r := range g.Repositories {
		out = append(out, r.Name)
	}
	return out
}

// New builds the scenario's GitHub from given.github.
func New(g *Given, o Options) (*Server, error) {
	if g == nil {
		g = &Given{}
	}
	m := NewModel(o.Clock, cmpOr(g.Viewer, DefaultViewer))
	m.Origins = o.Origins
	b := &builder{g: g, o: o, m: m}
	if err := b.build(); err != nil {
		return nil, err
	}
	s := NewServer(m, o.Log)
	for name, spec := range g.Budgets.each() {
		if spec == nil {
			continue
		}
		if err := s.budgets.set(name, spec.spec(), s.clock()); err != nil {
			return nil, fmt.Errorf("budgets: %w", err)
		}
	}
	if sec := g.Budgets.Secondary; sec != nil {
		if sec.Max <= 0 || sec.Per.Duration <= 0 {
			return nil, errors.New("budgets.secondary: max and per are required")
		}
		s.SetSecondary(sec.Max, sec.Per.Duration, sec.RetryAfter.Duration)
	}
	if err := s.SetCosts(slices.Clone(g.Costs)); err != nil {
		return nil, err
	}
	for i, f := range g.Faults {
		if err := s.Arm(f); err != nil {
			return nil, fmt.Errorf("faults[%d]: %w", i, err)
		}
	}
	return s, nil
}

type builder struct {
	g       *Given
	o       Options
	m       *Model
	project *Project
	errs    []error
}

func (b *builder) bad(format string, args ...any) {
	b.errs = append(b.errs, fmt.Errorf(format, args...))
}

func (b *builder) build() error {
	for _, u := range b.g.Users {
		if u.Login == "" {
			b.bad("users: an account without a login")
			continue
		}
		kind := cmpOr(u.Type, kindUser)
		if !slices.Contains([]string{kindUser, kindBot, kindOrg}, kind) {
			b.bad("users: %s has type %q, not User, Bot or Organization", u.Login, kind)
			continue
		}
		b.m.Account(u.Login, kind).Name = u.Name
	}
	for _, org := range b.g.Organizations {
		b.m.Account(org.Login, kindOrg)
		for _, member := range org.Members {
			b.m.AddMember(org.Login, member)
		}
	}
	for _, name := range b.o.Repositories {
		if _, err := b.m.AddRepo(name); err != nil {
			b.bad("%v", err)
		}
	}
	for _, p := range b.g.Projects {
		b.addProject(p)
	}
	for _, gr := range b.g.Repositories {
		r, err := b.m.AddRepo(gr.Name)
		if err != nil {
			b.bad("repositories: %v", err)
			continue
		}
		b.repo(r, gr)
	}
	// Links between issues need every issue: a second pass.
	for _, gr := range b.g.Repositories {
		r, ok := b.m.Repo(gr.Name)
		if !ok {
			continue
		}
		for _, gi := range gr.Issues {
			b.links(r, gi)
		}
		for _, gp := range gr.Pulls {
			b.pull(r, gp)
		}
	}
	return errors.Join(b.errs...)
}

func (b *builder) addProject(gp GivenProject) {
	if gp.Owner == "" || gp.Number <= 0 {
		b.bad("projects: a board needs an owner and a number")
		return
	}
	p, err := b.m.AddProject(gp.Owner, gp.Number, gp.Title)
	if err != nil {
		b.bad("projects: %v", err)
		return
	}
	if b.project == nil {
		b.project = p
	}
	hasStatus := false
	for _, f := range gp.Fields {
		kind := cmpOr(strings.ToUpper(f.Type), singleSelect)
		if kind != singleSelect && kind != "TEXT" {
			b.bad("projects: field %s of %s/%d has type %q, not SINGLE_SELECT or TEXT", f.Name, gp.Owner, gp.Number, f.Type)
			continue
		}
		b.m.AddField(p, f.Name, kind, f.Options)
		hasStatus = hasStatus || strings.EqualFold(f.Name, statusField)
	}
	if !hasStatus {
		b.m.AddField(p, statusField, singleSelect, defaultStatuses)
	}
}

// board is the scenario's board: the first declared, or the desk's with
// the usual statuses, which then takes any option an item names.
func (b *builder) board() (*Project, bool) {
	if b.project != nil {
		return b.project, len(b.g.Projects) == 0
	}
	owner, number := cmpOr(b.o.BoardOwner, "example"), b.o.BoardNumber
	if number <= 0 {
		number = 1
	}
	p, err := b.m.AddProject(owner, number, "")
	if err != nil {
		b.bad("%v", err)
		return nil, false
	}
	b.m.AddField(p, statusField, singleSelect, defaultStatuses)
	b.project = p
	return p, true
}

// place puts an issue or a pull request on the scenario's board.
func (b *builder) place(where string, i *Issue, pr *Pull, status string, fields map[string]string) {
	if status == "" && len(fields) == 0 {
		return
	}
	p, open := b.board()
	if p == nil {
		return
	}
	it := b.m.AddItem(p, i, pr)
	values := map[string]string{}
	for k, v := range fields {
		values[k] = v
	}
	if status != "" {
		values[statusField] = status
	}
	for field, option := range values {
		if open {
			b.m.AddField(p, field, singleSelect, []string{option})
		}
		if err := b.m.SetItemOption(it, field, option); err != nil {
			b.bad("%s: %v", where, err)
		}
	}
}

func (b *builder) repo(r *Repo, gr GivenRepo) {
	r.Private, r.Description = gr.Private, gr.Description
	if gr.DefaultBranch != "" {
		r.DefaultBranch = gr.DefaultBranch
	}
	for _, l := range gr.Labels {
		b.m.EnsureLabel(r, l.Name, l.Color, l.Description)
	}
	if !b.m.Origins.Has(r) {
		r.Branches[r.DefaultBranch] = fakeSHA(r.Full(), r.DefaultBranch)
	}
	for _, gp := range gr.Protection {
		b.m.AddProtection(r, Protection{Branch: cmpOr(gp.Branch, r.DefaultBranch), RequiredReviews: gp.RequiredReviews,
			RequiredChecks: gp.RequiredChecks, Strict: gp.Strict, MergeQueue: gp.MergeQueue, EnforceAdmins: gp.EnforceAdmins})
	}
	for _, gi := range gr.Issues {
		where := fmt.Sprintf("repositories: %s issue %d", r.Full(), gi.Number)
		i, err := b.m.AddIssue(r, IssueSpec{Number: gi.Number, Title: gi.Title, Body: gi.Body, Author: gi.Author, Labels: gi.Labels, Assignees: gi.Assignees})
		if err != nil {
			b.bad("%s: %v", where, err)
			continue
		}
		for _, c := range gi.Comments {
			b.m.AddComment(i, nil, c.Author, c.Body)
		}
		switch strings.ToLower(gi.State) {
		case "", "open":
		case "closed":
			b.m.CloseIssue(i, "")
		default:
			b.bad("%s: state %q is neither open nor closed", where, gi.State)
		}
		b.place(where, i, nil, gi.Board, gi.Fields)
	}
	for _, gl := range gr.Releases {
		if gl.Tag == "" {
			b.bad("repositories: a release of %s without a tag", r.Full())
			continue
		}
		rel, err := b.m.AddRelease(r, Release{Tag: gl.Tag, Name: gl.Name, Body: gl.Body, Draft: gl.Draft, Prerelease: gl.Prerelease, Target: gl.Target})
		if err != nil {
			b.bad("repositories: %s release %s: %v", r.Full(), gl.Tag, err)
			continue
		}
		for _, a := range gl.Assets {
			b.m.AddAsset(rel, a.Name, a.ContentType, []byte(a.Content))
		}
		if _, tagged := b.m.Tag(r, gl.Tag); !tagged && !gl.Draft {
			if sha, ok := b.m.Commit(r, rel.Target); ok {
				if err := b.m.SetRef(r, "refs/tags/"+gl.Tag, sha); err != nil {
					b.bad("repositories: %s release %s: %v", r.Full(), gl.Tag, err)
				}
			}
		}
	}
}

// issue resolves a reference from a repository.
func (b *builder) issue(r *Repo, ref IssueRef) (*Issue, error) {
	target := r
	if ref.Repo != "" {
		other, ok := b.m.Repo(ref.Repo)
		if !ok {
			return nil, fmt.Errorf("%s is no repository of the scenario", ref.Repo)
		}
		target = other
	}
	i, ok := target.Issue(ref.Number)
	if !ok {
		return nil, fmt.Errorf("%s#%d is no issue of the scenario", target.Full(), ref.Number)
	}
	return i, nil
}

func (b *builder) links(r *Repo, gi GivenIssue) {
	i, ok := r.Issue(gi.Number)
	if !ok {
		return
	}
	if gi.Parent != nil {
		parent, err := b.issue(r, *gi.Parent)
		if err != nil {
			b.bad("repositories: %s parent: %v", i.Ref(), err)
		} else {
			b.m.SetParent(i, parent)
		}
	}
	for _, ref := range gi.BlockedBy {
		blocker, err := b.issue(r, ref)
		if err != nil {
			b.bad("repositories: %s blocked_by: %v", i.Ref(), err)
			continue
		}
		i.BlockedBy = append(i.BlockedBy, blocker)
	}
}

// checkStates are a check's states in a scenario and what they set.
var checkStates = map[string][2]string{
	"queued":  {checkQueued, ""},
	"pending": {checkInProgress, ""},
	"green":   {checkCompleted, conclusionSuccess},
	"red":     {checkCompleted, conclusionFailure},
}

// reviewStates are a review's states in a scenario.
var reviewStates = map[string]string{"approved": reviewApproved, "changes_requested": reviewChanges, "commented": reviewCommented}

func (b *builder) pull(r *Repo, gp GivenPull) {
	where := fmt.Sprintf("repositories: %s pull %d", r.Full(), gp.Number)
	if gp.Head == "" {
		b.bad("%s: a pull request needs a head branch", where)
		return
	}
	base := cmpOr(gp.Base, r.DefaultBranch)
	title := cmpOr(gp.Title, "Pull request of "+gp.Head)
	sha := ""
	if b.m.Origins.Has(r) {
		if _, exists := b.m.Branch(r, gp.Head); !exists {
			if _, err := b.m.Origins.Push(r, gp.Head, base, ".scenario/"+gp.Head, title+"\n", title); err != nil {
				b.bad("%s: %v", where, err)
				return
			}
		}
	} else {
		sha = fakeSHA(r.Full(), gp.Head)
		r.Branches[gp.Head] = sha
		if _, ok := r.Branches[base]; !ok {
			r.Branches[base] = fakeSHA(r.Full(), base)
		}
	}
	p, err := b.m.AddPull(r, PullSpec{Number: gp.Number, Title: title, Body: gp.Body, Author: gp.Author, Base: base, Head: gp.Head, HeadSHA: sha, Draft: gp.Draft, Labels: gp.Labels})
	if err != nil {
		b.bad("%s: %v", where, err)
		return
	}
	p.Mergeable, p.MergeState, p.Decision = strings.ToUpper(gp.Mergeable), strings.ToUpper(gp.MergeState), strings.ToUpper(gp.ReviewDecision)
	for _, f := range gp.Files {
		p.Files = append(p.Files, PullFile{Path: f.Path, Additions: f.Additions, Deletions: f.Deletions})
	}
	for _, ref := range gp.Closes {
		i, err := b.issue(r, ref)
		if err != nil {
			b.bad("%s closes: %v", where, err)
			continue
		}
		p.closes = append(p.closes, i)
	}
	for _, c := range gp.Comments {
		b.m.AddComment(nil, p, c.Author, c.Body)
	}
	for _, rv := range gp.Reviews {
		state, ok := reviewStates[strings.ToLower(rv.State)]
		if !ok {
			b.bad("%s: review state %q is none of approved, changes_requested, commented", where, rv.State)
			continue
		}
		b.m.AddReview(p, cmpOr(rv.Author, "reviewer"), state, rv.Body, "")
	}
	for _, c := range gp.Checks {
		st, ok := checkStates[strings.ToLower(c.State)]
		if !ok || c.Name == "" {
			b.bad("%s: a check needs a name and a state of queued, pending, green or red (has %q, %q)", where, c.Name, c.State)
			continue
		}
		b.m.SetCheck(r, b.m.HeadSHA(p), CheckSpec{Name: c.Name, Workflow: c.Workflow, Status: st[0], Conclusion: st[1], Branch: p.Head})
	}
	b.place(where, nil, p, gp.Board, gp.Fields)
	switch strings.ToLower(gp.State) {
	case "", "open":
	case "closed":
		b.m.ClosePull(p)
	case "merged":
		if err := b.m.Merge(p, MergeSpec{Method: "squash", Force: true, By: gp.Author}); err != nil {
			b.bad("%s: %v", where, err)
		}
	default:
		b.bad("%s: state %q is none of open, closed, merged", where, gp.State)
	}
}

// SetRef points a branch or a tag at a commit.
func (m *Model) SetRef(r *Repo, ref, sha string) error {
	full := "refs/" + strings.TrimPrefix(ref, "refs/")
	if m.Origins.Has(r) {
		return m.Origins.SetRef(r, full, sha)
	}
	switch {
	case strings.HasPrefix(full, "refs/heads/"):
		r.Branches[strings.TrimPrefix(full, "refs/heads/")] = sha
	case strings.HasPrefix(full, "refs/tags/"):
		r.Tags[strings.TrimPrefix(full, "refs/tags/")] = sha
	default:
		return &apiError{Status: 422, Message: "Reference name must start with refs/heads/ or refs/tags/"}
	}
	return nil
}
