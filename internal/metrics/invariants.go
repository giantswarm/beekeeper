package metrics

import (
	"sort"
	"strconv"
	"time"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
)

// Invariants (contract §1.5, design §12.2), exported as engine_invariant_violations{invariant}.
const (
	InvWIPHasRunOrWait         = "wip-has-run-or-wait"
	InvWaitWithinDeadline      = "wait-within-deadline"
	InvStageAgeUnderP90        = "stage-age-under-p90"
	InvRefusalNotRepeated      = "refusal-not-repeated"
	InvSourceFreshOrDegraded   = "source-fresh-or-degraded"
	InvGateNotHoldingRepair    = "gate-not-holding-repair"
	InvPendingRunPlaced        = "pending-run-placed"
	InvMergeReachesResultCheck = "merge-reaches-result-check"
	InvLeaderDecides           = "leader-decides"
)

// Invariants lists every invariant in the contract's order.
var Invariants = []string{
	InvWIPHasRunOrWait, InvWaitWithinDeadline, InvStageAgeUnderP90, InvRefusalNotRepeated,
	InvSourceFreshOrDegraded, InvGateNotHoldingRepair, InvPendingRunPlaced, InvMergeReachesResultCheck,
	InvLeaderDecides,
}

// Defaults used when the Installation leaves an interval unset (fixture values).
const (
	DefaultGrace       = 5 * time.Minute
	DefaultDeliveryLog = 5 * time.Minute
	DefaultBoardProbe  = 10 * time.Minute
	DefaultBoardResync = 10 * time.Minute
)

// Snapshot is what the leader knows when metrics are collected. The engine builds it from its informer
// cache and in-memory state; a replica that doesn't lead returns the zero Snapshot, and then nothing
// derived is exported.
type Snapshot struct {
	// Leader is true on the replica that leads.
	Leader bool
	// Now is the engine clock's time.
	Now time.Time
	// LeaderSince is when this replica started leading. It bounds the age of checks and decision passes
	// that haven't run yet on this leader, so a new leader isn't blamed for its predecessor.
	LeaderSince time.Time
	// Installation is the engine's Installation (spec intervals, status sources); nil before it exists.
	Installation *v1alpha1.Installation
	// Teams are the configured Teams (to label Items by team when they carry no team label).
	Teams []v1alpha1.Team
	// Items and Changes are every Item and Change in the namespace.
	Items   []v1alpha1.Item
	Changes []v1alpha1.Change
	// Verdicts are, by source (webhooks, board), when its freshness was last judged, kept in memory
	// (an idle tick writes no object). Every scheduled check renews it, including one that leaves or
	// makes the source degraded (failed, deferred by the budget, filter invalid); a transient retry
	// doesn't. status.sources[*].checkedAt is used when it is later.
	Verdicts map[string]time.Time
	// LastDecision is when the last decision pass of any team ran, kept in memory.
	// status.lastDecisionAt is used when it is later.
	LastDecision time.Time
	// RefusalRetries counts, by object ref (an Item's issue ref or a Change's PR ref), the reads made
	// on unchanged inputs after a refusal. status.refusal.retries counts too; the larger wins.
	RefusalRetries map[string]int
	// HeldExempt are gate-exempt items a gate holds, as the team pass saw them (issue refs). Held Items
	// that unblock in-flight work are also found from the Items themselves.
	HeldExempt []string
}

// Evaluate counts the violating objects of every invariant (0 when it holds). It is pure.
func Evaluate(s Snapshot) map[string]int {
	return map[string]int{
		InvWIPHasRunOrWait:         wipHasRunOrWait(s),
		InvWaitWithinDeadline:      waitWithinDeadline(s),
		InvStageAgeUnderP90:        0, // no engine WIP to age in phase 1
		InvRefusalNotRepeated:      refusalNotRepeated(s),
		InvSourceFreshOrDegraded:   sourceFreshOrDegraded(s),
		InvGateNotHoldingRepair:    gateNotHoldingRepair(s),
		InvPendingRunPlaced:        0, // no runs in phase 1
		InvMergeReachesResultCheck: mergeReachesResultCheck(s),
		InvLeaderDecides:           leaderDecides(s),
	}
}

// wipHasRunOrWait counts Ready implementable Items with an open blocker but no dependency wait, and
// Changes with CI, a review or a merge result pending but no wait. A Change counts once, not again for
// its Item. Objects whose read stands refused are left out: what can't be observed isn't expected.
func wipHasRunOrWait(s Snapshot) int {
	n := 0
	counted := map[string]bool{}
	for i := range s.Changes {
		c := &s.Changes[i]
		if changeLacksWait(c) {
			n++
			counted[changeRef(c)] = true
		}
	}
	for i := range s.Items {
		it := &s.Items[i]
		if !itemLacksWait(it) {
			continue
		}
		dup := false
		for _, ch := range it.Status.Changes {
			dup = dup || counted[ch]
		}
		if !dup {
			n++
		}
	}
	return n
}

func itemLacksWait(it *v1alpha1.Item) bool {
	st := &it.Status
	if !inScope(it) || st.LeftAt != nil || st.Refusal != nil || st.Class != v1alpha1.ClassImplementable ||
		st.Phase != v1alpha1.PhaseReady {
		return false
	}
	if st.Wait != nil && st.Wait.On == v1alpha1.WaitOnDependency {
		return false
	}
	for _, b := range st.BlockedBy {
		if b.State == v1alpha1.IssueOpen {
			return true
		}
	}
	return false
}

func changeLacksWait(c *v1alpha1.Change) bool {
	st := &c.Status
	if st.Wait != nil || st.Refusal != nil || st.PR == nil {
		return false
	}
	switch st.PR.State {
	case v1alpha1.PROpen:
		if st.CI == nil {
			return false
		}
		if st.CI.State == v1alpha1.CIPending {
			return true
		}
		ciDone := st.CI.State == v1alpha1.CIGreen || st.CI.State == v1alpha1.CINone
		return ciDone && !st.PR.Draft && reviewPending(st.Review)
	case v1alpha1.PRMerged:
		return st.MergeResult != nil && st.MergeResult.State == v1alpha1.CIPending
	}
	return false
}

// reviewPending is true when the PR awaits a review or another team's approval (not when changes
// were requested: then it is the author's turn).
func reviewPending(r *v1alpha1.ChangeReview) bool {
	if r == nil || r.State == v1alpha1.ReviewChangesRequested {
		return false
	}
	return r.NeedsOtherTeam || (r.State != v1alpha1.ReviewApproved && r.Required > 0)
}

// waitWithinDeadline counts waits past deadline plus grace whose deadline wasn't handled (an overdue
// wait was). Unobserved waits never count: the source's degraded state is what alerts.
func waitWithinDeadline(s Snapshot) int {
	grace := DefaultGrace
	if s.Installation != nil && s.Installation.Spec.Waits.Grace.Duration > 0 {
		grace = s.Installation.Spec.Waits.Grace.Duration
	}
	n := 0
	check := func(w *v1alpha1.Wait) {
		if w == nil || w.State == v1alpha1.WaitUnobserved || w.State == v1alpha1.WaitOverdue || w.DeadlineHandled ||
			w.Deadline.IsZero() {
			return
		}
		if s.Now.After(w.Deadline.Add(grace)) {
			n++
		}
	}
	for i := range s.Items {
		check(s.Items[i].Status.Wait)
	}
	for i := range s.Changes {
		check(s.Changes[i].Status.Wait)
	}
	return n
}

// refusalNotRepeated counts objects whose refused read was retried on unchanged inputs.
func refusalNotRepeated(s Snapshot) int {
	retries := map[string]int{}
	for ref, r := range s.RefusalRetries {
		retries[ref] = r
	}
	for i := range s.Items {
		if r := s.Items[i].Status.Refusal; r != nil {
			ref := itemRef(&s.Items[i])
			retries[ref] = max(retries[ref], r.Retries)
		}
	}
	for i := range s.Changes {
		if r := s.Changes[i].Status.Refusal; r != nil {
			ref := changeRef(&s.Changes[i])
			retries[ref] = max(retries[ref], r.Retries)
		}
	}
	n := 0
	for _, r := range retries {
		if r > 0 {
			n++
		}
	}
	return n
}

// sourceFreshOrDegraded counts sources whose freshness verdict is older than twice their check
// interval: a verdict nobody renews is no verdict, whatever it says.
func sourceFreshOrDegraded(s Snapshot) int {
	if s.Installation == nil {
		return 0
	}
	n := 0
	for _, src := range sourceNames(s) {
		verdict := s.Verdicts[src]
		if st, ok := s.Installation.Status.Sources[src]; ok && st.CheckedAt != nil {
			verdict = later(verdict, st.CheckedAt.Time)
		}
		verdict = later(verdict, s.LeaderSince)
		if verdict.IsZero() {
			continue
		}
		if s.Now.Sub(verdict) > 2*checkInterval(s.Installation, src) {
			n++
		}
	}
	return n
}

func checkInterval(inst *v1alpha1.Installation, src string) time.Duration {
	switch src {
	case v1alpha1.SourceWebhooks:
		if d := inst.Spec.Freshness.DeliveryLog.Duration; d > 0 {
			return d
		}
		return DefaultDeliveryLog
	default:
		if d := inst.Spec.Freshness.BoardProbe.Duration; d > 0 {
			return d
		}
		return DefaultBoardProbe
	}
}

// sourceNames are the known sources and any other the Installation reports, sorted.
func sourceNames(s Snapshot) []string {
	names := []string{v1alpha1.SourceBoard, v1alpha1.SourceWebhooks}
	if s.Installation != nil {
		for src := range s.Installation.Status.Sources {
			if src != v1alpha1.SourceBoard && src != v1alpha1.SourceWebhooks {
				names = append(names, src)
			}
		}
	}
	sort.Strings(names)
	return names
}

// inFlightPhases are the phases of in-flight work: Implementing through Validating.
var inFlightPhases = map[string]bool{
	v1alpha1.PhaseImplementing: true, v1alpha1.PhasePublished: true, v1alpha1.PhaseFixing: true,
	v1alpha1.PhaseMerging: true, v1alpha1.PhaseMerged: true, v1alpha1.PhaseWaiting: true,
	v1alpha1.PhaseValidating: true,
}

// gateNotHoldingRepair counts gate-exempt items held by a gate. In phase 1 exempt work is work that
// unblocks in-flight work: an Item an open blocker of an in-flight implementable Item.
func gateNotHoldingRepair(s Snapshot) int {
	unblocks := map[string]bool{}
	for i := range s.Items {
		st := &s.Items[i].Status
		if !inScope(&s.Items[i]) || st.LeftAt != nil || st.Class != v1alpha1.ClassImplementable || !inFlightPhases[st.Phase] {
			continue
		}
		for _, b := range st.BlockedBy {
			if b.State == v1alpha1.IssueOpen {
				unblocks[b.Ref] = true
			}
		}
	}
	held := map[string]bool{}
	for _, ref := range s.HeldExempt {
		held[ref] = true
	}
	for i := range s.Items {
		it := &s.Items[i]
		if sh := it.Status.Shadow; sh != nil && sh.HeldBy != nil && unblocks[itemRef(it)] {
			held[itemRef(it)] = true
		}
	}
	return len(held)
}

// mergeReachesResultCheck counts merges whose merge-result check is still missing after the
// deadline's re-observe of their wait. An unobserved wait doesn't count.
func mergeReachesResultCheck(s Snapshot) int {
	n := 0
	for i := range s.Changes {
		st := &s.Changes[i].Status
		if st.PR == nil || st.PR.State != v1alpha1.PRMerged {
			continue
		}
		if st.MergeResult != nil && st.MergeResult.State != v1alpha1.CIPending {
			continue
		}
		w := st.Wait
		if w == nil || w.On != v1alpha1.WaitOnCI || w.State == v1alpha1.WaitUnobserved {
			continue
		}
		if w.DeadlineHandled || w.State == v1alpha1.WaitOverdue {
			n++
		}
	}
	return n
}

// leaderDecides is 1 when the last decision pass is older than twice the board resync interval.
func leaderDecides(s Snapshot) int {
	last := later(lastDecision(s), s.LeaderSince)
	if last.IsZero() {
		return 0
	}
	interval := DefaultBoardResync
	if s.Installation != nil && s.Installation.Spec.Resync.Board.Duration > 0 {
		interval = s.Installation.Spec.Resync.Board.Duration
	}
	if s.Now.Sub(last) > 2*interval {
		return 1
	}
	return 0
}

// lastDecision is the later of the in-memory last decision pass and the Installation's.
func lastDecision(s Snapshot) time.Time {
	last := s.LastDecision
	if s.Installation != nil && s.Installation.Status.LastDecisionAt != nil {
		last = later(last, s.Installation.Status.LastDecisionAt.Time)
	}
	return last
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// inScope is false for Items read only for their links.
func inScope(it *v1alpha1.Item) bool {
	return it.Status.InScope == nil || *it.Status.InScope
}

// itemRef is an Item's issue ref (owner/repo#n), else its object name.
func itemRef(it *v1alpha1.Item) string {
	if is := it.Status.Issue; is != nil && is.Repo != "" && is.Number > 0 {
		return is.Repo + "#" + strconv.Itoa(is.Number)
	}
	return it.Name
}

// changeRef is a Change's PR ref (owner/repo#n), else its object name.
func changeRef(c *v1alpha1.Change) string {
	if pr := c.Status.PR; pr != nil && pr.Repo != "" && pr.Number > 0 {
		return pr.Repo + "#" + strconv.Itoa(pr.Number)
	}
	return c.Name
}
