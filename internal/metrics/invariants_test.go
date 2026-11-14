package metrics

import (
	"maps"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
)

var start = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

func at(hhmm string) time.Time {
	t, err := time.Parse("2006-01-02T15:04", "2026-11-16T"+hhmm)
	if err != nil {
		panic(err)
	}
	return t
}

func mt(t time.Time) metav1.Time   { return metav1.NewTime(t) }
func pmt(t time.Time) *metav1.Time { m := metav1.NewTime(t); return &m }
func dur(d time.Duration) metav1.Duration {
	return metav1.Duration{Duration: d}
}

// installation is the base fixture's Installation, both sources fresh.
func installation() *v1alpha1.Installation {
	inst := &v1alpha1.Installation{}
	inst.Name = "giantswarm"
	inst.Spec.Resync = v1alpha1.ResyncSpec{Board: dur(10 * time.Minute), Changes: dur(15 * time.Minute)}
	inst.Spec.Freshness = v1alpha1.FreshnessSpec{DeliveryLog: dur(5 * time.Minute), BoardProbe: dur(10 * time.Minute)}
	inst.Spec.Waits.Grace = dur(5 * time.Minute)
	inst.Status.Sources = map[string]v1alpha1.SourceStatus{
		v1alpha1.SourceWebhooks: {State: v1alpha1.SourceFresh},
		v1alpha1.SourceBoard:    {State: v1alpha1.SourceFresh},
	}
	return inst
}

// healthy is a leader's snapshot at now with fresh verdicts and a decision pass at now.
func healthy(now time.Time) Snapshot {
	return Snapshot{
		Leader:       true,
		Now:          now,
		LeaderSince:  start,
		Installation: installation(),
		Verdicts:     map[string]time.Time{v1alpha1.SourceWebhooks: now, v1alpha1.SourceBoard: now},
		LastDecision: now,
	}
}

func item(ref, phase string, opts ...func(*v1alpha1.Item)) v1alpha1.Item {
	r, err := v1alpha1.ParseRef(ref)
	if err != nil {
		panic(err)
	}
	it := v1alpha1.Item{}
	it.Name = v1alpha1.ItemName(ref)
	it.Status = v1alpha1.ItemStatus{
		Issue: &v1alpha1.ItemIssue{Repo: r.FullRepo(), Number: r.Number, State: v1alpha1.IssueOpen},
		Board: &v1alpha1.ItemBoard{Team: "Bumblebee🐝"},
		Class: v1alpha1.ClassImplementable,
		Phase: phase,
	}
	for _, o := range opts {
		o(&it)
	}
	return it
}

func blockedBy(refs ...string) func(*v1alpha1.Item) {
	return func(it *v1alpha1.Item) {
		for _, r := range refs {
			it.Status.BlockedBy = append(it.Status.BlockedBy, v1alpha1.Blocker{Ref: r, State: v1alpha1.IssueOpen})
		}
	}
}

func heldBy(person, gate string) func(*v1alpha1.Item) {
	return func(it *v1alpha1.Item) {
		it.Status.Shadow = &v1alpha1.Shadow{HeldBy: &v1alpha1.ShadowHeld{Person: person, Gate: gate}}
	}
}

func itemWait(w *v1alpha1.Wait) func(*v1alpha1.Item) {
	return func(it *v1alpha1.Item) { it.Status.Wait = w }
}

func withChanges(refs ...string) func(*v1alpha1.Item) {
	return func(it *v1alpha1.Item) { it.Status.Changes = refs }
}

func change(ref, state string, opts ...func(*v1alpha1.Change)) v1alpha1.Change {
	r, err := v1alpha1.ParseRef(ref)
	if err != nil {
		panic(err)
	}
	c := v1alpha1.Change{}
	c.Name = v1alpha1.ChangeName(ref)
	c.Status.PR = &v1alpha1.ChangePR{Repo: r.FullRepo(), Number: r.Number, State: state, HeadSHA: "a1"}
	for _, o := range opts {
		o(&c)
	}
	return c
}

func ci(state string) func(*v1alpha1.Change) {
	return func(c *v1alpha1.Change) { c.Status.CI = &v1alpha1.ChangeCI{SHA: c.Status.PR.HeadSHA, State: state} }
}

func review(state string, required int) func(*v1alpha1.Change) {
	return func(c *v1alpha1.Change) { c.Status.Review = &v1alpha1.ChangeReview{State: state, Required: required} }
}

func merged(sha, result string) func(*v1alpha1.Change) {
	return func(c *v1alpha1.Change) {
		c.Status.PR.State = v1alpha1.PRMerged
		c.Status.PR.MergeSHA = sha
		c.Status.MergeResult = &v1alpha1.ChangeCI{SHA: sha, State: result}
	}
}

func changeWait(w *v1alpha1.Wait) func(*v1alpha1.Change) {
	return func(c *v1alpha1.Change) { c.Status.Wait = w }
}

func refused(retries int) func(*v1alpha1.Change) {
	return func(c *v1alpha1.Change) {
		c.Status.Refusal = &v1alpha1.Refusal{Operation: "pulls/get", Code: 404, Since: mt(at("09:15")), Retries: retries}
	}
}

func wait(on, ref, state string, deadline time.Time, handled bool) *v1alpha1.Wait {
	return &v1alpha1.Wait{
		On: on, Ref: ref, State: state, Since: mt(start), Deadline: mt(deadline),
		AtDeadline: v1alpha1.AtDeadlineReobserve, DeadlineHandled: handled,
	}
}

func TestEvaluate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		snap func() Snapshot
		want map[string]int // invariants not named must be 0
	}{
		{
			name: "invariant-all-hold-in-steady-state",
			snap: func() Snapshot {
				s := healthy(at("09:30"))
				s.Items = []v1alpha1.Item{
					item("giantswarm/roadmap#101", v1alpha1.PhasePublished, withChanges("giantswarm/klaus#55")),
					item("giantswarm/roadmap#102", v1alpha1.PhaseMerged, withChanges("giantswarm/klaus#56")),
					item("giantswarm/roadmap#103", v1alpha1.PhaseImplementing),
					item("giantswarm/roadmap#104", v1alpha1.PhaseReady),
					item("giantswarm/roadmap#105", v1alpha1.PhaseReady, heldBy("bob", v1alpha1.GateTeamCap)),
				}
				s.Changes = []v1alpha1.Change{
					change("giantswarm/klaus#55", v1alpha1.PROpen, ci(v1alpha1.CIPending), review(v1alpha1.ReviewNone, 1),
						changeWait(wait(v1alpha1.WaitOnCI, "ci:giantswarm/klaus@a1", v1alpha1.WaitWatching, at("10:00"), false))),
					change("giantswarm/klaus#56", v1alpha1.PROpen, ci(v1alpha1.CIGreen), merged("m1", v1alpha1.CIPending),
						changeWait(wait(v1alpha1.WaitOnCI, "ci:giantswarm/klaus@m1", v1alpha1.WaitWatching, at("11:00"), false))),
				}
				return s
			},
		},
		{
			name: "invariant-gate-holding-exempt-work-detected",
			snap: func() Snapshot {
				s := healthy(at("09:00"))
				s.Items = []v1alpha1.Item{
					item("giantswarm/roadmap#101", v1alpha1.PhaseImplementing, blockedBy("giantswarm/roadmap#102")),
					item("giantswarm/roadmap#102", v1alpha1.PhaseReady, heldBy("alice", v1alpha1.GateTeamCap)),
				}
				return s
			},
			want: map[string]int{InvGateNotHoldingRepair: 1},
		},
		{
			name: "gate-unblocking-work-exempt (picked, not held)",
			snap: func() Snapshot {
				s := healthy(at("09:00"))
				s.Items = []v1alpha1.Item{
					item("giantswarm/roadmap#100", v1alpha1.PhaseImplementing, blockedBy("giantswarm/roadmap#120")),
					item("giantswarm/roadmap#120", v1alpha1.PhaseReady),
					item("giantswarm/roadmap#101", v1alpha1.PhaseReady),
				}
				return s
			},
		},
		{
			name: "held item that unblocks nothing in flight is no violation",
			snap: func() Snapshot {
				s := healthy(at("09:00"))
				s.Items = []v1alpha1.Item{
					item("giantswarm/roadmap#101", v1alpha1.PhaseReady, blockedBy("giantswarm/roadmap#102")),
					item("giantswarm/roadmap#102", v1alpha1.PhaseReady, heldBy("alice", v1alpha1.GateRepoConcurrency)),
				}
				s.Items[0].Status.Wait = wait(v1alpha1.WaitOnDependency, "issue:giantswarm/roadmap#102", v1alpha1.WaitWatching, at("09:00").Add(168*time.Hour), false)
				return s
			},
		},
		{
			name: "held exempt items reported by the team pass count once with those found",
			snap: func() Snapshot {
				s := healthy(at("09:00"))
				s.Items = []v1alpha1.Item{
					item("giantswarm/roadmap#101", v1alpha1.PhaseImplementing, blockedBy("giantswarm/roadmap#102")),
					item("giantswarm/roadmap#102", v1alpha1.PhaseReady, heldBy("alice", v1alpha1.GateTeamCap)),
				}
				s.HeldExempt = []string{"giantswarm/roadmap#102", "giantswarm/roadmap#103"}
				return s
			},
			want: map[string]int{InvGateNotHoldingRepair: 2},
		},
		{
			name: "invariant-leader-stops-deciding-detected",
			snap: func() Snapshot {
				s := healthy(at("09:21"))
				s.LastDecision = at("09:00")
				return s
			},
			want: map[string]int{InvLeaderDecides: 1},
		},
		{
			name: "leader decided within two board resync intervals",
			snap: func() Snapshot {
				s := healthy(at("09:20"))
				s.LastDecision = at("09:00")
				return s
			},
		},
		{
			name: "last decision on the Installation is used when later",
			snap: func() Snapshot {
				s := healthy(at("09:21"))
				s.LastDecision = time.Time{}
				s.Installation.Status.LastDecisionAt = pmt(at("09:10"))
				return s
			},
		},
		{
			name: "a new leader is not blamed for its predecessor",
			snap: func() Snapshot {
				s := healthy(at("10:00"))
				s.LeaderSince = at("09:50")
				s.LastDecision = at("09:00")
				return s
			},
		},
		{
			name: "invariant-overnight-stall-alerts-within-minutes (healthy at 21:00)",
			snap: func() Snapshot {
				s := healthy(at("21:00"))
				s.LeaderSince = at("20:00")
				return s
			},
		},
		{
			name: "invariant-overnight-stall-alerts-within-minutes (alerting by 21:21)",
			snap: func() Snapshot {
				s := healthy(at("21:21"))
				s.LeaderSince = at("20:00")
				s.LastDecision = at("21:00")
				return s
			},
			want: map[string]int{InvLeaderDecides: 1},
		},
		{
			name: "invariant-pending-ci-without-wait-detected (holds before the PR)",
			snap: func() Snapshot {
				s := healthy(at("09:00"))
				s.Items = []v1alpha1.Item{item("giantswarm/roadmap#101", v1alpha1.PhaseImplementing)}
				return s
			},
		},
		{
			name: "invariant-pending-ci-without-wait-detected",
			snap: func() Snapshot {
				s := healthy(at("09:00"))
				s.Items = []v1alpha1.Item{item("giantswarm/roadmap#101", v1alpha1.PhasePublished, withChanges("giantswarm/klaus#55"))}
				s.Changes = []v1alpha1.Change{change("giantswarm/klaus#55", v1alpha1.PROpen, ci(v1alpha1.CIPending))}
				return s
			},
			want: map[string]int{InvWIPHasRunOrWait: 1},
		},
		{
			name: "a Change missing its wait counts once, not again for its Item",
			snap: func() Snapshot {
				s := healthy(at("09:00"))
				s.Items = []v1alpha1.Item{item("giantswarm/roadmap#101", v1alpha1.PhaseReady,
					blockedBy("giantswarm/roadmap#100"), withChanges("giantswarm/klaus#55"))}
				s.Changes = []v1alpha1.Change{change("giantswarm/klaus#55", v1alpha1.PROpen, ci(v1alpha1.CIPending))}
				return s
			},
			want: map[string]int{InvWIPHasRunOrWait: 1},
		},
		{
			name: "Ready with an open blocker and no dependency wait",
			snap: func() Snapshot {
				s := healthy(at("09:00"))
				s.Items = []v1alpha1.Item{
					item("giantswarm/roadmap#102", v1alpha1.PhaseReady, blockedBy("giantswarm/roadmap#100")),
					item("giantswarm/roadmap#103", v1alpha1.PhaseReady, blockedBy("giantswarm/roadmap#100"),
						itemWait(wait(v1alpha1.WaitOnDependency, "issue:giantswarm/roadmap#100", v1alpha1.WaitWatching, at("09:00").Add(168*time.Hour), false))),
				}
				return s
			},
			want: map[string]int{InvWIPHasRunOrWait: 1},
		},
		{
			name: "pending review without a wait; changes requested and approved need none",
			snap: func() Snapshot {
				s := healthy(at("09:00"))
				s.Changes = []v1alpha1.Change{
					change("giantswarm/klaus#55", v1alpha1.PROpen, ci(v1alpha1.CIGreen), review(v1alpha1.ReviewNone, 1)),
					change("giantswarm/klaus#56", v1alpha1.PROpen, ci(v1alpha1.CIGreen), review(v1alpha1.ReviewChangesRequested, 1)),
					change("giantswarm/klaus#57", v1alpha1.PROpen, ci(v1alpha1.CIGreen), review(v1alpha1.ReviewApproved, 1)),
					change("giantswarm/klaus#58", v1alpha1.PROpen, ci(v1alpha1.CIRed), review(v1alpha1.ReviewNone, 1)),
				}
				return s
			},
			want: map[string]int{InvWIPHasRunOrWait: 1},
		},
		{
			name: "merged with the merge result pending and no wait",
			snap: func() Snapshot {
				s := healthy(at("09:00"))
				s.Changes = []v1alpha1.Change{
					change("giantswarm/muster#12", v1alpha1.PROpen, ci(v1alpha1.CIGreen), merged("m1", v1alpha1.CIPending)),
					change("giantswarm/muster#13", v1alpha1.PROpen, ci(v1alpha1.CIRed), merged("m2", v1alpha1.CIRed)),
				}
				return s
			},
			want: map[string]int{InvWIPHasRunOrWait: 1},
		},
		{
			name: "invariant-repeated-refusal-detected",
			snap: func() Snapshot {
				s := healthy(at("09:30"))
				s.Changes = []v1alpha1.Change{change("giantswarm/klaus#55", v1alpha1.PROpen, ci(v1alpha1.CIPending), refused(1))}
				return s
			},
			want: map[string]int{InvRefusalNotRepeated: 1},
		},
		{
			name: "refusal retries kept in memory count once per object",
			snap: func() Snapshot {
				s := healthy(at("09:30"))
				s.Changes = []v1alpha1.Change{change("giantswarm/klaus#55", v1alpha1.PROpen, refused(1))}
				s.RefusalRetries = map[string]int{"giantswarm/klaus#55": 2, "giantswarm/roadmap#101": 1, "giantswarm/roadmap#102": 0}
				return s
			},
			want: map[string]int{InvRefusalNotRepeated: 2},
		},
		{
			name: "budget-refusal-not-retried-until-inputs-change",
			snap: func() Snapshot {
				s := healthy(at("10:00"))
				s.Changes = []v1alpha1.Change{change("giantswarm/happa#7", v1alpha1.PROpen, refused(0))}
				s.Changes[0].Status.PR = nil
				return s
			},
		},
		{
			name: "invariant-source-without-verdict-detected",
			snap: func() Snapshot {
				s := healthy(at("09:31"))
				s.Verdicts = map[string]time.Time{v1alpha1.SourceWebhooks: at("09:10"), v1alpha1.SourceBoard: at("09:10")}
				s.LastDecision = at("09:30")
				return s
			},
			want: map[string]int{InvSourceFreshOrDegraded: 2},
		},
		{
			name: "source verdicts each judged against their own interval",
			snap: func() Snapshot {
				s := healthy(at("09:25"))
				s.Verdicts = map[string]time.Time{v1alpha1.SourceWebhooks: at("09:10"), v1alpha1.SourceBoard: at("09:10")}
				return s
			},
			want: map[string]int{InvSourceFreshOrDegraded: 1},
		},
		{
			name: "a renewed degraded verdict holds; checkedAt counts when later",
			snap: func() Snapshot {
				s := healthy(at("09:40"))
				s.Verdicts = map[string]time.Time{v1alpha1.SourceWebhooks: at("09:35")}
				s.Installation.Status.Sources[v1alpha1.SourceWebhooks] = v1alpha1.SourceStatus{
					State: v1alpha1.SourceDegraded, Reason: v1alpha1.SourceReasonLogUnreadable, Polling: true,
				}
				s.Installation.Status.Sources[v1alpha1.SourceBoard] = v1alpha1.SourceStatus{
					State: v1alpha1.SourceFresh, CheckedAt: pmt(at("09:30")),
				}
				return s
			},
		},
		{
			name: "invariant-wait-past-deadline-detected",
			snap: func() Snapshot {
				s := healthy(at("10:10"))
				s.Changes = []v1alpha1.Change{change("giantswarm/klaus#55", v1alpha1.PROpen, ci(v1alpha1.CIPending),
					changeWait(wait(v1alpha1.WaitOnCI, "ci:giantswarm/klaus@a1", v1alpha1.WaitWatching, at("10:00"), false)))}
				return s
			},
			want: map[string]int{InvWaitWithinDeadline: 1},
		},
		{
			name: "invariant-wait-past-deadline-detected (handled after resume)",
			snap: func() Snapshot {
				s := healthy(at("10:10"))
				s.Changes = []v1alpha1.Change{change("giantswarm/klaus#55", v1alpha1.PROpen, ci(v1alpha1.CIPending),
					changeWait(wait(v1alpha1.WaitOnCI, "ci:giantswarm/klaus@a1", v1alpha1.WaitOverdue, at("10:00"), true)))}
				return s
			},
		},
		{
			name: "overdue without the handled flag is handled",
			snap: func() Snapshot {
				s := healthy(at("10:30"))
				s.Changes = []v1alpha1.Change{change("giantswarm/klaus#55", v1alpha1.PROpen, ci(v1alpha1.CIPending),
					changeWait(wait(v1alpha1.WaitOnCI, "ci:giantswarm/klaus@a1", v1alpha1.WaitOverdue, at("10:00"), false)))}
				return s
			},
		},
		{
			name: "a Ready item that isn't implementable needs no dependency wait",
			snap: func() Snapshot {
				s := healthy(at("09:00"))
				s.Items = []v1alpha1.Item{item("giantswarm/roadmap#102", v1alpha1.PhaseReady, blockedBy("giantswarm/roadmap#100"),
					func(it *v1alpha1.Item) { it.Status.Class = v1alpha1.ClassIgnored })}
				return s
			},
		},
		{
			name: "wait within its grace",
			snap: func() Snapshot {
				s := healthy(at("10:05"))
				s.Items = []v1alpha1.Item{item("giantswarm/roadmap#102", v1alpha1.PhaseReady, blockedBy("giantswarm/roadmap#100"),
					itemWait(wait(v1alpha1.WaitOnDependency, "issue:giantswarm/roadmap#100", v1alpha1.WaitWatching, at("10:00"), false)))}
				return s
			},
		},
		{
			name: "wait-unobserved-when-reobserve-fails (unobserved is not stalled)",
			snap: func() Snapshot {
				s := healthy(at("10:10"))
				w := wait(v1alpha1.WaitOnCI, "ci:giantswarm/klaus@a1", v1alpha1.WaitUnobserved, at("10:00"), false)
				w.Reason = "http-502"
				s.Changes = []v1alpha1.Change{change("giantswarm/klaus#55", v1alpha1.PROpen, ci(v1alpha1.CIPending), changeWait(w))}
				return s
			},
		},
		{
			name: "wait-merge-result-never-reported-violates-invariant (before the deadline)",
			snap: func() Snapshot {
				s := healthy(at("10:59"))
				s.Changes = []v1alpha1.Change{change("giantswarm/muster#12", v1alpha1.PROpen, ci(v1alpha1.CIGreen), merged("m1", v1alpha1.CIPending),
					changeWait(wait(v1alpha1.WaitOnCI, "ci:giantswarm/muster@m1", v1alpha1.WaitWatching, at("11:00"), false)))}
				return s
			},
		},
		{
			name: "wait-merge-result-never-reported-violates-invariant",
			snap: func() Snapshot {
				s := healthy(at("11:00"))
				s.Changes = []v1alpha1.Change{change("giantswarm/muster#12", v1alpha1.PROpen, ci(v1alpha1.CIGreen), merged("m1", v1alpha1.CIPending),
					changeWait(wait(v1alpha1.WaitOnCI, "ci:giantswarm/muster@m1", v1alpha1.WaitOverdue, at("11:00"), true)))}
				return s
			},
			want: map[string]int{InvMergeReachesResultCheck: 1},
		},
		{
			name: "wait-merge-result-watched-after-hand-merge (result red)",
			snap: func() Snapshot {
				s := healthy(at("09:00"))
				s.Items = []v1alpha1.Item{item("giantswarm/roadmap#101", v1alpha1.PhaseMerged, withChanges("giantswarm/muster#12"))}
				s.Changes = []v1alpha1.Change{change("giantswarm/muster#12", v1alpha1.PROpen, ci(v1alpha1.CIRed), merged("m1", v1alpha1.CIRed))}
				return s
			},
		},
		{
			name: "merge result missing after the re-observe but unobserved",
			snap: func() Snapshot {
				s := healthy(at("11:30"))
				w := wait(v1alpha1.WaitOnCI, "ci:giantswarm/muster@m1", v1alpha1.WaitUnobserved, at("11:00"), true)
				w.Reason = v1alpha1.WaitReasonSourceDegraded
				s.Changes = []v1alpha1.Change{change("giantswarm/muster#12", v1alpha1.PROpen, ci(v1alpha1.CIGreen), merged("m1", v1alpha1.CIPending), changeWait(w))}
				return s
			},
		},
		{
			name: "zero snapshot",
			snap: func() Snapshot { return Snapshot{} },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			want := map[string]int{}
			for _, inv := range Invariants {
				want[inv] = 0
			}
			maps.Copy(want, tt.want)
			got := Evaluate(tt.snap())
			if !maps.Equal(got, want) {
				t.Errorf("Evaluate() = %v, want %v", got, want)
			}
		})
	}
}
