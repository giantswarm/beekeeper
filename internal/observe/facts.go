// Package observe turns what the engine reads from GitHub into the observed facts of Items and Changes,
// and judges the freshness of its observation sources (contract §1.1, §1.3). It holds the parts of
// observation that need no Kubernetes: merging board items and conditional REST reads into status,
// diffing them into `observed` and `webhook.missed` rows, scope, refusals and retries, pending reads,
// and the freshness state of the webhooks and board sources. The controllers run it on the engine's
// clock, on the leader only.
package observe

import (
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/forge"
)

// Board statuses and kinds the engine's scope depends on (the roadmap board's option names).
const (
	StatusInbox      = "Inbox 📥"
	StatusBacklog    = "Backlog 📦"
	StatusUpNext     = "Up Next ➡️"
	StatusInProgress = "In Progress ⛏️"
	StatusBlocked    = "Blocked / Waiting ⛔️"
	StatusValidation = "Validation ☑️"
	StatusDone       = "Done ✅"

	KindBug = "Bug 🐞"
)

// ActiveStatuses are the statuses whose open items the board resync tracks.
var ActiveStatuses = []string{StatusUpNext, StatusInProgress, StatusBlocked, StatusValidation}

// IsActive reports whether status is one of the active statuses.
func IsActive(status string) bool {
	for _, s := range ActiveStatuses {
		if s == status {
			return true
		}
	}
	return false
}

// IssueState maps GitHub's state and state reason to open, closed or closed_not_planned.
func IssueState(state, reason string) string {
	switch strings.ToLower(state) {
	case "closed":
		if strings.EqualFold(reason, "not_planned") {
			return v1alpha1.IssueClosedNotPlanned
		}
		return v1alpha1.IssueClosed
	case "":
		return ""
	default:
		return v1alpha1.IssueOpen
	}
}

// Time converts a GitHub time to a status time at the precision the API stores (seconds); nil for zero.
func Time(t time.Time) *metav1.Time {
	if t.IsZero() {
		return nil
	}
	mt := metav1.NewTime(t.UTC().Truncate(time.Second))
	return &mt
}

// ApplyBoardItem sets the fields of an Item's status that observation owns from a board item and its
// issue: issue, board, assignees, parent, sub-issues, blockers and the PRs that close it. Fields the
// decide functions own are kept (a sub-issue's phase included). When GitHub answered part of the item
// with errors (Incomplete), nothing is concluded from a link's absence: the links observed before stay.
func ApplyBoardItem(st *v1alpha1.ItemStatus, bi forge.BoardItem) {
	prev := *st.DeepCopy()
	defer func() {
		if bi.Incomplete {
			keepLinks(st, &prev)
		}
	}()
	is := bi.Issue
	ref, err := v1alpha1.ParseRef(is.Ref)
	issue := &v1alpha1.ItemIssue{Title: is.Title, State: IssueState(is.State, is.StateReason), Body: is.Body,
		NodeID: is.NodeID, CreatedAt: Time(is.CreatedAt), Labels: sortedCopy(is.Labels)}
	if err == nil {
		issue.Repo, issue.Number = ref.FullRepo(), ref.Number
	} else if st.Issue != nil {
		issue.Repo, issue.Number = st.Issue.Repo, st.Issue.Number
	}
	st.Issue = issue

	board := &v1alpha1.ItemBoard{Status: bi.Status, Kind: bi.Kind, Team: bi.Team, NodeID: bi.NodeID,
		UpdatedAt: Time(bi.UpdatedAt)}
	if bi.Priority != nil {
		p := *bi.Priority
		board.Priority = &p
	}
	st.Board = board

	st.Assignees = sortedCopy(is.Assignees)
	st.Parent = is.Parent

	phases := map[string]string{}
	for _, s := range st.SubIssues {
		phases[s.Ref] = s.Phase
	}
	st.SubIssues, st.SubItems = nil, nil
	for _, l := range is.SubIssues {
		st.SubIssues = append(st.SubIssues, v1alpha1.SubIssue{Ref: l.Ref, State: IssueState(l.State, l.StateReason),
			Status: l.Status, Phase: phases[l.Ref]})
		st.SubItems = append(st.SubItems, l.Ref)
	}
	sort.Slice(st.SubIssues, func(i, j int) bool { return st.SubIssues[i].Ref < st.SubIssues[j].Ref })
	sort.Strings(st.SubItems)

	st.BlockedBy = nil
	for _, l := range is.BlockedBy {
		st.BlockedBy = append(st.BlockedBy, v1alpha1.Blocker{Ref: l.Ref, State: IssueState(l.State, l.StateReason)})
	}
	sort.Slice(st.BlockedBy, func(i, j int) bool { return st.BlockedBy[i].Ref < st.BlockedBy[j].Ref })

	// The read lists a bounded number of sub-issues and blockers; past that only GitHub's totals tell.
	st.SubIssuesTotal, st.BlockedByTotal = 0, 0
	if is.SubIssuesTotal > len(is.SubIssues) {
		st.SubIssuesTotal = is.SubIssuesTotal
	}
	if is.BlockedByTotal > len(is.BlockedBy) {
		st.BlockedByTotal = is.BlockedByTotal
	}

	st.Changes = ClosingPRs(is.ClosedBy)
}

// keepLinks adds back the links of prev a partial read left out: the parent, sub-issues, blockers and
// closing PRs.
func keepLinks(st, prev *v1alpha1.ItemStatus) {
	if st.Parent == "" {
		st.Parent = prev.Parent
	}
	for _, s := range prev.SubIssues {
		if !contains(st.SubItems, s.Ref) {
			st.SubIssues = append(st.SubIssues, s)
			st.SubItems = append(st.SubItems, s.Ref)
		}
	}
	sort.Slice(st.SubIssues, func(i, j int) bool { return st.SubIssues[i].Ref < st.SubIssues[j].Ref })
	sort.Strings(st.SubItems)
	have := map[string]bool{}
	for _, b := range st.BlockedBy {
		have[b.Ref] = true
	}
	for _, b := range prev.BlockedBy {
		if !have[b.Ref] {
			st.BlockedBy = append(st.BlockedBy, b)
		}
	}
	sort.Slice(st.BlockedBy, func(i, j int) bool { return st.BlockedBy[i].Ref < st.BlockedBy[j].Ref })
	for _, c := range prev.Changes {
		if !contains(st.Changes, c) {
			st.Changes = append(st.Changes, c)
		}
	}
	sort.Strings(st.Changes)
	st.SubIssuesTotal = max(st.SubIssuesTotal, prev.SubIssuesTotal)
	st.BlockedByTotal = max(st.BlockedByTotal, prev.BlockedByTotal)
}

// ClosingPRs are the refs of the PRs that close an issue and may still land or have landed: open and
// merged ones, not those closed without merging. Sorted.
func ClosingPRs(prs []forge.LinkedPR) []string {
	var refs []string
	for _, pr := range prs {
		state := strings.ToLower(pr.State)
		if state == v1alpha1.PRClosed && !pr.Merged {
			continue
		}
		refs = append(refs, pr.Ref)
	}
	sort.Strings(refs)
	return dedupSorted(refs)
}

// PRStatus converts a read PR to the Change's observed PR.
func PRStatus(pr forge.PullRequest, repo string, number int) *v1alpha1.ChangePR {
	out := &v1alpha1.ChangePR{Repo: repo, Number: number, Draft: pr.Draft, Author: pr.Author, HeadSHA: pr.HeadSHA,
		Base: pr.Base, NodeID: pr.NodeID}
	switch {
	case pr.Merged || strings.EqualFold(pr.State, v1alpha1.PRMerged):
		out.State = v1alpha1.PRMerged
		out.MergeSHA = pr.MergeSHA
	case strings.EqualFold(pr.State, v1alpha1.PRClosed):
		out.State = v1alpha1.PRClosed
	default:
		out.State = v1alpha1.PROpen
	}
	return out
}

// Fact kinds of a CheckFact.
const (
	FactCheck  = "check"
	FactStatus = "status"
)

// CheckFacts converts check runs to facts.
func CheckFacts(runs []forge.CheckRun) []v1alpha1.CheckFact {
	var out []v1alpha1.CheckFact
	for _, r := range runs {
		out = append(out, v1alpha1.CheckFact{Kind: FactCheck, Name: r.Name, Status: strings.ToLower(r.Status),
			Conclusion: strings.ToLower(r.Conclusion), StartedAt: Time(r.StartedAt), CompletedAt: Time(r.CompletedAt)})
	}
	return sortFacts(out)
}

// StatusFacts converts commit statuses to facts: pending is in progress; success, failure and error
// complete as success, failure and failure.
func StatusFacts(statuses []forge.CommitStatus) []v1alpha1.CheckFact {
	latest := map[string]forge.CommitStatus{}
	for _, s := range statuses {
		if cur, ok := latest[s.Context]; !ok || s.UpdatedAt.After(cur.UpdatedAt) {
			latest[s.Context] = s
		}
	}
	var out []v1alpha1.CheckFact
	for _, s := range latest {
		f := v1alpha1.CheckFact{Kind: FactStatus, Name: s.Context, StartedAt: Time(s.CreatedAt)}
		switch strings.ToLower(s.State) {
		case "success":
			f.Status, f.Conclusion, f.CompletedAt = "completed", "success", Time(s.UpdatedAt)
		case "failure", "error":
			f.Status, f.Conclusion, f.CompletedAt = "completed", "failure", Time(s.UpdatedAt)
		default:
			f.Status = "in_progress"
		}
		out = append(out, f)
	}
	return sortFacts(out)
}

// ReviewFacts converts reviews to facts, oldest first, with lower-case states (approved,
// changes_requested, commented, dismissed).
func ReviewFacts(reviews []forge.Review) []v1alpha1.ReviewFact {
	rs := append([]forge.Review(nil), reviews...)
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].SubmittedAt.Before(rs[j].SubmittedAt) })
	var out []v1alpha1.ReviewFact
	for _, r := range rs {
		out = append(out, v1alpha1.ReviewFact{By: r.By, State: strings.ToLower(r.State), Teams: sortedCopy(r.ByTeams)})
	}
	return out
}

// replaceKind returns facts with every fact of kind replaced by repl.
func replaceKind(facts []v1alpha1.CheckFact, kind string, repl []v1alpha1.CheckFact) []v1alpha1.CheckFact {
	var out []v1alpha1.CheckFact
	for _, f := range facts {
		if f.Kind != kind {
			out = append(out, f)
		}
	}
	return sortFacts(append(out, repl...))
}

func sortFacts(fs []v1alpha1.CheckFact) []v1alpha1.CheckFact {
	sort.SliceStable(fs, func(i, j int) bool {
		if fs[i].Kind != fs[j].Kind {
			return fs[i].Kind < fs[j].Kind
		}
		return fs[i].Name < fs[j].Name
	})
	return fs
}

func sortedCopy(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func dedupSorted(s []string) []string {
	if len(s) < 2 {
		return s
	}
	out := s[:1]
	for _, v := range s[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}
