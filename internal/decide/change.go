package decide

import (
	"math"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/history"
)

const (
	defaultCIDeadline     = time.Hour
	defaultDeadlineFactor = 3.0
	defaultReviewDeadline = 48 * time.Hour
	defaultMergeDeadline  = 2 * time.Hour

	checkCompleted = "completed"
)

// Change decides a Change's CI verdict on the head SHA, its review state, the merge result on the merge
// commit and its wait, and records a pipeline's duration when CI on a SHA becomes final.
func Change(v ChangeView) ChangeDecision {
	var d ChangeDecision
	if v.Change == nil {
		return d
	}
	prev := v.Change.Status
	st := *prev.DeepCopy()
	if st.PR == nil {
		d.Status = st
		return d
	}
	spec := v.Spec
	if spec == nil {
		spec = &v1alpha1.InstallationSpec{}
	}
	ref := ChangeRef(v.Change)
	pr := st.PR
	f := st.Facts
	if f == nil {
		f = &v1alpha1.ChangeFacts{}
	}
	required := uniqueSorted(f.RequiredChecks)

	// CI on the head SHA.
	head := pr.HeadSHA
	if head == "" {
		head = f.HeadSHA
	}
	var headFacts []v1alpha1.CheckFact
	if f.HeadSHA == "" || f.HeadSHA == head {
		headFacts = f.Head
	}
	if len(headFacts) == 0 && pr.State != v1alpha1.PROpen && prev.CI != nil && prev.CI.SHA == head {
		// The head's verdict as it was when the PR stopped being open.
		st.CI = prev.CI.DeepCopy()
	} else {
		st.CI = verdict(head, headFacts, required)
	}

	// Review.
	rv := reviewVerdict(f)
	st.Review = &v1alpha1.ChangeReview{
		State:          rv.state,
		NeedsOtherTeam: rv.needsOtherTeam,
		Approvals:      rv.approvals,
		Required:       f.RequiredApprovals,
	}

	// The merge result on the merge commit.
	mergeSHA := ""
	if pr.State == v1alpha1.PRMerged {
		mergeSHA = pr.MergeSHA
		if mergeSHA == "" {
			mergeSHA = f.MergeSHA
		}
		var mergeFacts []v1alpha1.CheckFact
		if f.MergeSHA == "" || f.MergeSHA == mergeSHA {
			mergeFacts = f.Merge
		}
		st.MergeResult = verdict(mergeSHA, mergeFacts, required)
	} else {
		st.MergeResult = nil
	}

	// The wait: CI not final on the head, green but awaiting approval, or the merge result pending.
	var w *want
	subjectChanged := false
	switch pr.State {
	case v1alpha1.PRMerged:
		if mergeSHA != "" && st.MergeResult.State == v1alpha1.CIPending {
			dl := spec.Waits.MergeResult.Deadline.Duration
			if dl <= 0 {
				dl = defaultMergeDeadline
			}
			w = &want{On: v1alpha1.WaitOnCI, Ref: "ci:" + pr.Repo + "@" + mergeSHA, Deadline: dl, AtDeadline: spec.Waits.MergeResult.AtDeadline}
		}
	case v1alpha1.PROpen:
		switch {
		case head != "" && st.CI.State == v1alpha1.CIPending:
			w = &want{On: v1alpha1.WaitOnCI, Ref: "ci:" + pr.Repo + "@" + head, Deadline: CIDeadline(spec, v.CIDurations), AtDeadline: spec.Waits.CI.AtDeadline}
		case !pr.Draft && (st.CI.State == v1alpha1.CIGreen || st.CI.State == v1alpha1.CINone) && !rv.satisfied && !rv.changesRequested:
			dl := spec.Waits.Review.Deadline.Duration
			if dl <= 0 {
				dl = defaultReviewDeadline
			}
			w = &want{On: v1alpha1.WaitOnReview, Ref: "pr:" + ref, Deadline: dl, AtDeadline: spec.Waits.Review.AtDeadline}
			subjectChanged = prev.Review != nil &&
				(prev.Review.Approvals != rv.approvals || prev.Review.NeedsOtherTeam != rv.needsOtherTeam)
		}
	}
	wr := lifecycle(prev.Wait, w, waitCtx{
		Now: v.Now, Trigger: v.Trigger, Source: v.Source, Reobserve: v.Reobserve, Spec: spec, DropWaits: v.DropWaits,
		SubjectChanged: subjectChanged,
	})
	st.Wait = wr.Wait
	for i := range wr.Events {
		wr.Events[i].Change = ref
	}
	d.Events = wr.Events

	// Pipeline durations of CI that became final on a SHA.
	if dur, ok := becameFinal(prev.CI, st.CI, headFacts); ok {
		d.Durations = append(d.Durations, Duration{Kind: history.DurationCI, Key: pr.Repo, D: dur})
	}
	if st.MergeResult != nil {
		if dur, ok := becameFinal(prev.MergeResult, st.MergeResult, f.Merge); ok {
			d.Durations = append(d.Durations, Duration{Kind: history.DurationCI, Key: pr.Repo, D: dur})
		}
	}

	if changeMoved(&prev, &st) {
		d.WakeItems = append([]string(nil), st.Items...)
	}
	d.Status = st
	d.NextCheck = wr.NextCheck
	return d
}

// CIDeadline is deadlineFactor × the p90 (nearest rank) of the repo's observed pipeline durations once
// there are minHistory of them, else defaultDeadline.
func CIDeadline(spec *v1alpha1.InstallationSpec, durations []time.Duration) time.Duration {
	c := spec.Waits.CI
	def := c.DefaultDeadline.Duration
	if def <= 0 {
		def = defaultCIDeadline
	}
	if len(durations) == 0 || len(durations) < c.MinHistory {
		return def
	}
	factor := c.DeadlineFactor
	if factor <= 0 {
		factor = defaultDeadlineFactor
	}
	return time.Duration(factor * float64(P90(durations)))
}

// P90 is the 90th percentile of durations by nearest rank; 0 for none.
func P90(durations []time.Duration) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), durations...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	rank := int(math.Ceil(0.9 * float64(len(s))))
	if rank < 1 {
		rank = 1
	}
	return s[rank-1]
}

// verdict judges CI on a commit: none without merge-relevant checks, red if a relevant one failed,
// pending while one is missing or not final, green when all succeeded. Everything else seen is ignored.
func verdict(sha string, facts []v1alpha1.CheckFact, required []string) *v1alpha1.ChangeCI {
	ci := &v1alpha1.ChangeCI{SHA: sha}
	latest := latestByName(facts)
	isRequired := map[string]bool{}
	for _, r := range required {
		isRequired[r] = true
	}
	for name := range latest {
		if !isRequired[name] {
			ci.Ignored = append(ci.Ignored, name)
		}
	}
	sort.Strings(ci.Ignored)
	if len(required) == 0 {
		ci.State = v1alpha1.CINone
		return ci
	}
	ci.Relevant = append([]string(nil), required...)
	red, pending := false, false
	for _, r := range required {
		fct, ok := latest[r]
		if !ok || fct.Status != checkCompleted {
			pending = true
			continue
		}
		switch conclusion(fct.Conclusion) {
		case v1alpha1.CIRed:
			red = true
		case v1alpha1.CIPending:
			pending = true
		}
	}
	switch {
	case red:
		ci.State = v1alpha1.CIRed
	case pending:
		ci.State = v1alpha1.CIPending
	default:
		ci.State = v1alpha1.CIGreen
	}
	return ci
}

func conclusion(c string) string {
	switch strings.ToLower(c) {
	case "success", "neutral", "skipped":
		return v1alpha1.CIGreen
	case "":
		return v1alpha1.CIPending
	default:
		return v1alpha1.CIRed
	}
}

// latestByName keeps the newest fact per name (by start, then completion, then list order), so a rerun
// replaces the run before it.
func latestByName(facts []v1alpha1.CheckFact) map[string]v1alpha1.CheckFact {
	out := map[string]v1alpha1.CheckFact{}
	for _, f := range facts {
		if f.Name == "" {
			continue
		}
		if old, ok := out[f.Name]; ok && newer(old, f) {
			continue
		}
		out[f.Name] = f
	}
	return out
}

// newer reports whether a is strictly newer than b.
func newer(a, b v1alpha1.CheckFact) bool {
	if ta, tb := timeOf(a.StartedAt), timeOf(b.StartedAt); !ta.Equal(tb) {
		return ta.After(tb)
	}
	return timeOf(a.CompletedAt).After(timeOf(b.CompletedAt))
}

func timeOf(t *metav1.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.Time
}

type review struct {
	state            string
	approvals        int
	needsOtherTeam   bool
	satisfied        bool
	changesRequested bool
}

// reviewVerdict reads the reviews: each reviewer's latest decisive review counts; an open "changes
// requested" wins; approvals must reach requiredApprovals and cover every code-owner team.
func reviewVerdict(f *v1alpha1.ChangeFacts) review {
	latest := map[string]v1alpha1.ReviewFact{}
	var order []string
	for _, r := range f.Reviews {
		switch normReview(r.State) {
		case reviewApproved, reviewChangesRequested:
			if _, ok := latest[r.By]; !ok {
				order = append(order, r.By)
			}
			latest[r.By] = r
		case reviewDismissed:
			delete(latest, r.By)
		}
	}
	var rv review
	var approverTeams []string
	for _, by := range order {
		r, ok := latest[by]
		if !ok {
			continue
		}
		switch normReview(r.State) {
		case reviewApproved:
			rv.approvals++
			approverTeams = append(approverTeams, r.Teams...)
		case reviewChangesRequested:
			rv.changesRequested = true
		}
	}
	missing := 0
	for _, t := range f.CodeOwnerTeams {
		if containsTeam(approverTeams, t) {
			continue
		}
		missing++
		if f.OwnerTeam == "" || !sameTeam(t, f.OwnerTeam) {
			rv.needsOtherTeam = true
		}
	}
	rv.satisfied = rv.approvals >= f.RequiredApprovals && missing == 0
	switch {
	case rv.changesRequested:
		rv.state = v1alpha1.ReviewChangesRequested
	case rv.satisfied && rv.approvals > 0:
		rv.state = v1alpha1.ReviewApproved
	default:
		rv.state = v1alpha1.ReviewNone
	}
	return rv
}

// Review states of the review facts, as normReview spells them.
const (
	reviewApproved         = "approved"
	reviewChangesRequested = "changes_requested"
	reviewDismissed        = "dismissed"
)

func normReview(s string) string {
	return strings.ReplaceAll(strings.ToLower(s), "-", "_")
}

func normTeam(t string) string {
	t = strings.ToLower(strings.TrimPrefix(t, "@"))
	if i := strings.LastIndexByte(t, '/'); i >= 0 {
		t = t[i+1:]
	}
	return t
}

func sameTeam(a, b string) bool { return normTeam(a) == normTeam(b) }

func containsTeam(teams []string, t string) bool {
	for _, x := range teams {
		if sameTeam(x, t) {
			return true
		}
	}
	return false
}

// becameFinal returns the pipeline duration when CI on a SHA was seen pending and is final now.
func becameFinal(prev, cur *v1alpha1.ChangeCI, facts []v1alpha1.CheckFact) (time.Duration, bool) {
	if prev == nil || cur == nil || prev.SHA != cur.SHA || prev.State != v1alpha1.CIPending {
		return 0, false
	}
	if cur.State != v1alpha1.CIGreen && cur.State != v1alpha1.CIRed {
		return 0, false
	}
	latest := latestByName(facts)
	var start, end time.Time
	for _, name := range cur.Relevant {
		fct, ok := latest[name]
		if !ok || fct.Status != checkCompleted || fct.StartedAt == nil || fct.CompletedAt == nil {
			return 0, false
		}
		if start.IsZero() || fct.StartedAt.Time.Before(start) {
			start = fct.StartedAt.Time
		}
		if fct.CompletedAt.After(end) {
			end = fct.CompletedAt.Time
		}
	}
	if start.IsZero() || !end.After(start) {
		return 0, false
	}
	return end.Sub(start), true
}

// changeMoved reports whether anything an Item's phase depends on changed.
func changeMoved(prev, cur *v1alpha1.ChangeStatus) bool {
	if prev.CI == nil || prev.Review == nil {
		return true
	}
	if prev.PR == nil || prev.PR.State != cur.PR.State || prev.PR.Draft != cur.PR.Draft || prev.PR.HeadSHA != cur.PR.HeadSHA {
		return true
	}
	if prev.CI.SHA != cur.CI.SHA || prev.CI.State != cur.CI.State {
		return true
	}
	if prev.Review.State != cur.Review.State || prev.Review.NeedsOtherTeam != cur.Review.NeedsOtherTeam ||
		prev.Review.Approvals != cur.Review.Approvals || prev.Review.Required != cur.Review.Required {
		return true
	}
	if (prev.MergeResult == nil) != (cur.MergeResult == nil) {
		return true
	}
	return prev.MergeResult != nil && prev.MergeResult.State != cur.MergeResult.State
}

func uniqueSorted(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range s {
		if x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}
