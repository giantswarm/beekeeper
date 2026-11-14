package decide

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/forge"
	"github.com/giantswarm/beekeeper/internal/history"
)

const (
	goTest  = "ci/circleci: go-test"
	goBuild = "ci/circleci: go-build"
)

func check(name, status, conclusion string) v1alpha1.CheckFact {
	return v1alpha1.CheckFact{Kind: "check", Name: name, Status: status, Conclusion: conclusion}
}

func cstatus(name, state string) v1alpha1.CheckFact {
	f := v1alpha1.CheckFact{Kind: "status", Name: name, Status: checkCompleted}
	switch state {
	case "pending":
		f.Status = "in_progress"
	case "success":
		f.Conclusion = "success"
	default:
		f.Conclusion = "failure"
	}
	return f
}

func running(name string) v1alpha1.CheckFact { return check(name, "in_progress", "") }
func passed(name string) v1alpha1.CheckFact  { return check(name, "completed", "success") }
func failed(name string) v1alpha1.CheckFact  { return check(name, "completed", "failure") }

// pr is a Change as observed: klaus#55 (required go-test and go-build, one approval) or muster#12
// (required go-test), closing roadmap#101.
func pr(repo string, number int, head string, checks ...v1alpha1.CheckFact) *v1alpha1.Change {
	c := &v1alpha1.Change{}
	c.Name = "change"
	required := []string{goTest, goBuild}
	if repo == "giantswarm/muster" {
		required = []string{goTest}
	}
	c.Status.PR = &v1alpha1.ChangePR{Repo: repo, Number: number, State: v1alpha1.PROpen, Author: alice, HeadSHA: head, Base: "main"}
	c.Status.Items = []string{refRoadmap101}
	c.Status.Facts = &v1alpha1.ChangeFacts{
		HeadSHA: head, Head: checks, RequiredChecks: required, RequiredApprovals: 1, OwnerTeam: "team-bumblebee",
	}
	return c
}

// changeSim feeds a Change through decide.Change step by step, as its reconciler would.
type changeSim struct {
	t         *testing.T
	spec      *v1alpha1.InstallationSpec
	c         *v1alpha1.Change
	durations []time.Duration
	dropWaits bool
	rows      []history.Event
}

func (s *changeSim) step(now time.Time, trigger Trigger, re *Reobserve, observe func(st *v1alpha1.ChangeStatus)) ChangeDecision {
	s.t.Helper()
	c := s.c.DeepCopy()
	if observe != nil {
		observe(&c.Status)
	}
	src := ""
	switch trigger {
	case TriggerWebhook:
		src = v1alpha1.SourceWebhook
	case TriggerResync:
		src = v1alpha1.SourceResync
	case TriggerStartup:
		src = v1alpha1.SourceStartup
	}
	d := Change(ChangeView{Now: now, Spec: s.spec, Change: c, CIDurations: s.durations, Trigger: trigger, Source: src, Reobserve: re, DropWaits: s.dropWaits})
	c.Status = d.Status
	s.c = c
	s.rows = append(s.rows, d.Events...)
	return d
}

func newSim(t *testing.T, c *v1alpha1.Change) *changeSim {
	return &changeSim{t: t, spec: baseSpec(), c: c}
}

func setHead(checks ...v1alpha1.CheckFact) func(*v1alpha1.ChangeStatus) {
	return func(st *v1alpha1.ChangeStatus) { st.Facts.Head = checks }
}

func push(head string) func(*v1alpha1.ChangeStatus) {
	return func(st *v1alpha1.ChangeStatus) {
		st.PR.HeadSHA, st.Facts.HeadSHA, st.Facts.Head = head, head, nil
	}
}

func reviewBy(by, state string, teams ...string) func(*v1alpha1.ChangeStatus) {
	return func(st *v1alpha1.ChangeStatus) {
		st.Facts.Reviews = append(st.Facts.Reviews, v1alpha1.ReviewFact{By: by, State: state, Teams: teams})
	}
}

func merge(sha string) func(*v1alpha1.ChangeStatus) {
	return func(st *v1alpha1.ChangeStatus) {
		st.PR.State, st.PR.MergeSHA, st.Facts.MergeSHA = v1alpha1.PRMerged, sha, sha
	}
}

func setMerge(checks ...v1alpha1.CheckFact) func(*v1alpha1.ChangeStatus) {
	return func(st *v1alpha1.ChangeStatus) { st.Facts.Merge = checks }
}

func waitIs(t *testing.T, w *v1alpha1.Wait, on, ref string, since, deadline time.Time, state string) {
	t.Helper()
	if w == nil {
		t.Fatalf("no wait, want %s on %s", on, ref)
	}
	eq(t, "wait.on", w.On, on)
	eq(t, "wait.ref", w.Ref, ref)
	eq(t, "wait.since", w.Since.Time, since)
	eq(t, "wait.deadline", w.Deadline.Time, deadline)
	eq(t, "wait.atDeadline", w.AtDeadline, v1alpha1.AtDeadlineReobserve)
	eq(t, "wait.state", w.State, state)
}

// TestChangeScenarios ports the wait scenarios of a Change, one case per scenario.
func TestChangeScenarios(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"wait-ci-wakes-on-check-run", func(t *testing.T) {
			s := newSim(t, pr("giantswarm/klaus", 55, "a1", passed(goBuild), running(goTest)))
			d := s.step(t0, TriggerStartup, nil, nil)
			eq(t, "ci", d.Status.CI.State, v1alpha1.CIPending)
			waitIs(t, d.Status.Wait, "ci", "ci:giantswarm/klaus@a1", t0, at(time.Hour), "watching")
			eq(t, "next check", *d.NextCheck, at(time.Hour))
			eq(t, "wakes items", d.WakeItems, []string{refRoadmap101})
			d = s.step(at(time.Minute), TriggerWebhook, nil, setHead(passed(goBuild), passed(goTest)))
			eq(t, "ci", d.Status.CI.State, v1alpha1.CIGreen)
			eq(t, "wait.on", d.Status.Wait.On, v1alpha1.WaitOnReview)
			w := eventsOf(d.Events, EventWaitWoken)
			eq(t, "woken", len(w), 1)
			eq(t, "woken source", w[0].Source, v1alpha1.SourceWebhook)
			eq(t, "woken on", w[0].Data["on"], "ci")
			eq(t, "woken change", w[0].Change, "giantswarm/klaus#55")
			eq(t, "ended", len(eventsOf(d.Events, EventWaitEnded)), 1)
			eq(t, "started", len(eventsOf(d.Events, EventWaitStarted)), 1)
			eq(t, "wakes items", d.WakeItems, []string{refRoadmap101})
		}},
		{"wait-ci-wakes-on-commit-status", func(t *testing.T) {
			s := newSim(t, pr("giantswarm/klaus", 55, "a1", cstatus(goBuild, "success"), cstatus(goTest, "pending")))
			d := s.step(t0, TriggerStartup, nil, nil)
			waitIs(t, d.Status.Wait, "ci", "ci:giantswarm/klaus@a1", t0, at(time.Hour), "watching")
			d = s.step(at(time.Minute), TriggerWebhook, nil, setHead(cstatus(goBuild, "success"), cstatus(goTest, "success")))
			eq(t, "ci", d.Status.CI.State, v1alpha1.CIGreen)
			eq(t, "wait.on", d.Status.Wait.On, v1alpha1.WaitOnReview)
			eq(t, "woken", len(eventsOf(d.Events, EventWaitWoken)), 1)
		}},
		{"wait-ci-deadline-from-observed-durations", func(t *testing.T) {
			s := newSim(t, pr("giantswarm/klaus", 55, "a1", running(goBuild), running(goTest)))
			s.durations = []time.Duration{20 * time.Minute, 25 * time.Minute, 30 * time.Minute}
			d := s.step(t0, TriggerStartup, nil, nil)
			waitIs(t, d.Status.Wait, "ci", "ci:giantswarm/klaus@a1", t0, at(90*time.Minute), "watching")
			d = s.step(at(89*time.Minute), TriggerResync, nil, nil)
			eq(t, "not yet", len(eventsOf(d.Events, EventWaitDeadline)), 0)
			d = s.step(at(90*time.Minute), TriggerDeadline, &Reobserve{Made: true}, nil)
			dl := eventsOf(d.Events, EventWaitDeadline)
			eq(t, "deadline rows", len(dl), 1)
			eq(t, "deadline at", dl[0].At, at(90*time.Minute))
		}},
		{"wait-record-has-deadline-and-action", func(t *testing.T) {
			s := newSim(t, pr("giantswarm/klaus", 55, "a1"))
			d := s.step(at(7*time.Minute), TriggerWebhook, nil, nil)
			eq(t, "ci sha", d.Status.CI.SHA, "a1")
			eq(t, "ci", d.Status.CI.State, v1alpha1.CIPending)
			waitIs(t, d.Status.Wait, "ci", "ci:giantswarm/klaus@a1", at(7*time.Minute), at(67*time.Minute), "watching")
			st := eventsOf(d.Events, EventWaitStarted)
			eq(t, "started", len(st), 1)
			eq(t, "started at", st[0].At, at(7*time.Minute))
			eq(t, "started source", st[0].Source, v1alpha1.SourceWebhook)
			eq(t, "started on", st[0].Data["on"], "ci")
		}},
		{"wait-red-ci-then-push-waits-on-new-sha", func(t *testing.T) {
			s := newSim(t, pr("giantswarm/klaus", 55, "a1", running(goBuild), running(goTest)))
			s.step(t0, TriggerStartup, nil, nil)
			d := s.step(at(10*time.Minute), TriggerWebhook, nil, setHead(running(goBuild), failed(goTest)))
			eq(t, "ci", d.Status.CI.State, v1alpha1.CIRed)
			eq(t, "no wait", d.Status.Wait == nil, true)
			eq(t, "no check", d.NextCheck == nil, true)
			d = s.step(at(30*time.Minute), TriggerWebhook, nil, push("b2"))
			eq(t, "head", d.Status.PR.HeadSHA, "b2")
			eq(t, "ci sha", d.Status.CI.SHA, "b2")
			eq(t, "ci", d.Status.CI.State, v1alpha1.CIPending)
			waitIs(t, d.Status.Wait, "ci", "ci:giantswarm/klaus@b2", at(30*time.Minute), at(90*time.Minute), "watching")
		}},
		{"wait-review-wakes-on-approval", func(t *testing.T) {
			s := newSim(t, pr("giantswarm/klaus", 55, "a1", passed(goBuild), passed(goTest)))
			d := s.step(t0, TriggerStartup, nil, nil)
			eq(t, "ci", d.Status.CI.State, v1alpha1.CIGreen)
			eq(t, "needsOtherTeam", d.Status.Review.NeedsOtherTeam, false)
			waitIs(t, d.Status.Wait, "review", "pr:giantswarm/klaus#55", t0, time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC), "watching")
			d = s.step(at(time.Minute), TriggerWebhook, nil, reviewBy(bob, "APPROVED", "team-bumblebee"))
			eq(t, "review", d.Status.Review.State, v1alpha1.ReviewApproved)
			eq(t, "no wait", d.Status.Wait == nil, true)
			w := eventsOf(d.Events, EventWaitWoken)
			eq(t, "woken", len(w), 1)
			eq(t, "woken on", w[0].Data["on"], "review")
			eq(t, "woken source", w[0].Source, v1alpha1.SourceWebhook)
		}},
		{"wait-changes-requested-means-fixing", func(t *testing.T) {
			s := newSim(t, pr("giantswarm/klaus", 55, "a1", passed(goBuild), passed(goTest)))
			s.step(t0, TriggerStartup, nil, nil)
			d := s.step(at(30*time.Minute), TriggerWebhook, nil, reviewBy(bob, "CHANGES_REQUESTED"))
			eq(t, "review", d.Status.Review.State, v1alpha1.ReviewChangesRequested)
			eq(t, "no wait", d.Status.Wait == nil, true)
			d = s.step(at(time.Hour), TriggerWebhook, nil, push("b2"))
			eq(t, "ci", d.Status.CI.State, v1alpha1.CIPending)
			eq(t, "review", d.Status.Review.State, v1alpha1.ReviewChangesRequested)
			waitIs(t, d.Status.Wait, "ci", "ci:giantswarm/klaus@b2", at(time.Hour), at(2*time.Hour), "watching")
			st := eventsOf(d.Events, EventWaitStarted)
			eq(t, "started", len(st), 1)
			eq(t, "started ref", st[0].Data["ref"], "ci:giantswarm/klaus@b2")
			eq(t, "started source", st[0].Source, v1alpha1.SourceWebhook)
		}},
		{"wait-merge-result-observed", func(t *testing.T) {
			c := pr("giantswarm/muster", 12, "a1", cstatus(goTest, "success"))
			c.Status.Facts.Reviews = []v1alpha1.ReviewFact{{By: bob, State: reviewApproved}}
			s := newSim(t, c)
			d := s.step(t0, TriggerStartup, nil, nil)
			eq(t, "ci", d.Status.CI.State, v1alpha1.CIGreen)
			eq(t, "review", d.Status.Review.State, v1alpha1.ReviewApproved)
			eq(t, "nothing pending", d.Status.Wait == nil, true)
			d = s.step(t0, TriggerWebhook, nil, merge("m1"))
			eq(t, "pr", d.Status.PR.State, v1alpha1.PRMerged)
			eq(t, "ci keeps head", *d.Status.CI, v1alpha1.ChangeCI{SHA: "a1", State: v1alpha1.CIGreen, Relevant: []string{goTest}})
			eq(t, "merge result", d.Status.MergeResult.SHA+"/"+d.Status.MergeResult.State, "m1/pending")
			waitIs(t, d.Status.Wait, "ci", "ci:giantswarm/muster@m1", t0, at(2*time.Hour), "watching")
			d = s.step(at(20*time.Minute), TriggerWebhook, nil, setMerge(cstatus(goTest, "success")))
			eq(t, "merge result", d.Status.MergeResult.State, v1alpha1.CIGreen)
			eq(t, "relevant", d.Status.MergeResult.Relevant, []string{goTest})
			eq(t, "no wait", d.Status.Wait == nil, true)
			e := eventsOf(d.Events, EventWaitEnded)
			eq(t, "ended", len(e), 1)
			eq(t, "ended source", e[0].Source, v1alpha1.SourceWebhook)
			eq(t, "ended on", e[0].Data["on"], "ci")
		}},
		{"wait-merge-result-ignores-irrelevant-check", func(t *testing.T) {
			c := pr("giantswarm/muster", 12, "a1", cstatus(goTest, "success"))
			c.Status.Facts.Reviews = []v1alpha1.ReviewFact{{By: bob, State: reviewApproved}}
			s := newSim(t, c)
			s.step(t0, TriggerStartup, nil, nil)
			s.step(t0, TriggerWebhook, nil, merge("m1"))
			d := s.step(at(time.Minute), TriggerWebhook, nil, setMerge(failed("add-to-project")))
			eq(t, "ci", d.Status.CI.SHA+"/"+d.Status.CI.State, "a1/green")
			eq(t, "merge result", d.Status.MergeResult.SHA+"/"+d.Status.MergeResult.State, "m1/pending")
			eq(t, "ignored", d.Status.MergeResult.Ignored, []string{"add-to-project"})
			eq(t, "wait", d.Status.Wait.Ref+"/"+d.Status.Wait.State, "ci:giantswarm/muster@m1/watching")
			d = s.step(at(2*time.Minute), TriggerWebhook, nil, setMerge(failed("add-to-project"), cstatus(goTest, "success")))
			eq(t, "merge result", d.Status.MergeResult.State, v1alpha1.CIGreen)
			eq(t, "relevant", d.Status.MergeResult.Relevant, []string{goTest})
			eq(t, "ignored", d.Status.MergeResult.Ignored, []string{"add-to-project"})
			eq(t, "no wait", d.Status.Wait == nil, true)
		}},
		{"wait-merge-result-watched-after-hand-merge", func(t *testing.T) {
			s := newSim(t, pr("giantswarm/muster", 12, "a1", cstatus(goTest, "failure")))
			d := s.step(t0, TriggerStartup, nil, nil)
			eq(t, "red", d.Status.CI.State, v1alpha1.CIRed)
			eq(t, "no wait", d.Status.Wait == nil, true)
			d = s.step(t0, TriggerWebhook, nil, merge("m1"))
			eq(t, "ci keeps red head", d.Status.CI.SHA+"/"+d.Status.CI.State, "a1/red")
			eq(t, "merge result", d.Status.MergeResult.State, v1alpha1.CIPending)
			eq(t, "wait", d.Status.Wait.Ref+"/"+d.Status.Wait.State, "ci:giantswarm/muster@m1/watching")
			d = s.step(at(time.Minute), TriggerWebhook, nil, setMerge(cstatus(goTest, "failure")))
			eq(t, "merge result", d.Status.MergeResult.State, v1alpha1.CIRed)
			eq(t, "no wait", d.Status.Wait == nil, true)
		}},
		{"wait-merge-result-never-reported-violates-invariant", func(t *testing.T) {
			c := pr("giantswarm/muster", 12, "a1", cstatus(goTest, "success"))
			c.Status.Facts.Reviews = []v1alpha1.ReviewFact{{By: bob, State: reviewApproved}}
			s := newSim(t, c)
			s.step(t0, TriggerStartup, nil, nil)
			d := s.step(t0, TriggerWebhook, nil, merge("m1"))
			eq(t, "deadline", d.Status.Wait.Deadline.Time, at(2*time.Hour))
			d = s.step(at(2*time.Hour), TriggerDeadline, &Reobserve{Made: true}, nil)
			eq(t, "reobserved", len(eventsOf(d.Events, EventWaitReobserved)), 1)
			eq(t, "merge result", d.Status.MergeResult.State, v1alpha1.CIPending)
			eq(t, "overdue", d.Status.Wait.State, v1alpha1.WaitOverdue)
			eq(t, "handled", d.Status.Wait.DeadlineHandled, true)
		}},
		{"invariant-pending-ci-without-wait-detected", func(t *testing.T) {
			s := newSim(t, pr("giantswarm/klaus", 55, "a1"))
			s.dropWaits = true
			d := s.step(t0, TriggerWebhook, nil, nil)
			eq(t, "ci", d.Status.CI.SHA+"/"+d.Status.CI.State, "a1/pending")
			eq(t, "no wait", d.Status.Wait == nil, true)
			eq(t, "no rows", len(d.Events), 0)
		}},
		{"needs-other-team-keeps-the-review-wait", func(t *testing.T) {
			c := pr("giantswarm/klaus", 55, "a1", passed(goBuild), passed(goTest))
			c.Status.Facts.CodeOwnerTeams = []string{"giantswarm/team-atlas"}
			s := newSim(t, c)
			s.step(t0, TriggerStartup, nil, nil)
			d := s.step(at(time.Minute), TriggerWebhook, nil, reviewBy(bob, reviewApproved, "team-bumblebee"))
			eq(t, "needsOtherTeam", d.Status.Review.NeedsOtherTeam, true)
			eq(t, "state", d.Status.Review.State, v1alpha1.ReviewNone)
			eq(t, "approvals", d.Status.Review.Approvals, 1)
			eq(t, "still waits", d.Status.Wait.On, v1alpha1.WaitOnReview)
			eq(t, "woken by the approval", len(eventsOf(d.Events, EventWaitWoken)), 1)
			d = s.step(at(2*time.Minute), TriggerWebhook, nil, reviewBy("carol", reviewApproved, "team-atlas"))
			eq(t, "approved", d.Status.Review.State, v1alpha1.ReviewApproved)
			eq(t, "no wait", d.Status.Wait == nil, true)
		}},
		{"ci-duration-recorded-when-final", func(t *testing.T) {
			start := metav1.NewTime(t0)
			c := pr("giantswarm/klaus", 55, "a1", running(goBuild), running(goTest))
			c.Status.Facts.Head[0].StartedAt, c.Status.Facts.Head[1].StartedAt = &start, &start
			s := newSim(t, c)
			d := s.step(t0, TriggerStartup, nil, nil)
			eq(t, "none yet", len(d.Durations), 0)
			end1, end2 := metav1.NewTime(at(20*time.Minute)), metav1.NewTime(at(25*time.Minute))
			b, g := passed(goBuild), passed(goTest)
			b.StartedAt, b.CompletedAt, g.StartedAt, g.CompletedAt = &start, &end1, &start, &end2
			d = s.step(at(25*time.Minute), TriggerWebhook, nil, setHead(b, g))
			eq(t, "duration", d.Durations, []Duration{{Kind: history.DurationCI, Key: "giantswarm/klaus", D: 25 * time.Minute}}) // gitleaks:allow: a repository, not a key
			d = s.step(at(26*time.Minute), TriggerResync, nil, nil)
			eq(t, "once", len(d.Durations), 0)
		}},
		{"rerun-replaces-failed-run", func(t *testing.T) {
			first, second := metav1.NewTime(t0), metav1.NewTime(at(time.Minute))
			f, p := failed(goTest), passed(goTest)
			f.StartedAt, p.StartedAt = &first, &second
			s := newSim(t, pr("giantswarm/muster", 12, "a1", p, f))
			d := s.step(t0, TriggerStartup, nil, nil)
			eq(t, "green", d.Status.CI.State, v1alpha1.CIGreen)
		}},
		{"no-required-checks-means-none", func(t *testing.T) {
			c := pr("giantswarm/klaus", 55, "a1", running("lint"))
			c.Status.Facts.RequiredChecks = nil
			s := newSim(t, c)
			d := s.step(t0, TriggerStartup, nil, nil)
			eq(t, "none", d.Status.CI.State, v1alpha1.CINone)
			eq(t, "ignored", d.Status.CI.Ignored, []string{"lint"})
			eq(t, "review wait", d.Status.Wait.On, v1alpha1.WaitOnReview)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.run(t)
		})
	}
}

// TestWaitLifecycle ports the deadline, overdue, unobserved and restart scenarios.
func TestWaitLifecycle(t *testing.T) {
	t.Parallel()
	pending := func() *v1alpha1.Change { return pr("giantswarm/klaus", 55, "a1", passed(goBuild), running(goTest)) }
	green := setHead(passed(goBuild), passed(goTest))
	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"wait-deadline-reobserves-once", func(t *testing.T) {
			s := newSim(t, pending())
			s.spec.Waits.CI.DefaultDeadline = dur(4 * time.Minute)
			d := s.step(t0, TriggerStartup, nil, nil)
			eq(t, "deadline", d.Status.Wait.Deadline.Time, at(4*time.Minute))
			d = s.step(at(4*time.Minute), TriggerDeadline, &Reobserve{Made: true, Changed: true}, green)
			eq(t, "ci", d.Status.CI.State, v1alpha1.CIGreen)
			eq(t, "wait.on", d.Status.Wait.On, v1alpha1.WaitOnReview)
			dl := eventsOf(d.Events, EventWaitDeadline)
			eq(t, "deadline rows", len(dl), 1)
			eq(t, "deadline source", dl[0].Source, v1alpha1.SourceDeadline)
			eq(t, "reobserved", len(eventsOf(d.Events, EventWaitReobserved)), 1)
			w := eventsOf(d.Events, EventWaitWoken)
			eq(t, "woken", len(w), 1)
			eq(t, "woken source", w[0].Source, v1alpha1.SourceReobserve)
			eq(t, "woken on", w[0].Data["on"], "ci")
			eq(t, "never overdue", len(eventsOf(s.rows, EventWaitOverdue)), 0)
		}},
		{"wait-deadline-unchanged-goes-overdue", func(t *testing.T) {
			s := newSim(t, pending())
			d := s.step(t0, TriggerStartup, nil, nil)
			eq(t, "deadline", d.Status.Wait.Deadline.Time, at(time.Hour))
			// The engine's timers and the 15-minute changes resyncs, in clock order, for three hours.
			next := *d.NextCheck
			resync := at(15 * time.Minute)
			count := func(from, to time.Time, typ string) int {
				n := 0
				for _, e := range s.rows {
					if e.Type == typ && !e.At.Before(from) && e.At.Before(to) {
						n++
					}
				}
				return n
			}
			for now := t0; now.Before(at(3 * time.Hour)); {
				if resync.Before(next) {
					now = resync
					s.step(now, TriggerResync, nil, nil)
					resync = resync.Add(15 * time.Minute)
					continue
				}
				now = next
				d = s.step(now, TriggerDeadline, &Reobserve{Made: true}, nil)
				next = *d.NextCheck
			}
			eq(t, "reobserved at the deadline", count(at(time.Hour), at(time.Hour+time.Second), EventWaitReobserved), 1)
			eq(t, "overdue rows", len(eventsOf(s.rows, EventWaitOverdue)), 1)
			eq(t, "overdue at", eventsOf(s.rows, EventWaitOverdue)[0].At, at(time.Hour))
			eq(t, "first overdue hour", count(at(time.Hour+time.Second), at(2*time.Hour), EventWaitReobserved), 8)
			eq(t, "second overdue hour", count(at(2*time.Hour), at(3*time.Hour), EventWaitReobserved), 6)
			eq(t, "state", s.c.Status.Wait.State, v1alpha1.WaitOverdue)
			eq(t, "capped", s.c.Status.Wait.Backoff.Duration, 10*time.Minute)
			eq(t, "no woken", len(eventsOf(s.rows, EventWaitWoken)), 0)
		}},
		{"wait-survives-restart", func(t *testing.T) {
			s := newSim(t, pending())
			s.step(t0, TriggerStartup, nil, nil)
			d := s.step(at(30*time.Minute), TriggerStartup, nil, nil)
			waitIs(t, d.Status.Wait, "ci", "ci:giantswarm/klaus@a1", t0, at(time.Hour), "watching")
			eq(t, "no rows", len(d.Events), 0)
			d = s.step(at(time.Hour), TriggerDeadline, &Reobserve{Made: true}, nil)
			eq(t, "deadline", len(eventsOf(d.Events, EventWaitDeadline)), 1)
			eq(t, "reobserved", len(eventsOf(d.Events, EventWaitReobserved)), 1)
			eq(t, "overdue", d.Status.Wait.State, v1alpha1.WaitOverdue)
			eq(t, "next", *d.NextCheck, at(time.Hour+time.Minute))
		}},
		{"wait-unobserved-when-reobserve-fails", func(t *testing.T) {
			s := newSim(t, pending())
			s.step(t0, TriggerStartup, nil, nil)
			bad := &Reobserve{Made: true, Err: &forge.Error{Kind: forge.Transient, API: forge.APIREST, Code: 502}}
			d := s.step(at(time.Hour), TriggerDeadline, bad, nil)
			eq(t, "state", d.Status.Wait.State, v1alpha1.WaitUnobserved)
			eq(t, "reason", d.Status.Wait.Reason, "http-502")
			eq(t, "unobserved rows", len(eventsOf(d.Events, EventWaitUnobserved)), 1)
			eq(t, "not handled", d.Status.Wait.DeadlineHandled, false)
			// Transient retries: 10s, doubling, capped at 5m, failing until 10:10.
			var retries []time.Time
			for d.NextCheck.Before(at(70 * time.Minute)) {
				retries = append(retries, *d.NextCheck)
				d = s.step(*d.NextCheck, TriggerRetry, bad, nil)
				eq(t, "still unobserved", d.Status.Wait.State, v1alpha1.WaitUnobserved)
			}
			want := []time.Time{
				at(time.Hour + 10*time.Second), at(time.Hour + 30*time.Second), at(time.Hour + 70*time.Second),
				at(time.Hour + 150*time.Second), at(time.Hour + 310*time.Second),
			}
			eq(t, "retries", retries, want)
			eq(t, "next retry", *d.NextCheck, at(time.Hour+10*time.Minute+10*time.Second))
			eq(t, "one unobserved row", len(eventsOf(s.rows, EventWaitUnobserved)), 1)
			eq(t, "never overdue while unobserved", len(eventsOf(s.rows, EventWaitOverdue)), 0)
			d = s.step(*d.NextCheck, TriggerRetry, &Reobserve{Made: true}, nil)
			eq(t, "overdue", d.Status.Wait.State, v1alpha1.WaitOverdue)
			eq(t, "reason cleared", d.Status.Wait.Reason, "")
			o := eventsOf(d.Events, EventWaitOverdue)
			eq(t, "overdue rows", len(o), 1)
			eq(t, "overdue at", o[0].At, at(time.Hour+10*time.Minute+10*time.Second))
			eq(t, "deadline handled once", len(eventsOf(s.rows, EventWaitDeadline)), 1)
		}},
		{"budget-reserve-defers-observation", func(t *testing.T) {
			s := newSim(t, pending())
			s.step(t0, TriggerStartup, nil, nil)
			deferred := &Reobserve{Err: &forge.Error{Kind: forge.Deferred, API: forge.APIREST, ResetAt: at(30 * time.Minute)}}
			d := s.step(at(15*time.Minute), TriggerWebhook, deferred, nil)
			eq(t, "ci", d.Status.CI.State, v1alpha1.CIPending)
			eq(t, "state", d.Status.Wait.State, v1alpha1.WaitUnobserved)
			eq(t, "reason", d.Status.Wait.Reason, v1alpha1.WaitReasonBudget)
			eq(t, "until the reset", *d.NextCheck, at(30*time.Minute))
			d = s.step(at(30*time.Minute), TriggerRetry, &Reobserve{Made: true, Changed: true}, green)
			eq(t, "ci", d.Status.CI.State, v1alpha1.CIGreen)
			eq(t, "review", d.Status.Wait.On+"/"+d.Status.Wait.State, "review/watching")
		}},
		{"source-degraded-unobserved", func(t *testing.T) {
			s := newSim(t, pending())
			s.step(t0, TriggerStartup, nil, nil)
			d := s.step(at(10*time.Minute), TriggerPoll, &Reobserve{Made: false, Reason: v1alpha1.WaitReasonSourceDegraded}, nil)
			eq(t, "state", d.Status.Wait.State, v1alpha1.WaitUnobserved)
			eq(t, "reason", d.Status.Wait.Reason, v1alpha1.WaitReasonSourceDegraded)
			d = s.step(*d.NextCheck, TriggerRetry, &Reobserve{Made: true}, nil)
			eq(t, "watching again", d.Status.Wait.State, v1alpha1.WaitWatching)
			eq(t, "next is the deadline", *d.NextCheck, at(time.Hour))
		}},
		{"wait-lost-webhook-woken-by-resync", func(t *testing.T) {
			s := newSim(t, pending())
			s.step(t0, TriggerStartup, nil, nil)
			d := s.step(at(15*time.Minute), TriggerResync, nil, green)
			w := eventsOf(d.Events, EventWaitWoken)
			eq(t, "woken", len(w), 1)
			eq(t, "source", w[0].Source, v1alpha1.SourceResync)
			eq(t, "at", w[0].At, at(15*time.Minute))
			eq(t, "no deadline", len(eventsOf(s.rows, EventWaitDeadline)), 0)
		}},
		{"wait-delayed-delivery-processed-once", func(t *testing.T) {
			s := newSim(t, pending())
			s.step(t0, TriggerStartup, nil, nil)
			s.step(at(15*time.Minute), TriggerResync, nil, green)
			d := s.step(at(20*time.Minute), TriggerWebhook, nil, nil)
			eq(t, "no second wake", len(eventsOf(d.Events, EventWaitWoken)), 0)
			eq(t, "no rows", len(d.Events), 0)
			eq(t, "still review", d.Status.Wait.On, v1alpha1.WaitOnReview)
			eq(t, "wakes once in all", len(eventsOf(s.rows, EventWaitWoken)), 1)
			// A second reconcile at the same instant changes nothing either.
			d = s.step(at(20*time.Minute), TriggerWebhook, nil, nil)
			eq(t, "idempotent", len(d.Events), 0)
		}},
		{"wait-unrelated-webhook-wakes-nothing", func(t *testing.T) {
			s := newSim(t, pending())
			before := s.step(t0, TriggerStartup, nil, nil)
			d := s.step(at(time.Minute), TriggerWebhook, nil, nil)
			eq(t, "no rows", len(d.Events), 0)
			eq(t, "unchanged", *d.Status.Wait, *before.Status.Wait)
			eq(t, "no item wakes", d.WakeItems == nil, true)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.run(t)
		})
	}
}

// TestDependencyWaitDeadline: a dependency wait re-observes once at its 168h deadline, like any wait.
func TestDependencyWaitDeadline(t *testing.T) {
	t.Parallel()
	w := newWorld(iss{ref: refRoadmap101, body: ac + "Repo: giantswarm/klaus\n", blockedBy: []string{refRoadmap100}, status: upNext, kind: story})
	items, _ := w.decided(t, t0, nil)
	it := items[refRoadmap101]
	d := Item(ItemView{Now: at(168 * time.Hour), Spec: w.spec, Team: w.team, Participants: w.participants(), Item: it, Trigger: TriggerDeadline, Reobserve: &Reobserve{Made: true}})
	eq(t, "deadline rows", len(eventsOf(d.Events, EventWaitDeadline)), 1)
	eq(t, "overdue", d.Status.Wait.State, v1alpha1.WaitOverdue)
	eq(t, "item", eventsOf(d.Events, EventWaitOverdue)[0].Item, refRoadmap101)
	eq(t, "next", *d.NextCheck, at(168*time.Hour+time.Minute))
}

func TestP90NearestRank(t *testing.T) {
	t.Parallel()
	m := time.Minute
	cases := []struct {
		name string
		in   []time.Duration
		want time.Duration
	}{
		{"none", nil, 0},
		{"one", []time.Duration{5 * m}, 5 * m},
		{"three", []time.Duration{20 * m, 25 * m, 30 * m}, 30 * m},
		{"unsorted", []time.Duration{30 * m, 20 * m, 25 * m}, 30 * m},
		{"ten", []time.Duration{1 * m, 2 * m, 3 * m, 4 * m, 5 * m, 6 * m, 7 * m, 8 * m, 9 * m, 10 * m}, 9 * m},
		{"eleven", []time.Duration{1 * m, 2 * m, 3 * m, 4 * m, 5 * m, 6 * m, 7 * m, 8 * m, 9 * m, 10 * m, 11 * m}, 10 * m},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			eq(t, "p90", P90(c.in), c.want)
		})
	}
	spec := baseSpec()
	eq(t, "too little history", CIDeadline(spec, []time.Duration{4 * m, 4 * m}), time.Hour)
	eq(t, "from history", CIDeadline(spec, []time.Duration{4 * m, 4 * m, 4 * m}), 12*m)
}
