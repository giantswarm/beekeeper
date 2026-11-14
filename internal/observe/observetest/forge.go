// Package observetest has an in-memory forge.Reader for the engine's unit and smoke tests, until the
// fake GitHub is wired: it answers from maps, honours ETags, records calls and injects errors.
package observetest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/giantswarm/beekeeper/internal/forge"
)

// Call is one recorded read.
type Call struct {
	Op          string
	Key         string
	Conditional bool
	NotModified bool
	Err         error
}

// Forge is an in-memory forge.Reader.
type Forge struct {
	mu sync.Mutex

	Schema     forge.BoardSchema
	Items      map[string]forge.BoardItem // by node id
	PRs        map[string]forge.PullRequest
	ReviewsBy  map[string][]forge.Review
	Checks     map[string][]forge.CheckRun     // by repo@sha
	StatusesBy map[string][]forge.CommitStatus // by repo@sha
	Rules      map[string]forge.BranchRules    // by repo
	ProbeItems []forge.ProbeItem
	Log        []forge.DeliveryDetail
	Repos      []string
	Limits     forge.Budget

	// Errs fail the next calls of an operation (by op name), one error per call.
	Errs map[string][]error

	Calls []Call
}

// New returns an empty Forge.
func New() *Forge {
	return &Forge{Items: map[string]forge.BoardItem{}, PRs: map[string]forge.PullRequest{},
		ReviewsBy: map[string][]forge.Review{}, Checks: map[string][]forge.CheckRun{},
		StatusesBy: map[string][]forge.CommitStatus{}, Rules: map[string]forge.BranchRules{}, Errs: map[string][]error{}}
}

var _ forge.Reader = (*Forge)(nil)

// Fail makes the next calls of op fail with errs, in order.
func (f *Forge) Fail(op string, errs ...error) {
	f.mu.Lock()
	f.Errs[op] = append(f.Errs[op], errs...)
	f.mu.Unlock()
}

// CallsOf returns the recorded calls of op.
func (f *Forge) CallsOf(op string) []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Call
	for _, c := range f.Calls {
		if c.Op == op {
			out = append(out, c)
		}
	}
	return out
}

// Count returns the number of recorded calls.
func (f *Forge) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Calls)
}

// Lock and Unlock guard direct changes to the maps while the engine runs.
func (f *Forge) Lock()   { f.mu.Lock() }
func (f *Forge) Unlock() { f.mu.Unlock() }

func (f *Forge) begin(op, key string) error {
	if errs := f.Errs[op]; len(errs) > 0 {
		f.Errs[op] = errs[1:]
		f.Calls = append(f.Calls, Call{Op: op, Key: key, Err: errs[0]})
		return errs[0]
	}
	return nil
}

func etag(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

func (f *Forge) conditional(op, key, inm string, v any) forge.Meta {
	tag := etag(v)
	m := forge.Meta{ETag: tag, NotModified: inm != "" && inm == tag}
	f.Calls = append(f.Calls, Call{Op: op, Key: key, Conditional: inm != "", NotModified: m.NotModified})
	return m
}

// BoardSchema implements forge.Reader.
func (f *Forge) BoardSchema(context.Context, string, int) (forge.BoardSchema, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(forge.OpBoardSchema, ""); err != nil {
		return forge.BoardSchema{}, err
	}
	f.Calls = append(f.Calls, Call{Op: forge.OpBoardSchema})
	return f.Schema, nil
}

// BoardTeamItems implements forge.Reader.
func (f *Forge) BoardTeamItems(_ context.Context, q forge.TeamItemsQuery) (forge.TeamItems, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(forge.OpBoardTeamItems, q.TeamOption); err != nil {
		return forge.TeamItems{}, err
	}
	f.Calls = append(f.Calls, Call{Op: forge.OpBoardTeamItems, Key: q.TeamOption})
	var out forge.TeamItems
	for _, node := range f.sortedNodes() {
		bi := f.Items[node]
		if !bi.IsIssue || bi.Archived || bi.Team != q.TeamOption || !strings.EqualFold(bi.Issue.State, "open") {
			continue
		}
		switch {
		case contains(q.Statuses, bi.Status):
			out.Active = append(out.Active, bi)
		case bi.Status == q.InboxStatus && bi.Kind == q.BugKind:
			out.InboxBugs = append(out.InboxBugs, bi)
		}
	}
	out.Points = 1
	return out, nil
}

func (f *Forge) sortedNodes() []string {
	nodes := make([]string, 0, len(f.Items))
	for n := range f.Items {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	return nodes
}

// BoardItem implements forge.Reader.
func (f *Forge) BoardItem(_ context.Context, node string) (forge.BoardItem, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(forge.OpBoardItemsByNode, node); err != nil {
		return forge.BoardItem{}, false, err
	}
	f.Calls = append(f.Calls, Call{Op: forge.OpBoardItemsByNode, Key: node})
	bi, ok := f.Items[node]
	return bi, ok, nil
}

// BoardProbe implements forge.Reader.
func (f *Forge) BoardProbe(_ context.Context, q forge.ProbeQuery) (forge.Probe, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(forge.OpBoardProbe, q.TeamOption); err != nil {
		return forge.Probe{}, err
	}
	f.Calls = append(f.Calls, Call{Op: forge.OpBoardProbe, Key: q.TeamOption})
	if f.ProbeItems != nil {
		return forge.Probe{Items: append([]forge.ProbeItem(nil), f.ProbeItems...)}, nil
	}
	var p forge.Probe
	for _, node := range f.sortedNodes() {
		bi := f.Items[node]
		if bi.Team == q.TeamOption {
			p.Items = append(p.Items, forge.ProbeItem{IsIssue: bi.IsIssue, NodeID: node, IssueRef: bi.Issue.Ref, UpdatedAt: bi.UpdatedAt})
		}
	}
	return p, nil
}

// Issue implements forge.Reader.
func (f *Forge) Issue(_ context.Context, ref, inm string) (forge.Issue, forge.Meta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(forge.OpIssuesGet, ref); err != nil {
		return forge.Issue{}, forge.Meta{}, err
	}
	for _, bi := range f.Items {
		if bi.Issue.Ref == ref {
			return bi.Issue, f.conditional(forge.OpIssuesGet, ref, inm, bi.Issue), nil
		}
	}
	return forge.Issue{}, forge.Meta{}, &forge.Error{Kind: forge.Refused, API: forge.APIREST, Operation: forge.OpIssuesGet, Code: 404}
}

// Repo implements forge.Reader.
func (f *Forge) Repo(_ context.Context, repo, inm string) (forge.Repo, forge.Meta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(forge.OpReposGet, repo); err != nil {
		return forge.Repo{}, forge.Meta{}, err
	}
	r := forge.Repo{FullName: repo, DefaultBranch: "main"}
	return r, f.conditional(forge.OpReposGet, repo, inm, r), nil
}

// PullRequest implements forge.Reader.
func (f *Forge) PullRequest(_ context.Context, ref, inm string) (forge.PullRequest, forge.Meta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(forge.OpPullsGet, ref); err != nil {
		return forge.PullRequest{}, forge.Meta{}, err
	}
	pr, ok := f.PRs[ref]
	if !ok {
		f.Calls = append(f.Calls, Call{Op: forge.OpPullsGet, Key: ref})
		return pr, forge.Meta{}, &forge.Error{Kind: forge.Refused, API: forge.APIREST, Operation: forge.OpPullsGet, Code: 404}
	}
	return pr, f.conditional(forge.OpPullsGet, ref, inm, pr), nil
}

// Reviews implements forge.Reader.
func (f *Forge) Reviews(_ context.Context, ref, inm string) ([]forge.Review, forge.Meta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(forge.OpPullsListReviews, ref); err != nil {
		return nil, forge.Meta{}, err
	}
	rs := f.ReviewsBy[ref]
	return rs, f.conditional(forge.OpPullsListReviews, ref, inm, rs), nil
}

// CheckRuns implements forge.Reader.
func (f *Forge) CheckRuns(_ context.Context, repo, sha, inm string) ([]forge.CheckRun, forge.Meta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := repo + "@" + sha
	if err := f.begin(forge.OpChecksListForRef, key); err != nil {
		return nil, forge.Meta{}, err
	}
	rs := f.Checks[key]
	return rs, f.conditional(forge.OpChecksListForRef, key, inm, rs), nil
}

// Statuses implements forge.Reader.
func (f *Forge) Statuses(_ context.Context, repo, sha, inm string) ([]forge.CommitStatus, forge.Meta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := repo + "@" + sha
	if err := f.begin(forge.OpStatusesListForRef, key); err != nil {
		return nil, forge.Meta{}, err
	}
	rs := f.StatusesBy[key]
	return rs, f.conditional(forge.OpStatusesListForRef, key, inm, rs), nil
}

// BranchRules implements forge.Reader.
func (f *Forge) BranchRules(_ context.Context, repo, branch, inm string) (forge.BranchRules, forge.Meta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(forge.OpReposGetBranchRules, repo); err != nil {
		return forge.BranchRules{}, forge.Meta{}, err
	}
	r := f.Rules[repo]
	return r, f.conditional(forge.OpReposGetBranchRules, repo+"@"+branch, inm, r), nil
}

// Deliveries implements forge.Reader: the log entries after cursor (an index), oldest first.
func (f *Forge) Deliveries(_ context.Context, cursor string) ([]forge.Delivery, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(forge.OpListWebhookDeliveries, cursor); err != nil {
		return nil, cursor, err
	}
	f.Calls = append(f.Calls, Call{Op: forge.OpListWebhookDeliveries, Key: cursor})
	start := 0
	if cursor != "" {
		_, _ = fmt.Sscanf(cursor, "%d", &start) // a cursor that isn't a number starts at the beginning
	}
	var out []forge.Delivery
	for _, d := range f.Log[min(start, len(f.Log)):] {
		out = append(out, d.Delivery)
	}
	return out, fmt.Sprint(len(f.Log)), nil
}

// Delivery implements forge.Reader.
func (f *Forge) Delivery(_ context.Context, id string) (forge.DeliveryDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(forge.OpGetWebhookDelivery, id); err != nil {
		return forge.DeliveryDetail{}, err
	}
	f.Calls = append(f.Calls, Call{Op: forge.OpGetWebhookDelivery, Key: id})
	for _, d := range f.Log {
		if d.ID == id {
			return d, nil
		}
	}
	return forge.DeliveryDetail{}, &forge.Error{Kind: forge.Refused, API: forge.APIApp, Operation: forge.OpGetWebhookDelivery, Code: 404}
}

// Budget implements forge.Reader.
func (f *Forge) Budget() forge.Budget {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Limits
}

// InstallationRepos implements forge.Reader.
func (f *Forge) InstallationRepos(_ context.Context, inm string) ([]string, forge.Meta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(forge.OpListInstallationRepos, ""); err != nil {
		return nil, forge.Meta{}, err
	}
	repos := append([]string(nil), f.Repos...)
	return repos, f.conditional(forge.OpListInstallationRepos, "", inm, repos), nil
}

// BoardItemsByNode implements forge.Reader.
func (f *Forge) BoardItemsByNode(_ context.Context, nodes []string) (map[string]forge.BoardItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(forge.OpBoardItemsByNode, strings.Join(nodes, ",")); err != nil {
		return nil, err
	}
	f.Calls = append(f.Calls, Call{Op: forge.OpBoardItemsByNode, Key: strings.Join(nodes, ",")})
	out := map[string]forge.BoardItem{}
	for _, n := range nodes {
		if bi, ok := f.Items[n]; ok {
			out[n] = bi
		}
	}
	return out, nil
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// PullClosingIssues returns the issues of board items whose ClosedBy lists the PR.
func (f *Forge) PullClosingIssues(_ context.Context, ref string) ([]forge.LinkedIssue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(forge.OpPullClosingIssues, ref); err != nil {
		return nil, err
	}
	f.Calls = append(f.Calls, Call{Op: forge.OpPullClosingIssues, Key: ref})
	var out []forge.LinkedIssue
	for _, node := range f.sortedNodes() {
		is := f.Items[node].Issue
		for _, pr := range is.ClosedBy {
			if pr.Ref == ref {
				out = append(out, forge.LinkedIssue{Ref: is.Ref, State: is.State, StateReason: is.StateReason, Status: f.Items[node].Status})
			}
		}
	}
	return out, nil
}
