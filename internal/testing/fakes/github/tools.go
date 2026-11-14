package fakegithub

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/beekeeper/internal/forge"
	"github.com/giantswarm/beekeeper/internal/forge/github/wire"
)

// deliverArg is a step's deliver argument: delivered (default), lost, never, duplicate, or
// { delay: 10m }.
type deliverArg struct {
	mode  string
	delay time.Duration
}

func (d deliverArg) String() string {
	if d.mode == deliverDelay {
		return "delay " + d.delay.String()
	}
	if d.mode == "" {
		return deliverDelivered
	}
	return d.mode
}

// UnmarshalYAML accepts a mode name or a { delay } mapping.
func (d *deliverArg) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		switch n.Value {
		case deliverDelivered, deliverLost, deliverNever, deliverDuplicate:
			d.mode = n.Value
			return nil
		}
		return fmt.Errorf("deliver: %q is not one of delivered, lost, never, duplicate or { delay }", n.Value)
	case yaml.MappingNode:
		if len(n.Content) != 2 || n.Content[0].Value != deliverDelay {
			return fmt.Errorf("deliver: a mapping takes exactly one key, delay")
		}
		dur, err := time.ParseDuration(n.Content[1].Value)
		if err != nil || dur <= 0 {
			return fmt.Errorf("deliver: invalid delay %q", n.Content[1].Value)
		}
		d.mode, d.delay = deliverDelay, dur
		return nil
	}
	return fmt.Errorf("deliver: want a mode or { delay }")
}

// Step tools the fake implements.
const (
	toolIssueCreate       = "github_issue_create"
	toolIssueEdit         = "github_issue_edit"
	toolBoardSet          = "github_board_set"
	toolBoardAdd          = "github_board_add"
	toolBoardArchive      = "github_board_archive"
	toolBoardRemove       = "github_board_remove"
	toolSubIssue          = "github_sub_issue"
	toolDependency        = "github_dependency"
	toolPROpen            = "github_pr_open"
	toolPRPush            = "github_pr_push"
	toolPRReview          = "github_pr_review"
	toolPRMerge           = "github_pr_merge"
	toolPRClose           = "github_pr_close"
	toolCheckRun          = "github_check_run"
	toolStatus            = "github_status"
	toolInstallationRepos = "github_installation_repos"
	toolFault             = "github_fault"
	toolCalls             = "github_calls"
)

// Arguments of the step tools.
const (
	argRef        = "ref"
	argTitle      = "title"
	argBody       = "body"
	argAuthor     = "author"
	argAssignees  = "assignees"
	argLabels     = "labels"
	argCreatedAt  = "createdAt"
	argBoard      = "board"
	argDeliver    = "deliver"
	argState      = "state"
	argIssue      = "issue"
	argStatus     = "status"
	argKind       = "kind"
	argTeam       = "team"
	argPriority   = "priority"
	argParent     = "parent"
	argChild      = "child"
	argOp         = "op"
	argBlockedBy  = "blocked_by"
	argHead       = "head"
	argCloses     = "closes"
	argDraft      = "draft"
	argBase       = "base"
	argBy         = "by"
	argMergeSHA   = "merge_sha"
	argRepo       = "repo"
	argSHA        = "sha"
	argName       = "name"
	argConclusion = "conclusion"
	argContext    = "context"
	argAdd        = "add"
	argRemove     = "remove"
	argTimes      = "times"
	argMatch      = "match"
	argRetryAfter = "retry_after"
	argAPI        = "api"
	argRemaining  = "remaining"
	argResetIn    = "reset_in"
	argMode       = "mode"
	argSinceStep  = "since_step"
	argReplica    = "replica"
	argMethod     = "method"
	argPath       = "path"
	argOperation  = "operation"
	argCode       = "code"
)

// Operations of github_sub_issue and github_dependency.
const (
	opAdd    = "add"
	opRemove = "remove"
)

// Tools are the step tools the fake implements, with their arguments (contract §4.2).
var Tools = map[string][]string{
	toolIssueCreate:       {argRef, argTitle, argBody, argAuthor, argAssignees, argLabels, argCreatedAt, argBoard, argDeliver},
	toolIssueEdit:         {argRef, argTitle, argBody, argAssignees, argLabels, argState, argDeliver},
	toolBoardSet:          {argIssue, argStatus, argKind, argTeam, argPriority, argDeliver},
	toolBoardAdd:          {argIssue, argBoard, argDeliver},
	toolBoardArchive:      {argIssue, argBoard, argDeliver},
	toolBoardRemove:       {argIssue, argBoard, argDeliver},
	toolSubIssue:          {argParent, argChild, argOp, argDeliver},
	toolDependency:        {argIssue, argBlockedBy, argOp, argDeliver},
	toolPROpen:            {argRef, argAuthor, argHead, argCloses, argDraft, argBase, argTitle, argDeliver},
	toolPRPush:            {argRef, argHead, argDeliver},
	toolPRReview:          {argRef, argBy, argState, argDeliver},
	toolPRMerge:           {argRef, argMergeSHA, argBy, argDeliver},
	toolPRClose:           {argRef, argBy, argDeliver},
	toolCheckRun:          {argRepo, argSHA, argName, argStatus, argConclusion, argDeliver},
	toolStatus:            {argRepo, argSHA, argContext, argState, argDeliver},
	toolInstallationRepos: {argAdd, argRemove, argDeliver},
	toolFault:             {argKind, argStatus, argTimes, argMatch, argRetryAfter, argAPI, argRemaining, argResetIn, argMode},
	toolCalls:             {argSinceStep, argReplica, argAPI, argMethod, argPath, argOperation, argCode},
}

// Do runs a github_* step tool. State-changing tools send their webhooks before they return
// (delayed ones on the fake clock).
func (s *Server) Do(ctx context.Context, tool string, args map[string]any) (any, error) {
	if args == nil {
		args = map[string]any{}
	}
	var (
		hooks   []hook
		deliver deliverArg
		err     error
	)
	switch tool {
	case toolCalls:
		return s.callsTool(args)
	case toolFault:
		return s.faultTool(args)
	case toolIssueCreate:
		hooks, deliver, err = s.issueCreate(args)
	case toolIssueEdit:
		hooks, deliver, err = s.issueEdit(args)
	case toolBoardSet:
		hooks, deliver, err = s.boardSet(args)
	case toolBoardAdd, toolBoardArchive, toolBoardRemove:
		hooks, deliver, err = s.boardMembership(tool, args)
	case toolSubIssue:
		hooks, deliver, err = s.subIssue(args)
	case toolDependency:
		hooks, deliver, err = s.dependency(args)
	case toolPROpen:
		hooks, deliver, err = s.prOpen(args)
	case toolPRPush:
		hooks, deliver, err = s.prPush(args)
	case toolPRReview:
		hooks, deliver, err = s.prReview(args)
	case toolPRMerge:
		hooks, deliver, err = s.prMerge(args)
	case toolPRClose:
		hooks, deliver, err = s.prClose(args)
	case toolCheckRun:
		hooks, deliver, err = s.checkRun(args)
	case toolStatus:
		hooks, deliver, err = s.status(args)
	case toolInstallationRepos:
		hooks, deliver, err = s.installationRepos(args)
	default:
		return nil, fmt.Errorf("fakegithub: unknown tool %q", tool)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", tool, err)
	}
	return map[string]any{"webhooks": s.dispatch(ctx, hooks, deliver)}, nil
}

func decodeArgs(args map[string]any, out any) error {
	return decodeStrict(args, out)
}

func required(name, v string) error {
	if v == "" {
		return fmt.Errorf("%s is required", name)
	}
	return nil
}

// ---- issues ----

type issueCreateArgs struct {
	Ref       string           `yaml:"ref"`
	Title     string           `yaml:"title"`
	Body      string           `yaml:"body"`
	Author    string           `yaml:"author"`
	Assignees []string         `yaml:"assignees"`
	Labels    []string         `yaml:"labels"`
	CreatedAt string           `yaml:"createdAt"`
	Board     *seedBoardFields `yaml:"board"`
	Deliver   deliverArg       `yaml:"deliver"`
}

func (s *Server) issueCreate(args map[string]any) ([]hook, deliverArg, error) {
	var a issueCreateArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, a.Deliver, err
	}
	if err := required(argRef, a.Ref); err != nil {
		return nil, a.Deliver, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, now := s.st, s.now()
	if is, ok := st.issues[a.Ref]; ok && !is.stub {
		return nil, a.Deliver, fmt.Errorf("issue %s exists", a.Ref)
	}
	created, err := parseTime(a.CreatedAt, now)
	if err != nil {
		return nil, a.Deliver, err
	}
	if a.Board != nil {
		for _, f := range [][2]string{{fieldStatus, a.Board.Status}, {fieldTeam, a.Board.Team}, {fieldKind, a.Board.Kind}} {
			if err := st.checkOption(f[0], f[1]); err != nil {
				return nil, a.Deliver, err
			}
		}
	}
	is, ok := st.issues[a.Ref]
	if !ok {
		if is, err = st.newIssue(a.Ref, now); err != nil {
			return nil, a.Deliver, err
		}
	}
	if is.item != nil && a.Board != nil {
		return nil, a.Deliver, fmt.Errorf("issue %s is already on the board", a.Ref)
	}
	is.stub = false
	is.title, is.body = a.Title, a.Body
	if a.Author != "" {
		is.author = a.Author
	}
	is.assignees, is.labels = append([]string(nil), a.Assignees...), append([]string(nil), a.Labels...)
	is.createdAt, is.updatedAt = created, maxTime(created, now)
	hooks := []hook{s.issueHook("opened", is, is.author, nil)}
	if a.Board != nil {
		it := s.addItem(is, a.Board, now)
		hooks = append(hooks, s.itemHook("created", it, is.author, nil))
	}
	return hooks, a.Deliver, nil
}

func (s *Server) addItem(is *issue, b *seedBoardFields, now time.Time) *boardItem {
	it := &boardItem{id: stableID("item", is.ref), nodeID: s.st.itemNodeID(is.ref), issue: is.ref, createdAt: now, updatedAt: now, statusAt: now}
	if b != nil {
		it.status, it.team, it.kind, it.priority = b.Status, b.Team, b.Kind, b.Priority
	}
	s.st.items = append(s.st.items, it)
	is.item = it
	return it
}

type issueEditArgs struct {
	Ref       string     `yaml:"ref"`
	Title     *string    `yaml:"title"`
	Body      *string    `yaml:"body"`
	Assignees *[]string  `yaml:"assignees"`
	Labels    *[]string  `yaml:"labels"`
	State     *string    `yaml:"state"`
	Deliver   deliverArg `yaml:"deliver"`
}

func (s *Server) issueEdit(args map[string]any) ([]hook, deliverArg, error) {
	var a issueEditArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, a.Deliver, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, now := s.st, s.now()
	is, ok := st.issues[a.Ref]
	if !ok {
		return nil, a.Deliver, fmt.Errorf("no issue %s", a.Ref)
	}
	var hooks []hook
	sender := ghostLogin
	if len(is.assignees) > 0 {
		sender = is.assignees[0]
	}
	touch := func() { is.updatedAt = now }

	changes := &wire.IssueChanges{}
	if a.Title != nil && *a.Title != is.title {
		changes.Title = &wire.ChangeFrom{From: is.title}
		is.title = *a.Title
	}
	if a.Body != nil && *a.Body != is.body {
		changes.Body = &wire.ChangeFrom{From: is.body}
		is.body = *a.Body
	}
	if changes.Title != nil || changes.Body != nil {
		touch()
		hooks = append(hooks, s.issueHook("edited", is, sender, func(e *wire.IssuesEvent) { e.Changes = changes }))
	}
	if a.Assignees != nil {
		added, removed := diff(is.assignees, *a.Assignees)
		for _, l := range removed {
			is.assignees = remove(is.assignees, l)
			touch()
			login := l
			hooks = append(hooks, s.issueHook("unassigned", is, sender, func(e *wire.IssuesEvent) { e.Assignee = &wire.User{Login: login} }))
		}
		for _, l := range added {
			is.assignees = append(is.assignees, l)
			touch()
			login := l
			hooks = append(hooks, s.issueHook("assigned", is, sender, func(e *wire.IssuesEvent) { e.Assignee = &wire.User{Login: login} }))
		}
	}
	if a.Labels != nil {
		added, removed := diff(is.labels, *a.Labels)
		for _, l := range removed {
			is.labels = remove(is.labels, l)
			touch()
			name := l
			hooks = append(hooks, s.issueHook("unlabeled", is, sender, func(e *wire.IssuesEvent) { e.Label = &wire.Label{Name: name} }))
		}
		for _, l := range added {
			is.labels = append(is.labels, l)
			touch()
			name := l
			hooks = append(hooks, s.issueHook("labeled", is, sender, func(e *wire.IssuesEvent) { e.Label = &wire.Label{Name: name} }))
		}
	}
	if a.State != nil {
		state, reason, err := parseIssueState(*a.State)
		if err != nil {
			return nil, a.Deliver, err
		}
		switch {
		case state == stateClosed && is.state != stateClosed:
			is.state, is.stateReason, is.closedAt = state, reason, timePtr(now)
			touch()
			hooks = append(hooks, s.issueHook("closed", is, sender, nil))
		case state == stateOpen && is.state == stateClosed:
			is.state, is.stateReason, is.closedAt = stateOpen, "reopened", nil
			touch()
			hooks = append(hooks, s.issueHook("reopened", is, sender, nil))
		}
	}
	return hooks, a.Deliver, nil
}

// diff returns what to add to and remove from have to get want.
func diff(have, want []string) (added, removed []string) {
	for _, w := range want {
		if !contains(have, w) {
			added = append(added, w)
		}
	}
	for _, h := range have {
		if !contains(want, h) {
			removed = append(removed, h)
		}
	}
	return added, removed
}

// ---- board ----

type boardSetArgs struct {
	Issue    string     `yaml:"issue"`
	Status   *string    `yaml:"status"`
	Kind     *string    `yaml:"kind"`
	Team     *string    `yaml:"team"`
	Priority *float64   `yaml:"priority"`
	Deliver  deliverArg `yaml:"deliver"`
}

func (s *Server) boardSet(args map[string]any) ([]hook, deliverArg, error) {
	var a boardSetArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, a.Deliver, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, now := s.st, s.now()
	is, ok := st.issues[a.Issue]
	if !ok || is.item == nil {
		return nil, a.Deliver, fmt.Errorf("%s is not on the board", a.Issue)
	}
	it := is.item
	var hooks []hook
	set := func(field string, cur *string, want *string) error {
		if want == nil || *want == *cur {
			return nil
		}
		if err := st.checkOption(field, *want); err != nil {
			return err
		}
		from := *cur
		*cur = *want
		it.updatedAt = now
		if field == fieldStatus {
			it.statusAt = now
		}
		hooks = append(hooks, s.itemHook("edited", it, ghostLogin, s.fieldChange(field, optionChange(field, from), optionChange(field, *want))))
		return nil
	}
	if err := set(fieldStatus, &it.status, a.Status); err != nil {
		return nil, a.Deliver, err
	}
	if err := set(fieldKind, &it.kind, a.Kind); err != nil {
		return nil, a.Deliver, err
	}
	if err := set(fieldTeam, &it.team, a.Team); err != nil {
		return nil, a.Deliver, err
	}
	if a.Priority != nil && (it.priority == nil || *it.priority != *a.Priority) {
		from := numberChange(it.priority)
		p := *a.Priority
		it.priority = &p
		it.updatedAt = now
		hooks = append(hooks, s.itemHook("edited", it, ghostLogin, s.fieldChange(fieldPriority, from, p)))
	}
	return hooks, a.Deliver, nil
}

type boardMembershipArgs struct {
	Issue   string           `yaml:"issue"`
	Board   *seedBoardFields `yaml:"board"`
	Deliver deliverArg       `yaml:"deliver"`
}

func (s *Server) boardMembership(tool string, args map[string]any) ([]hook, deliverArg, error) {
	var a boardMembershipArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, a.Deliver, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, now := s.st, s.now()
	is, ok := st.issues[a.Issue]
	if !ok {
		return nil, a.Deliver, fmt.Errorf("no issue %s", a.Issue)
	}
	if a.Board != nil {
		for _, f := range [][2]string{{fieldStatus, a.Board.Status}, {fieldTeam, a.Board.Team}, {fieldKind, a.Board.Kind}} {
			if err := st.checkOption(f[0], f[1]); err != nil {
				return nil, a.Deliver, err
			}
		}
	}
	it := is.item
	switch tool {
	case toolBoardAdd:
		if it != nil && !it.archived {
			return nil, a.Deliver, fmt.Errorf("%s is already on the board", a.Issue)
		}
		if it != nil { // restoring an archived item
			it.archived, it.updatedAt = false, now
			if a.Board != nil {
				if a.Board.Status != it.status {
					it.statusAt = now
				}
				it.status, it.team, it.kind, it.priority = a.Board.Status, a.Board.Team, a.Board.Kind, a.Board.Priority
			}
			return []hook{s.itemHook("restored", it, ghostLogin, nil)}, a.Deliver, nil
		}
		it = s.addItem(is, a.Board, now)
		return []hook{s.itemHook("created", it, ghostLogin, nil)}, a.Deliver, nil
	case toolBoardArchive:
		if it == nil || it.archived {
			return nil, a.Deliver, fmt.Errorf("%s is not on the board", a.Issue)
		}
		it.archived, it.archAt, it.updatedAt = true, now, now
		return []hook{s.itemHook("archived", it, ghostLogin, nil)}, a.Deliver, nil
	default: // github_board_remove
		if it == nil {
			return nil, a.Deliver, fmt.Errorf("%s is not on the board", a.Issue)
		}
		st.removeItem(it)
		is.item = nil
		it.updatedAt = now
		return []hook{s.itemHook("deleted", it, ghostLogin, nil)}, a.Deliver, nil
	}
}

// ---- links ----

type subIssueArgs struct {
	Parent  string     `yaml:"parent"`
	Child   string     `yaml:"child"`
	Op      string     `yaml:"op"`
	Deliver deliverArg `yaml:"deliver"`
}

func (s *Server) subIssue(args map[string]any) ([]hook, deliverArg, error) {
	var a subIssueArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, a.Deliver, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, now := s.st, s.now()
	parent, ok1 := st.issues[a.Parent]
	child, ok2 := st.issues[a.Child]
	if !ok1 || !ok2 {
		return nil, a.Deliver, fmt.Errorf("parent %s and child %s must exist", a.Parent, a.Child)
	}
	var action string
	switch a.Op {
	case opAdd:
		if child.parent != "" {
			return nil, a.Deliver, fmt.Errorf("%s already has the parent %s", a.Child, child.parent)
		}
		child.parent = parent.ref
		parent.subIssues = append(parent.subIssues, child.ref)
		action = "sub_issue_added"
	case opRemove:
		if child.parent != parent.ref {
			return nil, a.Deliver, fmt.Errorf("%s is not a sub-issue of %s", a.Child, a.Parent)
		}
		child.parent = ""
		parent.subIssues = remove(parent.subIssues, child.ref)
		action = "sub_issue_removed"
	default:
		return nil, a.Deliver, fmt.Errorf("op must be add or remove, got %q", a.Op)
	}
	parent.updatedAt, child.updatedAt = now, now
	pr, cr := st.repos[parent.repo], st.repos[child.repo]
	ev := &wire.SubIssuesEvent{
		Envelope:      s.envelope(action, pr, ghostLogin),
		ParentIssueID: parent.id, ParentIssue: s.wireIssue(parent), ParentIssueRepo: s.wireRepo(pr),
		SubIssueID: child.id, SubIssue: s.wireIssue(child), SubIssueRepo: s.wireRepo(cr),
	}
	return []hook{s.repoHook(wire.EventSubIssues, action, pr, ev)}, a.Deliver, nil
}

type dependencyArgs struct {
	Issue     string     `yaml:"issue"`
	BlockedBy string     `yaml:"blocked_by"`
	Op        string     `yaml:"op"`
	Deliver   deliverArg `yaml:"deliver"`
}

func (s *Server) dependency(args map[string]any) ([]hook, deliverArg, error) {
	var a dependencyArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, a.Deliver, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, now := s.st, s.now()
	blocked, ok1 := st.issues[a.Issue]
	blocking, ok2 := st.issues[a.BlockedBy]
	if !ok1 || !ok2 {
		return nil, a.Deliver, fmt.Errorf("issue %s and blocker %s must exist", a.Issue, a.BlockedBy)
	}
	var action string
	switch a.Op {
	case opAdd:
		if contains(blocked.blockedBy, blocking.ref) {
			return nil, a.Deliver, fmt.Errorf("%s is already blocked by %s", a.Issue, a.BlockedBy)
		}
		blocked.blockedBy = append(blocked.blockedBy, blocking.ref)
		action = "blocked_by_added"
	case opRemove:
		if !contains(blocked.blockedBy, blocking.ref) {
			return nil, a.Deliver, fmt.Errorf("%s is not blocked by %s", a.Issue, a.BlockedBy)
		}
		blocked.blockedBy = remove(blocked.blockedBy, blocking.ref)
		action = "blocked_by_removed"
	default:
		return nil, a.Deliver, fmt.Errorf("op must be add or remove, got %q", a.Op)
	}
	blocked.updatedAt = now
	br := st.repos[blocked.repo]
	ev := &wire.IssueDependenciesEvent{
		Envelope:       s.envelope(action, br, ghostLogin),
		BlockedIssueID: blocked.id, BlockedIssue: s.wireIssue(blocked),
		BlockingIssueID: blocking.id, BlockingIssue: s.wireIssue(blocking), BlockingIssueRepo: s.wireRepo(st.repos[blocking.repo]),
	}
	return []hook{s.repoHook(wire.EventIssueDependencies, action, br, ev)}, a.Deliver, nil
}

// ---- pull requests ----

type prOpenArgs struct {
	Ref     string     `yaml:"ref"`
	Author  string     `yaml:"author"`
	Head    string     `yaml:"head"`
	Closes  []string   `yaml:"closes"`
	Draft   bool       `yaml:"draft"`
	Base    string     `yaml:"base"`
	Title   string     `yaml:"title"`
	Deliver deliverArg `yaml:"deliver"`
}

func (s *Server) prOpen(args map[string]any) ([]hook, deliverArg, error) {
	var a prOpenArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, a.Deliver, err
	}
	if err := required(argHead, a.Head); err != nil {
		return nil, a.Deliver, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, now := s.st, s.now()
	if _, ok := st.pulls[a.Ref]; ok {
		return nil, a.Deliver, fmt.Errorf("PR %s exists", a.Ref)
	}
	err := s.seedPull(seedPull{Ref: a.Ref, Title: a.Title, Author: a.Author, Head: a.Head, Base: a.Base, Draft: a.Draft, Closes: a.Closes}, now)
	if err != nil {
		return nil, a.Deliver, err
	}
	p := st.pulls[a.Ref]
	return []hook{s.pullHook("opened", p, p.author, nil)}, a.Deliver, nil
}

type prPushArgs struct {
	Ref     string     `yaml:"ref"`
	Head    string     `yaml:"head"`
	Deliver deliverArg `yaml:"deliver"`
}

func (s *Server) openPull(ref string) (*pull, error) {
	p, ok := s.st.pulls[ref]
	if !ok {
		return nil, fmt.Errorf("no PR %s", ref)
	}
	if p.state != stateOpen {
		return nil, fmt.Errorf("PR %s is %s", ref, p.state)
	}
	return p, nil
}

func (s *Server) prPush(args map[string]any) ([]hook, deliverArg, error) {
	var a prPushArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, a.Deliver, err
	}
	if err := required(argHead, a.Head); err != nil {
		return nil, a.Deliver, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.openPull(a.Ref)
	if err != nil {
		return nil, a.Deliver, err
	}
	before := p.head
	p.head, p.updatedAt = a.Head, s.now()
	return []hook{s.pullHook("synchronize", p, p.author, func(e *wire.PullRequestEvent) { e.Before, e.After = before, a.Head })}, a.Deliver, nil
}

type prReviewArgs struct {
	Ref     string     `yaml:"ref"`
	By      string     `yaml:"by"`
	State   string     `yaml:"state"`
	Deliver deliverArg `yaml:"deliver"`
}

func (s *Server) prReview(args map[string]any) ([]hook, deliverArg, error) {
	var a prReviewArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, a.Deliver, err
	}
	if err := required(argBy, a.By); err != nil {
		return nil, a.Deliver, err
	}
	state, err := normReviewState(a.State)
	if err != nil || state == "DISMISSED" {
		return nil, a.Deliver, fmt.Errorf("state must be approved, changes_requested or commented, got %q", a.State)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.openPull(a.Ref)
	if err != nil {
		return nil, a.Deliver, err
	}
	now := s.now()
	s.reviewSeq++
	rv := &review{id: s.reviewSeq, by: a.By, state: state, commit: p.head, submittedAt: now}
	p.reviews = append(p.reviews, rv)
	p.updatedAt = now
	r := s.st.repos[p.repo]
	ev := &wire.PullRequestReviewEvent{
		Envelope:    s.envelope("submitted", r, a.By),
		Review:      wire.Review{ID: rv.id, User: wire.User{Login: rv.by}, State: strings.ToLower(state), CommitID: rv.commit, SubmittedAt: now},
		PullRequest: s.wirePull(p),
	}
	return []hook{s.repoHook(wire.EventPullRequestReview, "submitted", r, ev)}, a.Deliver, nil
}

type prMergeArgs struct {
	Ref      string     `yaml:"ref"`
	MergeSha string     `yaml:"merge_sha"`
	By       string     `yaml:"by"`
	Deliver  deliverArg `yaml:"deliver"`
}

// prMerge merges a PR: pull_request.closed (merged) and push, and the issues it closes are closed
// (issues.closed) when it merges into the default branch, as GitHub does.
func (s *Server) prMerge(args map[string]any) ([]hook, deliverArg, error) {
	var a prMergeArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, a.Deliver, err
	}
	if err := required(argMergeSHA, a.MergeSha); err != nil {
		return nil, a.Deliver, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, now := s.st, s.now()
	p, err := s.openPull(a.Ref)
	if err != nil {
		return nil, a.Deliver, err
	}
	by := a.By
	if by == "" {
		by = p.author
	}
	p.state, p.merged, p.mergeSHA, p.mergedBy = stateClosed, true, a.MergeSha, by
	p.mergedAt, p.closedAt, p.updatedAt = timePtr(now), timePtr(now), now
	r := st.repos[p.repo]
	hooks := []hook{s.pullHook("closed", p, by, nil)}
	before := r.branchHead
	if before == "" {
		before = strings.Repeat("0", 40)
	}
	push := &wire.PushEvent{
		Envelope: s.envelope("", r, by),
		Ref:      "refs/heads/" + p.base, Before: before, After: a.MergeSha,
		Pusher: wire.Pusher{Name: by}, Commits: []wire.PushCommit{{ID: a.MergeSha}}, HeadCommit: &wire.PushCommit{ID: a.MergeSha},
	}
	hooks = append(hooks, s.repoHook(wire.EventPush, "", r, push))
	if p.base == r.defaultBranch {
		r.branchHead = a.MergeSha
		for _, ref := range p.closes {
			is, ok := st.issues[ref]
			if !ok || is.state == stateClosed {
				continue
			}
			is.state, is.stateReason, is.closedAt, is.updatedAt = stateClosed, reasonCompleted, timePtr(now), now
			hooks = append(hooks, s.issueHook("closed", is, by, nil))
		}
	}
	return hooks, a.Deliver, nil
}

type prCloseArgs struct {
	Ref     string     `yaml:"ref"`
	By      string     `yaml:"by"`
	Deliver deliverArg `yaml:"deliver"`
}

func (s *Server) prClose(args map[string]any) ([]hook, deliverArg, error) {
	var a prCloseArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, a.Deliver, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.openPull(a.Ref)
	if err != nil {
		return nil, a.Deliver, err
	}
	now := s.now()
	p.state, p.closedAt, p.updatedAt = stateClosed, timePtr(now), now
	by := a.By
	if by == "" {
		by = p.author
	}
	return []hook{s.pullHook("closed", p, by, nil)}, a.Deliver, nil
}

// ---- CI ----

type checkRunArgs struct {
	Repo       string     `yaml:"repo"`
	Sha        string     `yaml:"sha"`
	Name       string     `yaml:"name"`
	Status     string     `yaml:"status"`
	Conclusion string     `yaml:"conclusion"`
	Deliver    deliverArg `yaml:"deliver"`
}

// checkRun creates or updates a check run: check_run.created for a new run, check_run.completed
// when it completes. GitHub sends nothing when a run only moves from queued to in progress.
func (s *Server) checkRun(args map[string]any) ([]hook, deliverArg, error) {
	var a checkRunArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, a.Deliver, err
	}
	for _, f := range [][2]string{{argRepo, a.Repo}, {argSHA, a.Sha}, {argName, a.Name}} {
		if err := required(f[0], f[1]); err != nil {
			return nil, a.Deliver, err
		}
	}
	status, conclusion, err := normCheckStatus(a.Status, a.Conclusion)
	if err != nil {
		return nil, a.Deliver, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, now := s.st, s.now()
	r, err := st.ensureRepo(a.Repo)
	if err != nil {
		return nil, a.Deliver, err
	}
	cr, isNew := s.upsertCheck(a.Repo, a.Sha, a.Name, status, conclusion, now)
	action := ""
	switch {
	case status == checkCompleted:
		action = checkCompleted
	case isNew:
		action = "created"
	default:
		return nil, a.Deliver, nil
	}
	payload := wire.CheckRunPayload{CheckRun: wireCheck(cr), CheckSuite: wire.CheckSuiteRef{HeadSHA: a.Sha}, PullRequests: []wire.PullRequestRef{}}
	for _, p := range st.openPullsWithHead(a.Repo, a.Sha) {
		payload.PullRequests = append(payload.PullRequests, wire.PullRequestRef{
			Number: p.number, Head: wire.BranchRef{Ref: "pr-" + fmt.Sprint(p.number), SHA: p.head}, Base: wire.BranchRef{Ref: p.base},
		})
	}
	ev := &wire.CheckRunEvent{Envelope: s.envelope(action, r, "ci-bot"), CheckRun: payload}
	return []hook{s.repoHook(wire.EventCheckRun, action, r, ev)}, a.Deliver, nil
}

type statusArgs struct {
	Repo    string     `yaml:"repo"`
	Sha     string     `yaml:"sha"`
	Context string     `yaml:"context"`
	State   string     `yaml:"state"`
	Deliver deliverArg `yaml:"deliver"`
}

func (s *Server) status(args map[string]any) ([]hook, deliverArg, error) {
	var a statusArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, a.Deliver, err
	}
	for _, f := range [][2]string{{argRepo, a.Repo}, {argSHA, a.Sha}, {argContext, a.Context}} {
		if err := required(f[0], f[1]); err != nil {
			return nil, a.Deliver, err
		}
	}
	state, err := normStatusState(a.State)
	if err != nil {
		return nil, a.Deliver, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, now := s.st, s.now()
	r, err := st.ensureRepo(a.Repo)
	if err != nil {
		return nil, a.Deliver, err
	}
	cs := &commitStatus{id: st.nextID(), context: a.Context, state: state, createdAt: now}
	key := a.Repo + "@" + a.Sha
	st.stats[key] = append(st.stats[key], cs)
	ev := &wire.StatusEvent{
		Envelope: s.envelope("", r, "ci-bot"),
		ID:       cs.id, SHA: a.Sha, Name: a.Repo, Context: a.Context, State: state, CreatedAt: now, UpdatedAt: now,
	}
	return []hook{s.repoHook(wire.EventStatus, "", r, ev)}, a.Deliver, nil
}

// ---- installation ----

type installationReposArgs struct {
	Add     []string   `yaml:"add"`
	Remove  []string   `yaml:"remove"`
	Deliver deliverArg `yaml:"deliver"`
}

func (s *Server) installationRepos(args map[string]any) ([]hook, deliverArg, error) {
	var a installationReposArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, a.Deliver, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st
	brief := func(r *repo) wire.RepoBrief {
		return wire.RepoBrief{ID: r.id, NodeID: r.nodeID, Name: r.name(), FullName: r.fullName, Private: r.private}
	}
	var added, removed []wire.RepoBrief
	for _, name := range a.Add {
		r, err := st.ensureRepo(name)
		if err != nil {
			return nil, a.Deliver, err
		}
		if !r.installed {
			r.installed = true
			added = append(added, brief(r))
		}
	}
	for _, name := range a.Remove {
		r, ok := st.repos[name]
		if !ok {
			return nil, a.Deliver, fmt.Errorf("no repo %s", name)
		}
		if r.installed {
			r.installed = false
			removed = append(removed, brief(r))
		}
	}
	var hooks []hook
	mk := func(action string, add, rem []wire.RepoBrief) hook {
		ev := &wire.InstallationRepositoriesEvent{
			Envelope: s.envelope(action, nil, ghostLogin), RepositorySelection: "selected",
			RepositoriesAdded: add, RepositoriesRemoved: rem,
		}
		if ev.RepositoriesAdded == nil {
			ev.RepositoriesAdded = []wire.RepoBrief{}
		}
		if ev.RepositoriesRemoved == nil {
			ev.RepositoriesRemoved = []wire.RepoBrief{}
		}
		return hook{event: wire.EventInstallationRepositories, action: action, payload: ev}
	}
	if len(added) > 0 {
		hooks = append(hooks, mk("added", added, nil))
	}
	if len(removed) > 0 {
		hooks = append(hooks, mk("removed", nil, removed))
	}
	return hooks, a.Deliver, nil
}

// ---- faults ----

type faultArgs struct {
	Kind       string `yaml:"kind"`
	Status     int    `yaml:"status"`
	Times      int    `yaml:"times"`
	Match      string `yaml:"match"`
	RetryAfter string `yaml:"retry_after"`
	API        string `yaml:"api"`
	Remaining  *int   `yaml:"remaining"`
	ResetIn    string `yaml:"reset_in"`
	Mode       string `yaml:"mode"`
}

// faultArgsOf lists the arguments each fault kind takes.
var faultArgsOf = map[string][]string{
	faultHTTP:       {argKind, argStatus, argTimes, argMatch, argAPI},
	faultSecondary:  {argKind, argRetryAfter, argTimes, argMatch, argAPI},
	faultRateLimit:  {argKind, argAPI, argRemaining, argResetIn},
	faultDeliveries: {argKind, argMode},
	faultClear:      {argKind},
}

// faultTool injects a fault (github_fault). http_error and secondary_rate_limit answer the matching
// calls (a path rule, or exactly a GraphQL operation name) `times` times or until clear; rate_limit
// sets an API's remaining and reset as other users of the App would; deliveries makes every delivery
// lost or never sent until clear; clear removes every fault (rate limits stay as they are).
func (s *Server) faultTool(args map[string]any) (any, error) {
	var a faultArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, fmt.Errorf("github_fault: %w", err)
	}
	allowed, ok := faultArgsOf[a.Kind]
	if !ok {
		return nil, fmt.Errorf("github_fault: unknown kind %q", a.Kind)
	}
	for k := range args {
		if !contains(allowed, k) {
			return nil, fmt.Errorf("github_fault: kind %s takes no %s", a.Kind, k)
		}
	}
	if a.API != "" && a.API != forge.APIREST && a.API != forge.APIGraphQL && a.API != forge.APIApp {
		return nil, fmt.Errorf("github_fault: unknown api %q", a.API)
	}
	if a.Times < 0 {
		return nil, fmt.Errorf("github_fault: times must not be negative")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	switch a.Kind {
	case faultHTTP:
		if a.Status == 0 {
			a.Status = http.StatusInternalServerError
		}
		if a.Status < 400 || a.Status > 599 {
			return nil, fmt.Errorf("github_fault: status %d is not an error", a.Status)
		}
		s.faults = append(s.faults, &fault{kind: a.Kind, status: a.Status, match: a.Match, api: a.API, times: a.Times})
	case faultSecondary:
		ra := 60 * time.Second
		if a.RetryAfter != "" {
			d, err := time.ParseDuration(a.RetryAfter)
			if err != nil || d < time.Second {
				return nil, fmt.Errorf("github_fault: invalid retry_after %q", a.RetryAfter)
			}
			ra = d
		}
		s.faults = append(s.faults, &fault{kind: a.Kind, status: http.StatusForbidden, match: a.Match, api: a.API, times: a.Times, retryAfter: ra})
	case faultRateLimit:
		if a.API == "" || a.Remaining == nil {
			return nil, fmt.Errorf("github_fault: rate_limit needs api and remaining")
		}
		rl := s.limit(a.API, now)
		rl.remaining = min(max(*a.Remaining, 0), rl.limit)
		if a.ResetIn != "" {
			d, err := time.ParseDuration(a.ResetIn)
			if err != nil || d <= 0 {
				return nil, fmt.Errorf("github_fault: invalid reset_in %q", a.ResetIn)
			}
			rl.resetAt = now.Add(d)
		}
	case faultDeliveries:
		if a.Mode != deliverLost && a.Mode != deliverNever {
			return nil, fmt.Errorf("github_fault: deliveries mode must be lost or never, got %q", a.Mode)
		}
		s.delivery = a.Mode
	case faultClear:
		s.faults, s.delivery = nil, ""
	}
	return map[string]any{argKind: a.Kind}, nil
}

// ---- github_calls ----

// Counts github_calls answers with, besides one per API.
const (
	callsCount         = "count"
	callsGraphQLPoints = "graphqlPoints"
	callsMutating      = "mutating"
	callsConditional   = "conditional"
	callsNotModified   = "notModified"
	callsModified      = "modified"
	callsRateLimited   = "rateLimited"
)

type callsArgs struct {
	SinceStep string `yaml:"since_step"`
	Replica   string `yaml:"replica"`
	API       string `yaml:"api"`
	Method    string `yaml:"method"`
	Path      string `yaml:"path"`
	Operation string `yaml:"operation"`
	Code      int    `yaml:"code"`
}

// callsTool counts the recorded calls that match the filters (github_calls). modified counts GETs
// answered 2xx (they cost budget), notModified the free 304s; mutating excludes token minting.
func (s *Server) callsTool(args map[string]any) (any, error) {
	var a callsArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, fmt.Errorf("github_calls: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	from := 0
	if a.SinceStep != "" {
		mark, ok := s.marks[a.SinceStep]
		if !ok {
			return nil, fmt.Errorf("github_calls: no step %q has started", a.SinceStep)
		}
		from = mark
	}
	res := map[string]int{
		callsCount: 0, forge.APIREST: 0, forge.APIGraphQL: 0, forge.APIApp: 0, callsGraphQLPoints: 0, callsMutating: 0,
		callsConditional: 0, callsNotModified: 0, callsModified: 0, callsRateLimited: 0,
	}
	for _, c := range s.calls[from:] {
		if (a.Replica != "" && c.Replica != a.Replica) ||
			(a.API != "" && c.API != a.API) ||
			(a.Method != "" && !strings.EqualFold(c.Method, a.Method)) ||
			(a.Path != "" && !pathMatches(a.Path, c.Path)) ||
			(a.Operation != "" && c.Operation != a.Operation) ||
			(a.Code != 0 && c.Code != a.Code) {
			continue
		}
		res[callsCount]++
		res[c.API]++
		res[callsGraphQLPoints] += c.Points
		if c.Mutating {
			res[callsMutating]++
		}
		if c.Conditional {
			res[callsConditional]++
		}
		if c.NotModified {
			res[callsNotModified]++
		}
		if c.Method == http.MethodGet && c.Code >= 200 && c.Code < 300 {
			res[callsModified]++
		}
		if c.RateLimited {
			res[callsRateLimited]++
		}
	}
	out := make(map[string]any, len(res))
	keys := make([]string, 0, len(res))
	for k := range res {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out[k] = res[k]
	}
	return out, nil
}
