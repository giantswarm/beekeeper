package fakegithub

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // G505: derives stable fake option ids, not a security use
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Board options of the roadmap board (research: board 273). Teams come from the seed.
var (
	defaultStatuses = []string{
		"Inbox 📥", "Backlog 📦", "Up Next ➡️", "In Progress ⛏️", "Blocked / Waiting ⛔️",
		"Validation ☑️", "Done ✅", "Recurring ⥁",
	}
	defaultKinds = []string{
		"Rock 🪨", "Epic 🎯", "Feature 💫", "Story 📑", "Task 🗒️", "Note 📜", "Discovery 👀",
		"Postmortem 🚧", "Experiment 🥼", "Insight 💬", "Request 🛎️", "Bug 🐞", "Operational ⚙️",
		"Alert 🚨", "Working Hour ⚒", "Coffee ☕", "Recurring ⥁", "Reminder 🎗", "Opportunity ⭐",
	}
)

// Board field names.
// States of issues and pull requests (REST spells them in lower case), and the reason of an issue
// closed as done.
const (
	stateOpen       = "open"
	stateClosed     = "closed"
	reasonCompleted = "completed"
)

// Statuses of check runs.
const (
	checkQueued    = "queued"
	checkCompleted = "completed"
)

// States of commit statuses.
const (
	statusPending = "pending"
	statusSuccess = "success"
	statusFailure = "failure"
	statusError   = "error"
)

// visibilityPublic is a repository's visibility unless it is private or internal.
const visibilityPublic = "public"

// ghostLogin is the login GitHub shows for an actor it doesn't name.
const ghostLogin = "ghost"

const (
	fieldStatus   = "Status"
	fieldTeam     = "Team"
	fieldKind     = "Kind"
	fieldPriority = "Priority"
)

type repo struct {
	fullName          string
	id                int64
	nodeID            string
	private           bool
	visibility        string
	installed         bool
	defaultBranch     string
	branchHead        string
	ownerTeam         string
	requiredChecks    []string
	requiredApprovals int
	archived          bool
}

func (r *repo) owner() string { o, _, _ := strings.Cut(r.fullName, "/"); return o }
func (r *repo) name() string  { _, n, _ := strings.Cut(r.fullName, "/"); return n }

type boardItem struct {
	id        int64
	nodeID    string
	issue     string
	status    string
	team      string
	kind      string
	priority  *float64
	statusAt  time.Time
	createdAt time.Time
	updatedAt time.Time
	archived  bool
	archAt    time.Time
}

type issue struct {
	ref         string
	repo        string
	number      int
	id          int64
	nodeID      string
	title       string
	body        string
	state       string // open, closed
	stateReason string // completed, not_planned, reopened
	author      string
	assignees   []string
	labels      []string
	createdAt   time.Time
	updatedAt   time.Time
	closedAt    *time.Time
	parent      string
	subIssues   []string
	blockedBy   []string
	item        *boardItem
	// stub issues were only linked to (a parent, a blocker, a PR's closes); creating one fills it.
	stub bool
}

type review struct {
	id          int64
	by          string
	state       string // APPROVED, CHANGES_REQUESTED, COMMENTED
	commit      string
	submittedAt time.Time
}

type pull struct {
	ref       string
	repo      string
	number    int
	id        int64
	nodeID    string
	title     string
	author    string
	head      string
	base      string
	state     string // open, closed
	merged    bool
	draft     bool
	mergeSHA  string
	mergedBy  string
	mergedAt  *time.Time
	closedAt  *time.Time
	createdAt time.Time
	updatedAt time.Time
	closes    []string
	reviews   []*review
}

type checkRun struct {
	id          int64
	name        string
	sha         string
	status      string
	conclusion  string
	startedAt   *time.Time
	completedAt *time.Time
}

type commitStatus struct {
	id        int64
	context   string
	state     string
	createdAt time.Time
}

// state is GitHub as the fake models it.
type state struct {
	org            string
	appID          int64
	installationID int64
	secret         string
	project        int
	statuses       []string
	teams          []string
	kinds          []string
	members        map[string][]string // login → teams

	repos  map[string]*repo
	issues map[string]*issue
	pulls  map[string]*pull
	items  []*boardItem // board order; deleted items are removed
	checks map[string][]*checkRun
	stats  map[string][]*commitStatus
	seq    int64
}

func newState() *state {
	return &state{
		org:      "giantswarm",
		project:  273,
		statuses: append([]string(nil), defaultStatuses...),
		kinds:    append([]string(nil), defaultKinds...),
		members:  map[string][]string{},
		repos:    map[string]*repo{},
		issues:   map[string]*issue{},
		pulls:    map[string]*pull{},
		checks:   map[string][]*checkRun{},
		stats:    map[string][]*commitStatus{},
	}
}

func (s *state) nextID() int64 { s.seq++; return s.seq }

// stableID derives a numeric id from a key (so ids don't depend on creation order).
func stableID(kind, key string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(kind + ":" + key))
	return int64(h.Sum64()&(1<<53-1)) | 1
}

func optionID(field, name string) string {
	sum := sha1.Sum([]byte(field + "/" + name)) //nolint:gosec // G401: a stable fake option id, not a security use
	return hex.EncodeToString(sum[:4])
}

func (s *state) projectNodeID() string { return fmt.Sprintf("PVT_%s_%d", s.org, s.project) }
func (s *state) itemNodeID(ref string) string {
	return fmt.Sprintf("PVTI_%s_%d_%s", s.org, s.project, ref)
}
func (s *state) fieldNodeID(field string) string {
	if field == fieldPriority {
		return fmt.Sprintf("PVTF_%s_%d_%s", s.org, s.project, field)
	}
	return fmt.Sprintf("PVTSSF_%s_%d_%s", s.org, s.project, field)
}
func (s *state) options(field string) []string {
	switch field {
	case fieldStatus:
		return s.statuses
	case fieldTeam:
		return s.teams
	case fieldKind:
		return s.kinds
	}
	return nil
}

func splitRef(ref string) (repo string, number int, err error) {
	repo, n, ok := strings.Cut(ref, "#")
	if !ok || !strings.Contains(repo, "/") {
		return "", 0, fmt.Errorf("invalid ref %q (want owner/repo#n)", ref)
	}
	number, err = strconv.Atoi(n)
	if err != nil || number <= 0 {
		return "", 0, fmt.Errorf("invalid ref %q (want owner/repo#n)", ref)
	}
	return repo, number, nil
}

func checkRepoName(name string) error {
	o, n, ok := strings.Cut(name, "/")
	if !ok || o == "" || n == "" || strings.Contains(n, "/") {
		return fmt.Errorf("invalid repo %q (want owner/repo)", name)
	}
	return nil
}

// ensureRepo returns a repo, creating an unknown one: public, installed when the org owns it.
func (s *state) ensureRepo(name string) (*repo, error) {
	if r, ok := s.repos[name]; ok {
		return r, nil
	}
	if err := checkRepoName(name); err != nil {
		return nil, err
	}
	r := &repo{
		fullName:      name,
		id:            stableID("repo", name),
		nodeID:        "R_" + name,
		visibility:    visibilityPublic,
		installed:     strings.HasPrefix(name, s.org+"/"),
		defaultBranch: "main",
	}
	s.repos[name] = r
	return r, nil
}

// visible reports whether the installation can read a repo: a private repo outside it answers 404.
func (s *state) visible(name string) bool {
	r, ok := s.repos[name]
	return ok && (!r.private || r.installed)
}

// ensureIssue returns an issue, creating a stub for a ref that is only linked to.
func (s *state) ensureIssue(ref string, now time.Time) (*issue, error) {
	if is, ok := s.issues[ref]; ok {
		return is, nil
	}
	is, err := s.newIssue(ref, now)
	if err == nil {
		is.stub = true
	}
	return is, err
}

func (s *state) newIssue(ref string, now time.Time) (*issue, error) {
	repoName, n, err := splitRef(ref)
	if err != nil {
		return nil, err
	}
	if _, ok := s.pulls[ref]; ok {
		return nil, fmt.Errorf("%s is a pull request", ref)
	}
	if _, err := s.ensureRepo(repoName); err != nil {
		return nil, err
	}
	is := &issue{
		ref: ref, repo: repoName, number: n, id: stableID("issue", ref), nodeID: "I_" + ref,
		title: ref, state: stateOpen, author: ghostLogin, createdAt: now, updatedAt: now,
	}
	s.issues[ref] = is
	return is, nil
}

func (s *state) itemByNode(nodeID string) *boardItem {
	for _, it := range s.items {
		if it.nodeID == nodeID {
			return it
		}
	}
	return nil
}

func (s *state) removeItem(it *boardItem) {
	for i, x := range s.items {
		if x == it {
			s.items = append(s.items[:i], s.items[i+1:]...)
			return
		}
	}
}

// closingPulls are the PRs that name ref in closes, oldest first.
func (s *state) closingPulls(ref string) []*pull {
	var out []*pull
	for _, p := range s.pulls {
		for _, c := range p.closes {
			if c == ref {
				out = append(out, p)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].createdAt.Equal(out[j].createdAt) {
			return out[i].createdAt.Before(out[j].createdAt)
		}
		return out[i].ref < out[j].ref
	})
	return out
}

// openPullsWithHead are the open PRs of a repo whose head is sha.
func (s *state) openPullsWithHead(repoName, sha string) []*pull {
	var out []*pull
	for _, p := range s.pulls {
		if p.repo == repoName && p.head == sha && p.state == stateOpen {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].number < out[j].number })
	return out
}

// checkOption validates a board option; unknown kinds are added (the board's Kind list evolves),
// unknown statuses and teams are an error (a typo would otherwise look like "nothing matches").
func (s *state) checkOption(field, value string) error {
	if value == "" {
		return nil
	}
	for _, o := range s.options(field) {
		if o == value {
			return nil
		}
	}
	if field == fieldKind {
		s.kinds = append(s.kinds, value)
		return nil
	}
	return fmt.Errorf("%q is not an option of the board's %s field", value, field)
}

// ---- seeding ----

type seedDoc struct {
	App       *seedApp             `yaml:"app"`
	Board     *seedBoard           `yaml:"board"`
	Org       *seedOrg             `yaml:"org"`
	Repos     []seedRepo           `yaml:"repos"`
	Issues    []seedIssue          `yaml:"issues"`
	Pulls     []seedPull           `yaml:"pulls"`
	RateLimit map[string]seedLimit `yaml:"rateLimit"`
}

type seedApp struct {
	ID            int64  `yaml:"id"`
	Installation  int64  `yaml:"installation"`
	WebhookSecret string `yaml:"webhookSecret"`
}

type seedBoard struct {
	Project  int      `yaml:"project"`
	Teams    []string `yaml:"teams"`
	Statuses []string `yaml:"statuses"`
	Kinds    []string `yaml:"kinds"`
}

type seedOrg struct {
	Login   string       `yaml:"login"`
	Members []seedMember `yaml:"members"`
}

type seedMember struct {
	Login string   `yaml:"login"`
	Teams []string `yaml:"teams"`
}

type seedRepo struct {
	Name              string   `yaml:"name"`
	Visibility        string   `yaml:"visibility"`
	Installed         *bool    `yaml:"installed"`
	DefaultBranch     string   `yaml:"defaultBranch"`
	Owner             string   `yaml:"owner"`
	RequiredChecks    []string `yaml:"requiredChecks"`
	RequiredApprovals int      `yaml:"requiredApprovals"`
	Archived          bool     `yaml:"archived"`
}

type seedBoardFields struct {
	Status   string   `yaml:"status"`
	Kind     string   `yaml:"kind"`
	Team     string   `yaml:"team"`
	Priority *float64 `yaml:"priority"`
	Archived bool     `yaml:"archived"`
}

type seedIssue struct {
	Ref       string           `yaml:"ref"`
	Title     string           `yaml:"title"`
	Body      string           `yaml:"body"`
	Author    string           `yaml:"author"`
	Assignees []string         `yaml:"assignees"`
	Labels    []string         `yaml:"labels"`
	State     string           `yaml:"state"`
	CreatedAt string           `yaml:"createdAt"`
	Parent    string           `yaml:"parent"`
	BlockedBy []string         `yaml:"blockedBy"`
	Board     *seedBoardFields `yaml:"board"`
}

type seedCheck struct {
	Name       string `yaml:"name"`
	Status     string `yaml:"status"`
	Conclusion string `yaml:"conclusion"`
}

type seedStatus struct {
	Context string `yaml:"context"`
	State   string `yaml:"state"`
}

type seedReview struct {
	By    string `yaml:"by"`
	State string `yaml:"state"`
}

type seedPull struct {
	Ref       string       `yaml:"ref"`
	Title     string       `yaml:"title"`
	Author    string       `yaml:"author"`
	Head      string       `yaml:"head"`
	Base      string       `yaml:"base"`
	State     string       `yaml:"state"`
	MergeSha  string       `yaml:"mergeSha"`
	Draft     bool         `yaml:"draft"`
	CreatedAt string       `yaml:"createdAt"`
	Closes    []string     `yaml:"closes"`
	Checks    []seedCheck  `yaml:"checks"`
	Statuses  []seedStatus `yaml:"statuses"`
	Reviews   []seedReview `yaml:"reviews"`
}

type seedLimit struct {
	Limit     int    `yaml:"limit"`
	Remaining *int   `yaml:"remaining"`
	ResetIn   string `yaml:"resetIn"`
}

// decodeStrict re-encodes v as YAML and decodes it into out, failing on unknown keys.
func decodeStrict(v any, out any) error {
	if v == nil {
		return nil
	}
	b, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		if err.Error() == "EOF" {
			return nil
		}
		return err
	}
	return nil
}

func parseTime(s string, def time.Time) (time.Time, error) {
	if s == "" {
		return def, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid time %q (want RFC 3339)", s)
	}
	return t.UTC(), nil
}

func parseIssueState(s string) (state, reason string, err error) {
	switch s {
	case "", stateOpen:
		return stateOpen, "", nil
	case stateClosed, "closed_completed":
		return stateClosed, reasonCompleted, nil
	case "closed_not_planned":
		return stateClosed, "not_planned", nil
	}
	return "", "", fmt.Errorf("invalid issue state %q (want open, closed or closed_not_planned)", s)
}

func normCheckStatus(status, conclusion string) (string, string, error) {
	switch status {
	case "":
		status = checkQueued
		if conclusion != "" {
			status = checkCompleted
		}
	case checkQueued, "in_progress", checkCompleted, "pending", "waiting", "requested":
	default:
		return "", "", fmt.Errorf("invalid check run status %q", status)
	}
	if status != checkCompleted {
		conclusion = ""
	} else if conclusion == "" {
		return "", "", fmt.Errorf("a completed check run needs a conclusion")
	}
	return status, conclusion, nil
}

func normStatusState(s string) (string, error) {
	switch s {
	case statusPending, statusSuccess, statusFailure, statusError:
		return s, nil
	}
	return "", fmt.Errorf("invalid commit status state %q", s)
}

func normReviewState(s string) (string, error) {
	switch strings.ToLower(s) {
	case "approved":
		return "APPROVED", nil
	case "changes_requested":
		return "CHANGES_REQUESTED", nil
	case "commented":
		return "COMMENTED", nil
	case "dismissed":
		return "DISMISSED", nil
	}
	return "", fmt.Errorf("invalid review state %q", s)
}
