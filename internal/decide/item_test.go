package decide

import (
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/decision"
)

// TestItemScenarios ports the eligibility, epic and kind scenarios: one case per scenario, decided at
// startup (decide.Item over every Item, then the team pass).
func TestItemScenarios(t *testing.T) {
	t.Parallel()
	ready := ac + "Repo: giantswarm/klaus\n"
	cases := []struct {
		name  string
		world func() *world
		check func(t *testing.T, r passResult)
	}{
		{
			name: "eligibility-assigned-to-non-participant-never-touched",
			world: func() *world {
				return newWorld(iss{ref: refRoadmap101, body: "Carol wants it.\nRepo: giantswarm/klaus\n", assignees: []string{"carol"}, status: upNext, kind: story, prio: 1})
			},
			check: func(t *testing.T, r passResult) {
				it := r.items[refRoadmap101]
				eq(t, "eligible", it.Status.Eligible.Value, false)
				eq(t, "reasons", joined(it.Status.Eligible.Reasons), "AssignedToNonParticipant")
				eq(t, "owner", it.Status.Owner, "")
				eq(t, v1alpha1.DecisionModeShadow, it.Status.Shadow == nil, true)
				eq(t, "questions", len(r.decisions[refRoadmap101].Questions), 0)
				eq(t, "picks", len(r.team.Team.Picks), 0)
			},
		},
		{
			name: "eligibility-assigned-to-participant-only-for-them",
			world: func() *world {
				return newWorld(iss{ref: refRoadmap101, body: ready, assignees: []string{bob}, status: upNext, kind: story})
			},
			check: func(t *testing.T, r passResult) {
				it := r.items[refRoadmap101]
				eq(t, "eligible", it.Status.Eligible.Value, true)
				eq(t, "owner", it.Status.Owner, bob)
				eq(t, "picks", joined(teamPicks(r.team)), "bob:giantswarm/roadmap#101")
				eq(t, "alice free", r.team.People[alice].Free.Items, 1)
				eq(t, "alice picks", len(r.team.People[alice].Picks), 0)
				eq(t, "no assign", actionOf(it.Status.Shadow, v1alpha1.ActionAssign) == nil, true)
			},
		},
		{
			name: "eligibility-bug-needs-up-next",
			world: func() *world {
				return newWorld(iss{ref: refRoadmap201, body: "Last message missing.\nRepo: giantswarm/klaus\n", status: inbox, kind: bug})
			},
			check: func(t *testing.T, r passResult) {
				it := r.items[refRoadmap201]
				eq(t, "class", it.Status.Class, v1alpha1.ClassImplementable)
				eq(t, "phase", it.Status.Phase, v1alpha1.PhaseBacklog)
				eq(t, "reasons", joined(it.Status.Eligible.Reasons), "NotUpNext")
				eq(t, "actions", joined(actionTypes(it.Status.Shadow)), "set-severity")
				eq(t, "severity", actionOf(it.Status.Shadow, v1alpha1.ActionSetSeverity).Args["severity"], "medium")
				qs := questionsOf(r.decisions[refRoadmap201].Questions, decision.PointBugSeverity, "")
				eq(t, "bug-severity? asked", len(qs), 1)
				eq(t, "rule", qs[0].Rule, "medium")
				eq(t, "mode", qs[0].Mode, v1alpha1.DecisionModeShadow)
				eq(t, "no ready?", len(questionsOf(r.decisions[refRoadmap201].Questions, decision.PointReady, "")), 0)
				eq(t, "one action row", len(eventsOf(r.team.Events, EventShadowAction)), 1)
				eq(t, "left", it.Status.LeftAt == nil, true)
			},
		},
		{
			name: "eligibility-epic-itself-not-picked",
			world: func() *world {
				return newWorld(
					iss{ref: refRoadmap90, status: upNext, kind: epic, prio: 1},
					iss{ref: refRoadmap101, body: ready, assignees: []string{alice}, parent: refRoadmap90, status: upNext, kind: story, prio: 2},
				)
			},
			check: func(t *testing.T, r passResult) {
				ep := r.items[refRoadmap90]
				eq(t, "class", ep.Status.Class, v1alpha1.ClassEpic)
				eq(t, "reasons", joined(ep.Status.Eligible.Reasons), "KindNotImplementable")
				eq(t, "epic shadow", ep.Status.Shadow == nil, true)
				eq(t, "sub-item pick", r.items[refRoadmap101].Status.Shadow.Pick.Person, alice)
				eq(t, "bob picks", len(r.team.People[bob].Picks), 0)
				eq(t, "epic actions", len(eventsFor(r.team.Events, refRoadmap90, EventShadowAction)), 0)
			},
		},
		{
			name: "eligibility-issue-in-code-repo-has-target",
			world: func() *world {
				return newWorld(iss{ref: "giantswarm/klaus#40", body: ac, status: upNext, kind: story})
			},
			check: func(t *testing.T, r passResult) {
				it := r.items["giantswarm/klaus#40"]
				eq(t, "targets", joined(it.Status.TargetRepos), "giantswarm/klaus")
				eq(t, "ac", it.Status.AcceptanceCriteria, true)
				eq(t, "eligible", it.Status.Eligible.Value, true)
				eq(t, "reasons", it.Status.Eligible.Reasons == nil, true)
				qs := questionsOf(r.decisions["giantswarm/klaus#40"].Questions, decision.PointReady, "giantswarm/klaus#40")
				eq(t, "ready?", len(qs), 1)
				eq(t, "rule", qs[0].Rule, "ready")
			},
		},
		{
			name: "eligibility-no-target-repo-not-ready",
			world: func() *world {
				return newWorld(iss{ref: refRoadmap101, body: ac, assignees: []string{alice}, status: upNext, kind: story})
			},
			check: func(t *testing.T, r passResult) {
				it := r.items[refRoadmap101]
				eq(t, "ac", it.Status.AcceptanceCriteria, true)
				eq(t, "targets", len(it.Status.TargetRepos), 0)
				eq(t, "reasons", joined(it.Status.Eligible.Reasons), "NotReady")
				eq(t, "todo to", actionOf(it.Status.Shadow, v1alpha1.ActionTodo).Args["to"], alice)
				eq(t, "rule", questionsOf(r.decisions[refRoadmap101].Questions, decision.PointReady, "")[0].Rule, "not-ready")
				eq(t, "one action row", len(eventsFor(r.team.Events, refRoadmap101, EventShadowAction)), 1)
			},
		},
		{
			name: "eligibility-not-ready-records-one-todo",
			world: func() *world {
				return newWorld(
					iss{ref: refRoadmap101, body: "The client should retry.\nRepo: giantswarm/klaus\n", assignees: []string{alice}, status: upNext, kind: story},
					iss{ref: refRoadmap102, body: "We need metrics.\nRepo: giantswarm/klaus\n", status: upNext, kind: story},
				)
			},
			check: func(t *testing.T, r passResult) {
				a := r.items[refRoadmap101]
				eq(t, "ac", a.Status.AcceptanceCriteria, false)
				eq(t, "reasons", joined(a.Status.Eligible.Reasons), "NotReady")
				eq(t, "todo to alice", actionOf(a.Status.Shadow, v1alpha1.ActionTodo).Args["to"], alice)
				eq(t, "todo text", actionOf(a.Status.Shadow, v1alpha1.ActionTodo).Args["text"], "refine with `/gs-plans:gs-plan`")
				b := r.items[refRoadmap102]
				eq(t, "todo to team", actionOf(b.Status.Shadow, v1alpha1.ActionTodo).Args["to"], "team:bumblebee")
				eq(t, "one todo each", len(eventsOf(r.team.Events, EventShadowAction)), 2)
			},
		},
		{
			name: "eligibility-open-blocker-waits-on-dependency",
			world: func() *world {
				return newWorld(
					iss{ref: refRoadmap100, assignees: []string{bob}},
					iss{ref: refRoadmap101, body: ready, assignees: []string{alice}, blockedBy: []string{refRoadmap100}, status: upNext, kind: story},
				)
			},
			check: func(t *testing.T, r passResult) {
				it := r.items[refRoadmap101]
				eq(t, "phase", it.Status.Phase, v1alpha1.PhaseReady)
				eq(t, "reasons", joined(it.Status.Eligible.Reasons), "BlockedByOpenIssue")
				eq(t, "pick", it.Status.Shadow == nil || it.Status.Shadow.Pick == nil, true)
				w := it.Status.Wait
				eq(t, "on", w.On, v1alpha1.WaitOnDependency)
				eq(t, "ref", w.Ref, "issue:giantswarm/roadmap#100")
				eq(t, "since", w.Since.Time, t0)
				eq(t, "deadline", w.Deadline.Time, time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC))
				eq(t, "atDeadline", w.AtDeadline, v1alpha1.AtDeadlineReobserve)
				eq(t, "state", w.State, v1alpha1.WaitWatching)
				eq(t, "next check", *r.decisions[refRoadmap101].NextCheck, w.Deadline.Time)
				eq(t, "wait.started", len(eventsOf(r.decisions[refRoadmap101].Events, EventWaitStarted)), 1)
			},
		},
		{
			name: "eligibility-team-not-opted-in-for-work",
			world: func() *world {
				w := newWorld(iss{ref: refRoadmap101, body: ready, assignees: []string{alice}, status: upNext, kind: story})
				w.team.Spec.OptIn.Work = []string{}
				return w
			},
			check: func(t *testing.T, r passResult) {
				it := r.items[refRoadmap101]
				eq(t, "phase", it.Status.Phase, v1alpha1.PhaseReady)
				eq(t, "reasons", joined(it.Status.Eligible.Reasons), "TeamNotOptedIn")
				eq(t, "no ready?", len(r.decisions[refRoadmap101].Questions), 0)
				eq(t, "picks", len(r.team.Team.Picks), 0)
				eq(t, "actions", len(eventsOf(r.team.Events, EventShadowAction)), 0)
			},
		},
		{
			name: "eligibility-up-next-ready-item-is-eligible",
			world: func() *world {
				return newWorld(iss{ref: refRoadmap101, body: ready, status: upNext, kind: story})
			},
			check: func(t *testing.T, r passResult) {
				it := r.items[refRoadmap101]
				eq(t, "phase", it.Status.Phase, v1alpha1.PhaseReady)
				eq(t, "ac", it.Status.AcceptanceCriteria, true)
				eq(t, "targets", joined(it.Status.TargetRepos), "giantswarm/klaus")
				eq(t, "eligible", it.Status.Eligible.Value, true)
				qs := questionsOf(r.decisions[refRoadmap101].Questions, decision.PointReady, "")
				eq(t, "ready?", len(qs), 1)
				eq(t, "rule", qs[0].Rule, "ready")
				eq(t, "mode", qs[0].Mode, v1alpha1.DecisionModeShadow)
				eq(t, "no todo", actionOf(it.Status.Shadow, v1alpha1.ActionTodo) == nil, true)
				eq(t, "shadow ready", it.Status.Shadow.Ready, "ready")
			},
		},
		{
			name: "epic-planned-with-no-started-sub-items",
			world: func() *world {
				return newWorld(
					iss{ref: refRoadmap90, assignees: []string{alice}, status: upNext, kind: epic},
					iss{ref: refRoadmap102, body: ready, assignees: []string{alice}, parent: refRoadmap90, status: upNext, kind: story},
					iss{ref: refRoadmap101, body: ready, assignees: []string{alice}, parent: refRoadmap90, status: upNext, kind: story},
				)
			},
			check: func(t *testing.T, r passResult) {
				ep := r.items[refRoadmap90]
				eq(t, "class", ep.Status.Class, v1alpha1.ClassEpic)
				eq(t, "phase", ep.Status.Phase, v1alpha1.PhasePlanned)
				eq(t, "subItems", joined(ep.Status.SubItems), "giantswarm/roadmap#101,giantswarm/roadmap#102")
			},
		},
		{
			name: "epic-active-when-a-sub-item-passes-ready",
			world: func() *world {
				return newWorld(
					iss{ref: refRoadmap90, assignees: []string{alice}, status: upNext, kind: epic},
					iss{ref: refRoadmap101, body: ready, assignees: []string{alice}, parent: refRoadmap90, status: inProgress, kind: story},
					iss{ref: refRoadmap102, body: ready, assignees: []string{alice}, parent: refRoadmap90, status: upNext, kind: story},
				)
			},
			check: func(t *testing.T, r passResult) {
				eq(t, "phase", r.items[refRoadmap90].Status.Phase, v1alpha1.PhaseActive)
				eq(t, "in flight", r.team.Team.InFlight, 1)
				eq(t, "engine wip", r.team.Team.WIP, v1alpha1.WIP{})
			},
		},
		{
			name: "epic-done-when-all-sub-items-done",
			world: func() *world {
				return newWorld(
					iss{ref: refRoadmap90, assignees: []string{alice}, status: inProgress, kind: epic},
					iss{ref: refRoadmap101, body: ready, assignees: []string{alice}, parent: refRoadmap90, status: done, kind: story},
					iss{ref: refRoadmap102, body: ready, assignees: []string{bob}, parent: refRoadmap90, status: inProgress, kind: story, state: v1alpha1.IssueClosed},
				)
			},
			check: func(t *testing.T, r passResult) {
				ep := r.items[refRoadmap90]
				eq(t, "phase", ep.Status.Phase, v1alpha1.PhaseDone)
				eq(t, "issue open", ep.Status.Issue.State, v1alpha1.IssueOpen)
				eq(t, "still tracked", ep.Status.LeftAt == nil, true)
			},
		},
		{
			name: "epic-sub-items-inherit-owner",
			world: func() *world {
				return newWorld(
					iss{ref: refRoadmap90, assignees: []string{alice}, status: upNext, kind: epic},
					iss{ref: refRoadmap101, body: ready, parent: refRoadmap90, status: upNext, kind: story},
				)
			},
			check: func(t *testing.T, r passResult) {
				it := r.items[refRoadmap101]
				eq(t, "assignees", len(it.Status.Assignees), 0)
				eq(t, "owner", it.Status.Owner, alice)
				eq(t, "pick", it.Status.Shadow.Pick.Person, alice)
				eq(t, "assign", actionOf(it.Status.Shadow, v1alpha1.ActionAssign).Args["login"], alice)
				eq(t, "bob picks", len(r.team.People[bob].Picks), 0)
				eq(t, "bob held", len(r.team.People[bob].Held), 0)
			},
		},
		{
			name: "board-kind-mapping",
			world: func() *world {
				return newWorld(
					iss{ref: refRoadmap201, status: upNext, kind: epic},
					iss{ref: refRoadmap202, status: upNext, kind: story},
					iss{ref: "giantswarm/roadmap#203", status: upNext, kind: task},
					iss{ref: "giantswarm/roadmap#204", status: upNext, kind: "Feature 💫"},
					iss{ref: "giantswarm/roadmap#205", status: upNext, kind: bug},
					iss{ref: refRoadmap206, body: ready, status: upNext, kind: "Alert 🚨", prio: 1},
					iss{ref: "giantswarm/roadmap#207", status: upNext, kind: "Note 📝"},
					iss{ref: "giantswarm/roadmap#208", status: upNext},
				)
			},
			check: func(t *testing.T, r passResult) {
				want := map[string]string{
					refRoadmap201: v1alpha1.ClassEpic, refRoadmap202: v1alpha1.ClassImplementable,
					"giantswarm/roadmap#203": v1alpha1.ClassImplementable, "giantswarm/roadmap#204": v1alpha1.ClassImplementable,
					"giantswarm/roadmap#205": v1alpha1.ClassImplementable, refRoadmap206: v1alpha1.ClassIncident,
					"giantswarm/roadmap#207": v1alpha1.ClassIgnored, "giantswarm/roadmap#208": v1alpha1.ClassIgnored,
				}
				for ref, class := range want {
					eq(t, ref+" class", r.items[ref].Status.Class, class)
				}
				alert := r.items[refRoadmap206]
				eq(t, "alert eligible", alert.Status.Eligible.Value, false)
				eq(t, "alert shadow", alert.Status.Shadow == nil, true)
				for _, p := range r.team.Team.Picks {
					if p.Item == refRoadmap206 {
						t.Error("alert picked")
					}
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.check(t, c.world().pass(t, t0, nil, false))
		})
	}
}

// TestItemTransitions ports the scenarios where a webhook changes an Item: the events are recorded once,
// with the webhook's source.
func TestItemTransitions(t *testing.T) {
	t.Parallel()
	ready := ac + "Repo: giantswarm/klaus\n"
	cases := []struct {
		name   string
		world  func() *world
		change func(w *world)
		check  func(t *testing.T, d ItemDecision, before, after passResult)
		ref    string
	}{
		{
			name: "eligibility-becomes-ready-when-body-edited",
			ref:  refRoadmap101,
			world: func() *world {
				return newWorld(iss{ref: refRoadmap101, body: "The client should retry.\nRepo: giantswarm/klaus\n", assignees: []string{alice}, status: upNext, kind: story})
			},
			change: func(w *world) {
				w.issues[0].body = "The client should retry.\n### Acceptance criteria\n- [ ] retries 5xx up to three times\nRepo: giantswarm/klaus\n"
			},
			check: func(t *testing.T, d ItemDecision, before, after passResult) {
				eq(t, "before", joined(before.items[refRoadmap101].Status.Eligible.Reasons), "NotReady")
				eq(t, "eligible", d.Status.Eligible.Value, true)
				eq(t, "reasons", d.Status.Eligible.Reasons == nil, true)
				ev := eventsOf(d.Events, EventEligibilityChanged)
				eq(t, "eligibility.changed", len(ev), 1)
				eq(t, "source", ev[0].Source, v1alpha1.SourceWebhook)
				q := questionsOf(d.Questions, decision.PointReady, "")
				eq(t, "decided again", q[0].Rule, "ready")
				eq(t, "new inputs", q[0].InputsHash != questionsOf(before.decisions[refRoadmap101].Questions, decision.PointReady, "")[0].InputsHash, true)
				eq(t, "team pass", d.TeamPass, true)
			},
		},
		{
			name: "eligibility-open-blocker-waits-on-dependency/blocker-closed",
			ref:  refRoadmap101,
			world: func() *world {
				return newWorld(
					iss{ref: refRoadmap100, assignees: []string{bob}},
					iss{ref: refRoadmap101, body: ready, assignees: []string{alice}, blockedBy: []string{refRoadmap100}, status: upNext, kind: story},
				)
			},
			change: func(w *world) { w.issues[0].state = v1alpha1.IssueClosed },
			check: func(t *testing.T, d ItemDecision, _, _ passResult) {
				eq(t, "eligible", d.Status.Eligible.Value, true)
				eq(t, "wait", d.Status.Wait == nil, true)
				eq(t, "next check", d.NextCheck == nil, true)
				w := eventsOf(d.Events, EventWaitWoken)
				eq(t, "woken", len(w), 1)
				eq(t, "woken source", w[0].Source, v1alpha1.SourceWebhook)
				eq(t, "woken on", w[0].Data["on"], v1alpha1.WaitOnDependency)
				eq(t, "ended", len(eventsOf(d.Events, EventWaitEnded)), 1)
			},
		},
		{
			name: "board-status-change-is-input",
			ref:  refRoadmap101,
			world: func() *world {
				return newWorld(iss{ref: refRoadmap101, body: ready, assignees: []string{alice}, status: upNext, kind: story})
			},
			change: func(w *world) { w.issues[0].status = backlog },
			check: func(t *testing.T, d ItemDecision, _, after passResult) {
				eq(t, "phase", d.Status.Phase, v1alpha1.PhaseBacklog)
				eq(t, "reasons", joined(d.Status.Eligible.Reasons), "NotUpNext")
				eq(t, "wait", d.Status.Wait == nil, true)
				eq(t, "phase.changed", len(eventsOf(d.Events, EventPhaseChanged)), 1)
				eq(t, "eligibility.changed", len(eventsOf(d.Events, EventEligibilityChanged)), 1)
				eq(t, "left", d.Status.LeftAt.Time, t0.Add(time.Minute))
				eq(t, "pick dropped", len(after.team.People[alice].Picks), 0)
				eq(t, "no new actions", len(eventsOf(after.team.Events, EventShadowAction)), 0)
			},
		},
		{
			name: "eligibility-requires-up-next",
			ref:  refRoadmap202,
			world: func() *world {
				return newWorld(iss{ref: refRoadmap202, body: ac + "Repo: giantswarm/muster\n", assignees: []string{bob}, status: upNext, kind: story})
			},
			change: func(w *world) { w.issues[0].status = inbox },
			check: func(t *testing.T, d ItemDecision, before, after passResult) {
				eq(t, "picked before", joined(teamPicks(before.team)), "bob:giantswarm/roadmap#202")
				eq(t, "phase", d.Status.Phase, v1alpha1.PhaseBacklog)
				eq(t, "reasons", joined(d.Status.Eligible.Reasons), "NotUpNext")
				eq(t, "left", d.Status.LeftAt != nil, true)
				eq(t, "no questions", len(d.Questions), 0)
				eq(t, "no picks", len(after.team.Team.Picks), 0)
				eq(t, "no new actions", len(eventsOf(after.team.Events, EventShadowAction)), 0)
			},
		},
		{
			name: "eligibility-closed-issue",
			ref:  refRoadmap101,
			world: func() *world {
				return newWorld(iss{ref: refRoadmap101, body: ready, assignees: []string{alice}, status: upNext, kind: story})
			},
			change: func(w *world) { w.issues[0].state = v1alpha1.IssueClosed },
			check: func(t *testing.T, d ItemDecision, before, after passResult) {
				eq(t, "picked before", joined(teamPicks(before.team)), "alice:giantswarm/roadmap#101")
				eq(t, "board", d.Status.Board.Status, upNext)
				eq(t, "phase", d.Status.Phase, v1alpha1.PhaseDone)
				eq(t, "reasons", joined(d.Status.Eligible.Reasons), "Closed")
				eq(t, "left", d.Status.LeftAt != nil, true)
				eq(t, "retention check", *d.NextCheck, t0.Add(time.Minute+168*time.Hour))
				sh := after.items[refRoadmap101].Status.Shadow
				eq(t, "pick dropped", sh == nil || sh.Pick == nil, true)
				eq(t, "alice picks", len(after.team.People[alice].Picks), 0)
			},
		},
		{
			name: "board-issue-closed-means-done",
			ref:  refRoadmap101,
			world: func() *world {
				return newWorld(iss{ref: refRoadmap101, body: ready, assignees: []string{alice}, status: inProgress, kind: story})
			},
			change: func(w *world) { w.issues[0].state = v1alpha1.IssueClosed },
			check: func(t *testing.T, d ItemDecision, before, after passResult) {
				eq(t, "implementing before", before.items[refRoadmap101].Status.Phase, v1alpha1.PhaseImplementing)
				eq(t, "in flight before", before.team.Team.InFlight, 1)
				eq(t, "phase", d.Status.Phase, v1alpha1.PhaseDone)
				pc := eventsOf(d.Events, EventPhaseChanged)
				eq(t, "phase.changed", len(pc), 1)
				eq(t, "to", pc[0].Data["to"], v1alpha1.PhaseDone)
				eq(t, "in flight after", after.team.Team.InFlight, 0)
			},
		},
		{
			name: "board-dependency-link-observed",
			ref:  refRoadmap101,
			world: func() *world {
				return newWorld(iss{ref: refRoadmap101, body: ready, assignees: []string{alice}, status: upNext, kind: story})
			},
			change: func(w *world) { w.issues[0].blockedBy = []string{refRoadmap100} },
			check: func(t *testing.T, d ItemDecision, _, _ passResult) {
				eq(t, "reasons", joined(d.Status.Eligible.Reasons), "BlockedByOpenIssue")
				eq(t, "eligibility.changed", len(eventsOf(d.Events, EventEligibilityChanged)), 1)
				eq(t, "wait", d.Status.Wait.Ref, "issue:giantswarm/roadmap#100")
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := c.world()
			before := w.pass(t, t0, nil, false)
			c.change(w)
			now := t0.Add(time.Minute)
			s := *w.issue(c.ref)
			fresh := w.item(s)
			carryDecided(&fresh.Status, &before.items[c.ref].Status)
			fresh.Status.Observed = &v1alpha1.Observed{At: mt(now), Source: v1alpha1.SourceWebhook}
			d := Item(ItemView{
				Now: now, Spec: w.spec, Team: w.team, Participants: w.participants(), Item: fresh,
				Trigger: TriggerWebhook, Source: v1alpha1.SourceWebhook,
			})
			after := w.pass(t, now, before.items, false)
			c.check(t, d, before, after)
		})
	}
}

func TestItemRemoval(t *testing.T) {
	t.Parallel()
	w := newWorld(iss{ref: refRoadmap101, body: ac + "Repo: giantswarm/klaus\n", assignees: []string{alice}, status: backlog, kind: story})
	it := w.item(w.issues[0])
	cases := []struct {
		name       string
		view       ItemView
		wantDelete bool
		wantReason string
	}{
		{
			name:       "board-item-leaves-team-scope",
			view:       ItemView{Now: t0, Spec: w.spec, Item: it, Trigger: TriggerWebhook, Source: v1alpha1.SourceWebhook, LeftScope: true, ScopeReason: "team-changed"},
			wantDelete: true, wantReason: "team-changed",
		},
		{
			name:       "board-item-archived-leaves-scope",
			view:       ItemView{Now: t0, Spec: w.spec, Item: it, Trigger: TriggerWebhook, Source: v1alpha1.SourceWebhook, LeftScope: true, ScopeReason: "archived"},
			wantDelete: true, wantReason: "archived",
		},
		{
			name: "retention-kept-before-it-passes",
			view: func() ItemView {
				i := it.DeepCopy()
				i.Status.LeftAt = mtp(t0)
				return ItemView{Now: t0.Add(167 * time.Hour), Spec: w.spec, Team: w.team, Item: i, Trigger: TriggerEngine}
			}(),
		},
		{
			name: "retention-passed",
			view: func() ItemView {
				i := it.DeepCopy()
				i.Status.LeftAt = mtp(t0)
				return ItemView{Now: t0.Add(168 * time.Hour), Spec: w.spec, Team: w.team, Item: i, Trigger: TriggerEngine}
			}(),
			wantDelete: true, wantReason: ReasonRetention,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := Item(c.view)
			eq(t, "delete", d.Delete, c.wantDelete)
			rm := eventsOf(d.Events, EventItemRemoved)
			if !c.wantDelete {
				eq(t, "removed rows", len(rm), 0)
				eq(t, "next check", *d.NextCheck, t0.Add(168*time.Hour))
				return
			}
			eq(t, "removed rows", len(rm), 1)
			eq(t, "reason", rm[0].Data["reason"], c.wantReason)
			eq(t, "item", rm[0].Item, refRoadmap101)
		})
	}
}

// TestItemPhase covers the phase table of contract §1.2 for implementable items.
func TestItemPhase(t *testing.T) {
	t.Parallel()
	change := func(state, ci, review string, required int) *v1alpha1.Change {
		c := &v1alpha1.Change{}
		c.Status.PR = &v1alpha1.ChangePR{Repo: "giantswarm/klaus", Number: 55, State: state}
		c.Status.CI = &v1alpha1.ChangeCI{SHA: "a1", State: ci}
		c.Status.Review = &v1alpha1.ChangeReview{State: review, Required: required}
		return c
	}
	cases := []struct {
		name    string
		status  string
		open    bool
		changes []*v1alpha1.Change
		want    string
	}{
		{"inbox", inbox, true, nil, v1alpha1.PhaseBacklog},
		{"backlog", backlog, true, nil, v1alpha1.PhaseBacklog},
		{"up next", upNext, true, nil, v1alpha1.PhaseReady},
		{"in progress without PR", inProgress, true, nil, v1alpha1.PhaseImplementing},
		{"closed PR only", inProgress, true, []*v1alpha1.Change{change(v1alpha1.PRClosed, v1alpha1.CIGreen, v1alpha1.ReviewNone, 1)}, v1alpha1.PhaseImplementing},
		{"PR pending", inProgress, true, []*v1alpha1.Change{change(v1alpha1.PROpen, v1alpha1.CIPending, v1alpha1.ReviewNone, 1)}, v1alpha1.PhasePublished},
		{"PR green awaiting approval", inProgress, true, []*v1alpha1.Change{change(v1alpha1.PROpen, v1alpha1.CIGreen, v1alpha1.ReviewNone, 1)}, v1alpha1.PhasePublished},
		{"PR red", inProgress, true, []*v1alpha1.Change{change(v1alpha1.PROpen, v1alpha1.CIRed, v1alpha1.ReviewApproved, 1)}, v1alpha1.PhaseFixing},
		{"PR changes requested", inProgress, true, []*v1alpha1.Change{change(v1alpha1.PROpen, v1alpha1.CIPending, v1alpha1.ReviewChangesRequested, 1)}, v1alpha1.PhaseFixing},
		{"PR green approved", inProgress, true, []*v1alpha1.Change{change(v1alpha1.PROpen, v1alpha1.CIGreen, v1alpha1.ReviewApproved, 1)}, v1alpha1.PhaseMerging},
		{"PR no checks, no approval needed", inProgress, true, []*v1alpha1.Change{change(v1alpha1.PROpen, v1alpha1.CINone, v1alpha1.ReviewNone, 0)}, v1alpha1.PhaseMerging},
		{"merged", inProgress, true, []*v1alpha1.Change{change(v1alpha1.PRMerged, v1alpha1.CIGreen, v1alpha1.ReviewApproved, 1)}, v1alpha1.PhaseMerged},
		{"merged and closed", inProgress, false, []*v1alpha1.Change{change(v1alpha1.PRMerged, v1alpha1.CIGreen, v1alpha1.ReviewApproved, 1)}, v1alpha1.PhaseMerged},
		{"blocked", "Blocked / Waiting ⛔️", true, nil, v1alpha1.PhaseWaiting},
		{"validation", "Validation ☑️", true, nil, v1alpha1.PhaseValidating},
		{"validation closed by merge", "Validation ☑️", false, []*v1alpha1.Change{change(v1alpha1.PRMerged, v1alpha1.CIGreen, v1alpha1.ReviewApproved, 1)}, v1alpha1.PhaseValidating},
		{"done", done, true, nil, v1alpha1.PhaseDone},
		{"closed without merge", inProgress, false, nil, v1alpha1.PhaseDone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			eq(t, "phase", itemPhase(norm(c.status), c.open, c.changes), c.want)
		})
	}
}

func TestTruncatedLinks(t *testing.T) {
	t.Parallel()
	sub := func(ref, state string) v1alpha1.SubIssue {
		return v1alpha1.SubIssue{Ref: ref, State: state}
	}
	epic := func(total int, subs ...v1alpha1.SubIssue) *v1alpha1.ItemStatus {
		return &v1alpha1.ItemStatus{SubIssues: subs, SubIssuesTotal: total}
	}
	closed := []v1alpha1.SubIssue{sub("giantswarm/roadmap#1", v1alpha1.IssueClosed), sub("giantswarm/roadmap#2", v1alpha1.IssueClosed)}
	eq(t, "every listed sub-issue closed", epicPhase(epic(0, closed...), upNext, true), v1alpha1.PhaseDone)
	eq(t, "more sub-issues than listed", epicPhase(epic(3, closed...), upNext, true), v1alpha1.PhaseActive)
	eq(t, "open sub-issue listed", epicPhase(epic(3, sub("giantswarm/roadmap#1", v1alpha1.IssueOpen)), upNext, true),
		v1alpha1.PhasePlanned)
}
