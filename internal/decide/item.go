package decide

import (
	"time"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/decision"
	"github.com/giantswarm/beekeeper/internal/history"
)

const defaultRetention = 168 * time.Hour

// Item decides an Item's class, phase, owner, eligibility and dependency wait, and asks ready? and
// bug-severity? where the code checks let them run. Status.Shadow is copied through unchanged.
func Item(v ItemView) ItemDecision {
	var d ItemDecision
	if v.Item == nil {
		return d
	}
	prev := v.Item.Status
	st := *prev.DeepCopy()
	ref := ItemRef(v.Item)
	team := teamName(v.Team)
	src := obsSource(v.Trigger, v.Source)
	stamp := func(evs []history.Event) {
		for i := range evs {
			evs[i].Item = ref
			evs[i].Team = team
		}
		d.Events = append(d.Events, evs...)
	}

	if v.LeftScope {
		d.Status = st
		d.Delete = true
		d.TeamPass = true
		stamp([]history.Event{event(v.Now, EventItemRemoved, src, map[string]any{"reason": v.ScopeReason})})
		return d
	}
	if st.Issue == nil {
		d.Status = st
		return d
	}

	spec := v.Spec
	if spec == nil {
		spec = &v1alpha1.InstallationSpec{}
	}
	kind, status := boardKind(&st), boardStatus(&st)
	open := st.Issue.State == v1alpha1.IssueOpen || st.Issue.State == ""
	scoped := st.InScope == nil || *st.InScope

	// Class, owner, links and what the body says.
	st.Class = classOf(kind)
	st.Owner = itemOwner(&st, v.Parent, v.Participants)
	assignedElsewhere := len(st.Assignees) > 0 && firstParticipant(st.Assignees, v.Participants) == ""
	if len(st.SubIssues) > 0 {
		subs := make([]string, 0, len(st.SubIssues))
		for _, s := range st.SubIssues {
			subs = append(subs, s.Ref)
		}
		sortRefs(subs)
		st.SubItems = subs
	}
	st.TargetRepos = decision.TargetRepos(spec.Ready, st.Issue.Repo, st.Issue.Body)
	st.AcceptanceCriteria = decision.AcceptanceCriteria(spec.Ready.AcceptanceCriteria, st.Issue.Body)
	st.Footprint = decision.Footprint(spec.Footprint, st.Issue.Body)

	// Phase.
	if st.Class == v1alpha1.ClassEpic {
		st.Phase = epicPhase(&st, status, open)
	} else {
		st.Phase = itemPhase(status, open, v.Changes)
	}

	// Decision points, only for subjects that pass the code checks first.
	optIn := optedIn(v.Team)
	facts := decision.ReadyFacts{Title: st.Issue.Title, Body: st.Issue.Body, Repo: st.Issue.Repo, Bug: kind == kindBug}
	readyAnswer := ""
	if askReady(scoped, open, st.Phase, st.Class, optIn, assignedElsewhere) {
		q := decision.ReadyQuestion(spec, team, ref, facts)
		readyAnswer = q.Rule
		d.Questions = append(d.Questions, q)
	}
	if askSeverity(scoped, open, kind, status, optIn, assignedElsewhere) {
		d.Questions = append(d.Questions, decision.BugSeverityQuestion(spec, team, ref, st.Issue.Title, st.Issue.Body))
	}

	// Eligibility.
	blocker := firstOpenBlocker(st.BlockedBy)
	// Blockers past the read's list may be open: not unblocked until they are seen.
	unseenBlockers := blocker == "" && st.BlockedByTotal > len(st.BlockedBy)
	var reasons []string
	if !open {
		reasons = []string{v1alpha1.ReasonClosed}
	} else {
		// Up Next on the board (a merged item left in Up Next isn't startable).
		if status != statusUpNext || (st.Class != v1alpha1.ClassEpic && st.Phase != v1alpha1.PhaseReady) {
			reasons = append(reasons, v1alpha1.ReasonNotUpNext)
		}
		if !optIn {
			reasons = append(reasons, v1alpha1.ReasonTeamNotOptedIn)
		}
		if st.Class != v1alpha1.ClassImplementable {
			reasons = append(reasons, v1alpha1.ReasonKindNotImplementable)
		}
		if assignedElsewhere {
			reasons = append(reasons, v1alpha1.ReasonAssignedToNonParticipant)
		}
		if len(reasons) == 0 {
			if readyAnswer == decision.AnswerNotReady {
				reasons = append(reasons, v1alpha1.ReasonNotReady)
			}
			if blocker != "" || unseenBlockers {
				reasons = append(reasons, v1alpha1.ReasonBlockedByOpenIssue)
			}
		}
	}
	st.Eligible = &v1alpha1.Eligibility{Value: len(reasons) == 0, Reasons: reasons}

	// Retention of an Item that left by its status.
	left := scoped && leftByStatus(st.Class, st.Phase, status, kind, open)
	var retentionAt *time.Time
	if left {
		if st.LeftAt == nil {
			st.LeftAt = mtp(v.Now)
		}
		retention := spec.ItemRetention.Duration
		if retention <= 0 {
			retention = defaultRetention
		}
		at := st.LeftAt.Add(retention)
		if !v.Now.Before(at) {
			d.Status = st
			d.Delete = true
			d.TeamPass = true
			stamp([]history.Event{event(v.Now, EventItemRemoved, src, map[string]any{"reason": ReasonRetention})})
			return d
		}
		retentionAt = &at
	} else {
		st.LeftAt = nil
	}

	// The dependency wait of a Ready item blocked by an open issue.
	var w *want
	if !left && scoped && st.Phase == v1alpha1.PhaseReady && blocker != "" {
		dl := spec.Waits.Dependency.Deadline.Duration
		if dl <= 0 {
			dl = defaultRetention
		}
		w = &want{On: v1alpha1.WaitOnDependency, Ref: "issue:" + blocker, Deadline: dl, AtDeadline: spec.Waits.Dependency.AtDeadline}
	}
	wr := lifecycle(prev.Wait, w, waitCtx{
		Now: v.Now, Trigger: v.Trigger, Source: v.Source, Reobserve: v.Reobserve, Spec: spec, DropWaits: v.DropWaits,
	})
	st.Wait = wr.Wait

	// History rows of what changed.
	var evs []history.Event
	if prev.Phase != st.Phase {
		evs = append(evs, event(v.Now, EventPhaseChanged, src, map[string]any{"from": prev.Phase, "to": st.Phase}))
	}
	if !sameEligibility(prev.Eligible, st.Eligible) {
		rs := reasons
		if rs == nil {
			rs = []string{}
		}
		evs = append(evs, event(v.Now, EventEligibilityChanged, src, map[string]any{"value": st.Eligible.Value, "reasons": rs}))
	}
	evs = append(evs, wr.Events...)
	stamp(evs)

	d.Status = st // Shadow as it was: only the team pass writes it
	d.NextCheck = earliest(wr.NextCheck, retentionAt)
	d.TeamPass = prev.Phase != st.Phase || prev.Class != st.Class || prev.Owner != st.Owner ||
		!sameEligibility(prev.Eligible, st.Eligible) ||
		!equalStrings(prev.TargetRepos, st.TargetRepos) || !equalStrings(prev.Footprint, st.Footprint) ||
		!equalStrings(prev.SubItems, st.SubItems) || (prev.LeftAt == nil) != (st.LeftAt == nil) ||
		observedNow(st.Observed, v.Now)
	return d
}

// itemOwner is the item's participant assignee; an unassigned sub-item belongs to its parent's owner.
func itemOwner(st *v1alpha1.ItemStatus, parent *v1alpha1.Item, participants []string) string {
	if len(st.Assignees) > 0 {
		return firstParticipant(st.Assignees, participants)
	}
	if parent == nil {
		return ""
	}
	if o := firstParticipant(parent.Status.Assignees, participants); o != "" {
		return o
	}
	if isParticipant(participants, parent.Status.Owner) {
		return parent.Status.Owner
	}
	return ""
}

func firstParticipant(assignees, participants []string) string {
	for _, a := range assignees {
		if isParticipant(participants, a) {
			return a
		}
	}
	return ""
}

func askReady(scoped, open bool, phase, class string, optIn, assignedElsewhere bool) bool {
	return scoped && open && phase == v1alpha1.PhaseReady && class == v1alpha1.ClassImplementable && optIn && !assignedElsewhere
}

func askSeverity(scoped, open bool, kind, status string, optIn, assignedElsewhere bool) bool {
	return scoped && open && kind == kindBug && (status == statusInbox || status == statusUpNext) && optIn && !assignedElsewhere
}

func firstOpenBlocker(bs []v1alpha1.Blocker) string {
	for _, b := range bs {
		if b.State == v1alpha1.IssueOpen {
			return b.Ref
		}
	}
	return ""
}

func sameEligibility(a, b *v1alpha1.Eligibility) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Value == b.Value && equalStrings(a.Reasons, b.Reasons)
}

func observedNow(o *v1alpha1.Observed, now time.Time) bool {
	return o != nil && o.At.Time.Equal(now)
}

// itemPhase derives an implementable (or incident, or ignored) item's phase from its board status, its
// issue's state and the PRs that close it (contract §1.2).
func itemPhase(status string, open bool, changes []*v1alpha1.Change) string {
	merged := false
	var prs []*v1alpha1.Change
	for _, c := range changes {
		if c == nil || c.Status.PR == nil {
			continue
		}
		switch c.Status.PR.State {
		case v1alpha1.PRMerged:
			merged = true
		case v1alpha1.PROpen:
			prs = append(prs, c)
		}
	}
	switch {
	case status == statusDone, !open && !merged:
		return v1alpha1.PhaseDone
	case status == statusValidation:
		return v1alpha1.PhaseValidating
	case merged:
		return v1alpha1.PhaseMerged
	case status == statusBlocked:
		return v1alpha1.PhaseWaiting
	case status == statusUpNext:
		return v1alpha1.PhaseReady
	case status == statusInProgress:
		return prPhase(prs)
	default:
		return v1alpha1.PhaseBacklog
	}
}

// prPhase is the phase of an In Progress item with open PRs: Fixing if any is red or has changes
// requested, Merging when all are green and approved where needed, else Published.
func prPhase(prs []*v1alpha1.Change) string {
	if len(prs) == 0 {
		return v1alpha1.PhaseImplementing
	}
	allMerging := true
	for _, c := range prs {
		ci := v1alpha1.CIPending
		if c.Status.CI != nil {
			ci = c.Status.CI.State
		}
		r := c.Status.Review
		if ci == v1alpha1.CIRed || (r != nil && r.State == v1alpha1.ReviewChangesRequested) {
			return v1alpha1.PhaseFixing
		}
		if (ci != v1alpha1.CIGreen && ci != v1alpha1.CINone) || !approvedWhereNeeded(r) {
			allMerging = false
		}
	}
	if allMerging {
		return v1alpha1.PhaseMerging
	}
	return v1alpha1.PhasePublished
}

func approvedWhereNeeded(r *v1alpha1.ChangeReview) bool {
	if r == nil {
		return false
	}
	return r.State == v1alpha1.ReviewApproved ||
		(r.State == v1alpha1.ReviewNone && r.Required == 0 && !r.NeedsOtherTeam)
}

// epicPhase: Planned (no sub-item past Ready, or none), Active (one past Ready, Done included), Done
// (every sub-item Done or closed, "not planned" counting as cancelled), or Done when the epic is.
func epicPhase(st *v1alpha1.ItemStatus, status string, open bool) string {
	if !open || status == statusDone {
		return v1alpha1.PhaseDone
	}
	if len(st.SubIssues) == 0 {
		return v1alpha1.PhasePlanned
	}
	// Sub-issues past the read's list may be open: the epic isn't Done until they are seen.
	allDone, anyPast := st.SubIssuesTotal <= len(st.SubIssues), false
	for _, s := range st.SubIssues {
		p := s.Phase
		if p == "" {
			p = subIssuePhase(s)
		}
		closed := s.State != "" && s.State != v1alpha1.IssueOpen
		if p != v1alpha1.PhaseDone && !closed {
			allDone = false
		}
		if s.State != v1alpha1.IssueClosedNotPlanned && pastReady(p) {
			anyPast = true
		}
	}
	switch {
	case allDone:
		return v1alpha1.PhaseDone
	case anyPast:
		return v1alpha1.PhaseActive
	default:
		return v1alpha1.PhasePlanned
	}
}

// subIssuePhase derives a sub-issue's phase from its state and board status when it isn't an Item.
func subIssuePhase(s v1alpha1.SubIssue) string {
	if s.State != "" && s.State != v1alpha1.IssueOpen {
		return v1alpha1.PhaseDone
	}
	switch norm(s.Status) {
	case statusUpNext:
		return v1alpha1.PhaseReady
	case statusInProgress:
		return v1alpha1.PhaseImplementing
	case statusBlocked:
		return v1alpha1.PhaseWaiting
	case statusValidation:
		return v1alpha1.PhaseValidating
	case statusDone:
		return v1alpha1.PhaseDone
	}
	return v1alpha1.PhaseBacklog
}

func pastReady(phase string) bool {
	switch phase {
	case "", v1alpha1.PhaseBacklog, v1alpha1.PhaseReady, v1alpha1.PhasePlanned:
		return false
	}
	return true
}

// inFlight reports whether a phase counts as work in flight (Implementing through Validating).
func inFlight(phase string) bool {
	switch phase {
	case v1alpha1.PhaseImplementing, v1alpha1.PhasePublished, v1alpha1.PhaseFixing, v1alpha1.PhaseMerging,
		v1alpha1.PhaseMerged, v1alpha1.PhaseWaiting, v1alpha1.PhaseValidating:
		return true
	}
	return false
}

// leftByStatus reports whether an item has left the tracked set by its status: moved out of the active
// statuses (Inbox only tracks bugs), or done, or closed without a merged PR. A merged item stays until
// the board says Done.
func leftByStatus(class, phase, status, kind string, open bool) bool {
	if class == v1alpha1.ClassEpic {
		return !open || !activeStatus(status, kind)
	}
	switch phase {
	case v1alpha1.PhaseMerged, v1alpha1.PhaseValidating:
		return false
	case v1alpha1.PhaseDone:
		return true
	}
	return !open || !activeStatus(status, kind)
}
