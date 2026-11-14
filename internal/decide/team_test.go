package decide

import (
	"fmt"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/decision"
)

type teamStep struct {
	name           string
	change         func(w *world) // nil for the first pass
	gateEverything bool
	check          func(t *testing.T, r passResult)
}

// TestTeamScenarios ports the pick, gate, shadow and decision scenarios: each step changes the world as
// the scenario's tools do and runs a decision pass (decide.Item over every Item, then decide.Team) on
// what the previous one wrote.
func TestTeamScenarios(t *testing.T) {
	t.Parallel()
	klaus := ac + "Repo: giantswarm/klaus\n"
	muster := ac + "Repo: giantswarm/muster\n"
	cases := []struct {
		name  string
		world func() *world
		steps []teamStep
	}{
		{
			name: "pick-unassigned-items-for-free-people",
			world: func() *world {
				return newWorld(
					iss{ref: refRoadmap101, body: klaus, status: upNext, kind: story, prio: 1},
					iss{ref: refRoadmap102, body: muster, status: upNext, kind: story, prio: 2},
				)
			},
			steps: []teamStep{{check: func(t *testing.T, r passResult) {
				eq(t, "wip", r.team.Team.WIP.Items, 0)
				eq(t, "picks", joined(teamPicks(r.team)), "alice:giantswarm/roadmap#101,bob:giantswarm/roadmap#102")
				eq(t, "held", len(r.team.Team.Held), 0)
				a, b := r.items[refRoadmap101].Status.Shadow, r.items[refRoadmap102].Status.Shadow
				eq(t, "alice pick", *a.Pick, v1alpha1.ShadowPick{Person: alice, Rank: 1})
				eq(t, "bob pick", *b.Pick, v1alpha1.ShadowPick{Person: bob, Rank: 2})
				eq(t, "alice actions", joined(actionTypes(a)), "assign,set-status,start-run")
				eq(t, "assign alice", a.Actions[0].Args["login"], alice)
				eq(t, "set-status", a.Actions[1].Args["status"], StatusInProgress)
				eq(t, "start-run", a.Actions[2].Args["purpose"], "implement")
				eq(t, "assign bob", b.Actions[0].Args["login"], bob)
				eq(t, "alice free", r.team.People[alice].Free.Items, 1)
				eq(t, "alice picks", joined(r.team.People[alice].Picks), refRoadmap101)
				eq(t, "six would-actions", len(eventsOf(r.team.Events, EventShadowAction)), 6)
				eq(t, "not executed", len(r.items[refRoadmap101].Status.Assignees), 0)
				eq(t, "still ready", r.items[refRoadmap101].Status.Phase, v1alpha1.PhaseReady)
			}}},
		},
		{
			name: "pick-assigned-item-only-for-its-assignee",
			world: func() *world {
				return newWorld(
					iss{ref: refRoadmap101, body: klaus, assignees: []string{bob}, status: upNext, kind: story, prio: 1},
					iss{ref: refRoadmap102, body: muster, status: upNext, kind: story, prio: 2},
				)
			},
			steps: []teamStep{{check: func(t *testing.T, r passResult) {
				eq(t, "picks", joined(teamPicks(r.team)), "alice:giantswarm/roadmap#102,bob:giantswarm/roadmap#101")
				b := r.items[refRoadmap101].Status.Shadow
				eq(t, "bob pick", *b.Pick, v1alpha1.ShadowPick{Person: bob, Rank: 1})
				eq(t, "no assign", joined(actionTypes(b)), "set-status,start-run")
				a := r.items[refRoadmap102].Status.Shadow
				eq(t, "alice pick", *a.Pick, v1alpha1.ShadowPick{Person: alice, Rank: 2})
				eq(t, "alice actions", joined(actionTypes(a)), "assign,set-status,start-run")
			}}},
		},
		{
			name: "pick-active-epic-items-first",
			world: func() *world {
				return newWorld(
					iss{ref: refRoadmap90, assignees: []string{alice}, status: inProgress, kind: epic},
					iss{ref: refRoadmap110, body: klaus, assignees: []string{alice}, state: v1alpha1.IssueClosed, parent: refRoadmap90, status: done, kind: story},
					iss{ref: refRoadmap111, body: klaus, createdAt: "2026-11-12T10:00:00Z", parent: refRoadmap90, status: upNext, kind: story},
					iss{ref: refRoadmap102, body: muster, createdAt: "2026-11-01T10:00:00Z", status: upNext, kind: story, prio: 1},
				)
			},
			steps: []teamStep{{check: func(t *testing.T, r passResult) {
				eq(t, "epic", r.items[refRoadmap90].Status.Phase, v1alpha1.PhaseActive)
				eq(t, "#111", *r.items[refRoadmap111].Status.Shadow.Pick, v1alpha1.ShadowPick{Person: alice, Rank: 1})
				eq(t, "#102", *r.items[refRoadmap102].Status.Shadow.Pick, v1alpha1.ShadowPick{Person: bob, Rank: 2})
				eq(t, "picks", joined(teamPicks(r.team)), "alice:giantswarm/roadmap#111,bob:giantswarm/roadmap#102")
			}}},
		},
		{
			name: "pick-epic-sub-items-only-for-epic-owner",
			world: func() *world {
				return newWorld(
					iss{ref: refRoadmap90, assignees: []string{alice}, status: inProgress, kind: epic},
					iss{ref: refRoadmap110, body: klaus, assignees: []string{alice}, state: v1alpha1.IssueClosed, parent: refRoadmap90, status: done, kind: story},
					iss{ref: refRoadmap111, body: klaus, parent: refRoadmap90, status: upNext, kind: story, prio: 1},
					iss{ref: "giantswarm/roadmap#112", body: muster, parent: refRoadmap90, status: upNext, kind: story, prio: 2},
					iss{ref: refRoadmap102, body: muster, status: upNext, kind: story, prio: 1},
				)
			},
			steps: []teamStep{{check: func(t *testing.T, r passResult) {
				eq(t, "picks", joined(teamPicks(r.team)), "alice:giantswarm/roadmap#111,bob:giantswarm/roadmap#102")
				eq(t, "held", len(r.team.Team.Held), 0)
				s := r.items["giantswarm/roadmap#112"].Status
				eq(t, "owner", s.Owner, alice)
				eq(t, "eligible", s.Eligible.Value, true)
				eq(t, "pick", s.Shadow.Pick == nil && s.Shadow.HeldBy == nil && len(s.Shadow.Actions) == 0, true)
				eq(t, "#102", *r.items[refRoadmap102].Status.Shadow.Pick, v1alpha1.ShadowPick{Person: bob, Rank: 3})
			}}},
		},
		{
			name: "pick-order-priority-then-age",
			world: func() *world {
				return newWorld(
					iss{ref: refRoadmap101, body: klaus, createdAt: "2026-11-12T10:00:00Z", status: upNext, kind: story, prio: 1},
					iss{ref: refRoadmap102, body: muster, createdAt: "2026-11-05T10:00:00Z", status: upNext, kind: story, prio: 2},
					iss{ref: refRoadmap103, body: klaus, createdAt: "2026-11-10T10:00:00Z", status: upNext, kind: story, prio: 2},
					iss{ref: "giantswarm/roadmap#104", body: muster, createdAt: "2026-10-01T10:00:00Z", status: upNext, kind: task},
				)
			},
			steps: []teamStep{
				{check: func(t *testing.T, r passResult) {
					eq(t, "picks", joined(teamPicks(r.team)), "alice:giantswarm/roadmap#101,bob:giantswarm/roadmap#102")
				}},
				{change: func(w *world) { w.issue(refRoadmap101).status = backlog }, check: func(t *testing.T, r passResult) {
					eq(t, "picks", joined(teamPicks(r.team)), "alice:giantswarm/roadmap#102,bob:giantswarm/roadmap#103")
				}},
				{change: func(w *world) { w.issue(refRoadmap102).status = backlog }, check: func(t *testing.T, r passResult) {
					eq(t, "picks", joined(teamPicks(r.team)), "alice:giantswarm/roadmap#103,bob:giantswarm/roadmap#104")
					eq(t, "#104", *r.items["giantswarm/roadmap#104"].Status.Shadow.Pick, v1alpha1.ShadowPick{Person: bob, Rank: 2})
				}},
			},
		},
		{
			name: "pick-recomputed-when-priority-changes",
			world: func() *world {
				return newWorld(
					iss{ref: refRoadmap101, body: klaus, status: upNext, kind: story, prio: 2},
					iss{ref: refRoadmap102, body: muster, status: upNext, kind: story, prio: 3},
				)
			},
			steps: []teamStep{
				{check: func(t *testing.T, r passResult) {
					eq(t, "picks", joined(teamPicks(r.team)), "alice:giantswarm/roadmap#101,bob:giantswarm/roadmap#102")
				}},
				{change: func(w *world) { w.issue(refRoadmap102).prio = 1 }, check: func(t *testing.T, r passResult) {
					eq(t, "picks", joined(teamPicks(r.team)), "alice:giantswarm/roadmap#102,bob:giantswarm/roadmap#101")
					eq(t, "#102", *r.items[refRoadmap102].Status.Shadow.Pick, v1alpha1.ShadowPick{Person: alice, Rank: 1})
					eq(t, "each changed pick once", len(eventsOf(r.team.Events, EventShadowPick)), 2)
				}},
			},
		},
		{
			name: "pick-recorded-once-not-executed",
			world: func() *world {
				return newWorld(
					iss{ref: refRoadmap101, body: klaus, status: upNext, kind: story, prio: 1},
					iss{ref: "giantswarm/roadmap#150", body: muster, assignees: []string{"carol"}, status: upNext, kind: task},
				)
			},
			steps: []teamStep{
				{check: func(t *testing.T, r passResult) {
					p := eventsOf(r.team.Events, EventShadowPick)
					eq(t, "one pick", len(p), 1)
					eq(t, "pick item", p[0].Item, refRoadmap101)
					eq(t, "pick person", p[0].Data["person"], alice)
					eq(t, "three actions", len(eventsFor(r.team.Events, refRoadmap101, EventShadowAction)), 3)
					eq(t, "person", joined(r.team.People[alice].Picks), refRoadmap101)
					eq(t, "decided at", r.team.Team.LastDecisionAt.Time, t0)
				}},
				{change: func(*world) {}, check: func(t *testing.T, r passResult) {
					eq(t, "decided again", r.team.Team.LastDecisionAt.Time, t0.Add(time.Minute))
					eq(t, "picks", joined(teamPicks(r.team)), "alice:giantswarm/roadmap#101")
					eq(t, "no pick row", len(eventsOf(r.team.Events, EventShadowPick)), 0)
				}},
				{change: func(w *world) { w.issue("giantswarm/roadmap#150").prio = 1 }, check: func(t *testing.T, r passResult) {
					eq(t, "no rows", len(r.team.Events), 0)
				}},
				{change: func(w *world) {
					w.issue(refRoadmap101).title = "Retry transient errors in the klaus API client"
				}, check: func(t *testing.T, r passResult) {
					eq(t, "no rows", len(r.team.Events), 0)
					eq(t, "still alice", r.items[refRoadmap101].Status.Shadow.Pick.Person, alice)
				}},
			},
		},
		{
			name: "pick-unblocking-work-first",
			world: func() *world {
				return newWorld(
					iss{ref: "giantswarm/roadmap#105", body: klaus, assignees: []string{bob}, blockedBy: []string{refRoadmap120}, status: inProgress, kind: story},
					iss{ref: refRoadmap120, body: muster, createdAt: "2026-11-14T10:00:00Z", status: upNext, kind: task},
					iss{ref: refRoadmap90, assignees: []string{alice}, status: inProgress, kind: epic},
					iss{ref: refRoadmap110, body: klaus, assignees: []string{alice}, state: v1alpha1.IssueClosed, parent: refRoadmap90, status: done, kind: story},
					iss{ref: refRoadmap111, body: klaus, createdAt: "2026-11-01T10:00:00Z", parent: refRoadmap90, status: upNext, kind: story, prio: 1},
				)
			},
			steps: []teamStep{{check: func(t *testing.T, r passResult) {
				eq(t, "#120", *r.items[refRoadmap120].Status.Shadow.Pick, v1alpha1.ShadowPick{Person: alice, Rank: 1})
				eq(t, "#111", r.items[refRoadmap111].Status.Shadow.Pick == nil, true)
				eq(t, "picks", joined(teamPicks(r.team)), "alice:giantswarm/roadmap#120")
			}}},
		},
		{
			name: "gate-epic-wip-holds-new-epic",
			world: func() *world {
				w := newWorld(
					iss{ref: refRoadmap90, assignees: []string{alice}, status: upNext, kind: epic},
					iss{ref: refRoadmap111, body: klaus, parent: refRoadmap90, status: upNext, kind: story, prio: 1},
					iss{ref: "giantswarm/roadmap#95", assignees: []string{alice}, status: upNext, kind: epic},
					iss{ref: "giantswarm/roadmap#121", body: muster, parent: "giantswarm/roadmap#95", status: upNext, kind: story, prio: 2},
					iss{ref: refRoadmap102, body: muster, assignees: []string{alice}, status: upNext, kind: task, prio: 3},
				)
				w.people[0] = person(alice, 2, 1)
				return w
			},
			steps: []teamStep{{check: func(t *testing.T, r passResult) {
				eq(t, "#90", r.items[refRoadmap90].Status.Phase, v1alpha1.PhasePlanned)
				eq(t, "#95", r.items["giantswarm/roadmap#95"].Status.Phase, v1alpha1.PhasePlanned)
				a := r.team.People[alice]
				eq(t, "free", a.Free, v1alpha1.WIP{Items: 2, Epics: 1})
				eq(t, "alice picks", joined(a.Picks), "giantswarm/roadmap#111,giantswarm/roadmap#102")
				eq(t, "alice held", a.Held, []v1alpha1.PersonHeld{{Item: "giantswarm/roadmap#121", Gate: v1alpha1.GateEpicWIP}})
				eq(t, "team wip", r.team.Team.WIP.Epics, 0)
				eq(t, "picks", joined(teamPicks(r.team)), "alice:giantswarm/roadmap#111,alice:giantswarm/roadmap#102")
				eq(t, "held", joined(teamHeld(r.team)), "alice:giantswarm/roadmap#121:EpicWIP")
				s := r.items["giantswarm/roadmap#121"].Status
				eq(t, "owner", s.Owner, alice)
				eq(t, "heldBy", s.Shadow.HeldBy.Person+"/"+s.Shadow.HeldBy.Gate, "alice/EpicWIP")
				eq(t, "no actions", len(s.Shadow.Actions), 0)
				n := r.items[refRoadmap102].Status.Shadow
				eq(t, "#102", *n.Pick, v1alpha1.ShadowPick{Person: alice, Rank: 3})
				eq(t, "#102 actions", joined(actionTypes(n)), "set-status,start-run")
			}}},
		},
		{
			name: "gate-limit-change-applies-at-once",
			world: func() *world {
				w := newWorld(
					iss{ref: refRoadmap101, body: klaus, status: upNext, kind: story, prio: 1},
					iss{ref: refRoadmap102, body: muster, status: upNext, kind: story, prio: 2},
				)
				w.team.Spec.Cap = &v1alpha1.Cap{Items: intp(1)}
				return w
			},
			steps: []teamStep{
				{check: func(t *testing.T, r passResult) {
					h := r.items[refRoadmap102].Status.Shadow.HeldBy
					eq(t, "held", h.Person+"/"+h.Gate, "bob/TeamCap")
				}},
				{change: func(w *world) { w.team.Spec.Cap.Items = intp(2) }, check: func(t *testing.T, r passResult) {
					s := r.items[refRoadmap102].Status.Shadow
					eq(t, "picked", s.Pick.Person, bob)
					eq(t, "not held", s.HeldBy == nil, true)
					eq(t, "actions", joined(actionTypes(s)), "assign,set-status,start-run")
					eq(t, "picks", joined(teamPicks(r.team)), "alice:giantswarm/roadmap#101,bob:giantswarm/roadmap#102")
					eq(t, "held", len(r.team.Team.Held), 0)
					eq(t, "action rows", len(eventsFor(r.team.Events, refRoadmap102, EventShadowAction)), 3)
				}},
			},
		},
		{
			name: "gate-person-wip-bounds-picks",
			world: func() *world {
				w := newWorld(
					iss{ref: refRoadmap101, body: klaus, status: upNext, kind: story, prio: 1},
					iss{ref: refRoadmap102, body: muster, status: upNext, kind: story, prio: 2},
					iss{ref: refRoadmap103, body: ac + "Repo: giantswarm/klaus-gateway\n", status: upNext, kind: story, prio: 3},
					iss{ref: "giantswarm/roadmap#104", body: ac + "Repo: giantswarm/mcp-oauth\n", status: upNext, kind: task, prio: 4},
				)
				w.people[0] = person(alice, 2, 1)
				return w
			},
			steps: []teamStep{{check: func(t *testing.T, r passResult) {
				eq(t, "picks", joined(teamPicks(r.team)), "alice:giantswarm/roadmap#101,bob:giantswarm/roadmap#102,alice:giantswarm/roadmap#103")
				eq(t, "held", len(r.team.Team.Held), 0)
				eq(t, alice, joined(r.team.People[alice].Picks), "giantswarm/roadmap#101,giantswarm/roadmap#103")
				eq(t, "alice free", r.team.People[alice].Free.Items, 2)
				eq(t, bob, joined(r.team.People[bob].Picks), refRoadmap102)
				eq(t, "#103", *r.items[refRoadmap103].Status.Shadow.Pick, v1alpha1.ShadowPick{Person: alice, Rank: 3})
				s := r.items["giantswarm/roadmap#104"].Status
				eq(t, "#104 eligible", s.Eligible.Value, true)
				eq(t, "#104 untouched", s.Shadow.Pick == nil && s.Shadow.HeldBy == nil && len(s.Shadow.Actions) == 0, true)
				eq(t, "pick rows", len(eventsOf(r.team.Events, EventShadowPick)), 3)
				eq(t, "held rows", len(eventsOf(r.team.Events, EventShadowHeld)), 0)
			}}},
		},
		{
			name: "gate-repo-concurrency-disjoint-footprints-pass",
			world: func() *world {
				return newWorld(
					iss{ref: refRoadmap101, body: klaus + "Footprint: pkg/client\n", status: upNext, kind: story, prio: 1},
					iss{ref: refRoadmap102, body: klaus + "Footprint: pkg/metrics\n", status: upNext, kind: story, prio: 2},
				)
			},
			steps: []teamStep{{check: func(t *testing.T, r passResult) {
				qs := questionsOf(r.team.Questions, decision.PointParallelSafe, refRoadmap102)
				eq(t, "asked", len(qs), 1)
				eq(t, "other", qs[0].Other, refRoadmap101)
				eq(t, "mode", qs[0].Mode, v1alpha1.DecisionModeShadow)
				eq(t, "rule", qs[0].Rule, "yes")
				s := r.items[refRoadmap102].Status
				eq(t, "footprint", joined(s.Footprint), "pkg/metrics")
				eq(t, "pick", *s.Shadow.Pick, v1alpha1.ShadowPick{Person: bob, Rank: 2})
				eq(t, "actions", joined(actionTypes(s.Shadow)), "assign,set-status,start-run")
				eq(t, "held", len(r.team.Team.Held), 0)
			}}},
		},
		{
			name: "gate-repo-concurrency-sequential-by-default",
			world: func() *world {
				return newWorld(
					iss{ref: refRoadmap101, body: klaus, status: upNext, kind: story, prio: 1},
					iss{ref: refRoadmap102, body: klaus, status: upNext, kind: story, prio: 2},
					iss{ref: refRoadmap103, body: muster, status: upNext, kind: story, prio: 3},
				)
			},
			steps: []teamStep{{check: func(t *testing.T, r passResult) {
				qs := questionsOf(r.team.Questions, decision.PointParallelSafe, refRoadmap102)
				eq(t, "asked", len(qs), 1)
				eq(t, "other", qs[0].Other, refRoadmap101)
				eq(t, "rule", qs[0].Rule, "no")
				s := r.items[refRoadmap102].Status
				eq(t, "targets", joined(s.TargetRepos), "giantswarm/klaus")
				eq(t, "footprint", len(s.Footprint), 0)
				eq(t, "pick", s.Shadow.Pick == nil, true)
				eq(t, "heldBy", s.Shadow.HeldBy.Person+"/"+s.Shadow.HeldBy.Gate, "bob/RepoConcurrency")
				eq(t, "no actions", len(s.Shadow.Actions), 0)
				eq(t, "#103", *r.items[refRoadmap103].Status.Shadow.Pick, v1alpha1.ShadowPick{Person: bob, Rank: 3})
				eq(t, "picks", joined(teamPicks(r.team)), "alice:giantswarm/roadmap#101,bob:giantswarm/roadmap#103")
				eq(t, "held", joined(teamHeld(r.team)), "bob:giantswarm/roadmap#102:RepoConcurrency")
			}}},
		},
		{
			name: "gate-team-cap-holds-picks",
			world: func() *world {
				w := newWorld(
					iss{ref: refRoadmap101, body: klaus, status: upNext, kind: story, prio: 1},
					iss{ref: refRoadmap102, body: muster, status: upNext, kind: story, prio: 2},
				)
				w.team.Spec.Cap = &v1alpha1.Cap{Items: intp(1)}
				return w
			},
			steps: []teamStep{{check: func(t *testing.T, r passResult) {
				eq(t, "picks", joined(teamPicks(r.team)), "alice:giantswarm/roadmap#101")
				eq(t, "held", joined(teamHeld(r.team)), "bob:giantswarm/roadmap#102:TeamCap")
				eq(t, "bob picks", len(r.team.People[bob].Picks), 0)
				eq(t, "bob held", r.team.People[bob].Held, []v1alpha1.PersonHeld{{Item: refRoadmap102, Gate: v1alpha1.GateTeamCap}})
				s := r.items[refRoadmap102].Status
				eq(t, "eligible", s.Eligible.Value, true)
				eq(t, "no actions", len(s.Shadow.Actions), 0)
				h := eventsFor(r.team.Events, refRoadmap102, EventShadowHeld)
				eq(t, "held rows", len(h), 1)
				eq(t, "held data", h[0].Data, map[string]any{"person": bob, "gate": "TeamCap"})
				eq(t, "pick rows", len(eventsOf(r.team.Events, EventShadowPick)), 1)
				eq(t, "no actions rows", len(eventsFor(r.team.Events, refRoadmap102, EventShadowAction)), 0)
			}}},
		},
		{
			name: "gate-unblocking-work-exempt",
			world: func() *world {
				w := newWorld(
					iss{ref: refRoadmap100, body: muster, assignees: []string{bob}, blockedBy: []string{refRoadmap120}, status: inProgress, kind: story},
					iss{ref: refRoadmap120, body: klaus, assignees: []string{bob}, status: upNext, kind: task},
					iss{ref: refRoadmap101, body: klaus, status: upNext, kind: story, prio: 1},
				)
				w.team.Spec.Cap = &v1alpha1.Cap{Items: intp(1)}
				return w
			},
			steps: []teamStep{{check: func(t *testing.T, r passResult) {
				eq(t, "in flight", r.team.Team.InFlight, 1)
				eq(t, "picks", joined(teamPicks(r.team)), "alice:giantswarm/roadmap#101,bob:giantswarm/roadmap#120")
				eq(t, "held", len(r.team.Team.Held), 0)
				s := r.items[refRoadmap120].Status.Shadow
				eq(t, "#120", *s.Pick, v1alpha1.ShadowPick{Person: bob, Rank: 1})
				eq(t, "actions", joined(actionTypes(s)), "set-status,start-run")
				eq(t, "held rows", len(eventsOf(r.team.Events, EventShadowHeld)), 0)
			}}},
		},
		{
			name: "invariant-gate-holding-exempt-work-detected",
			world: func() *world {
				return newWorld(
					iss{ref: refRoadmap101, body: klaus, assignees: []string{alice}, blockedBy: []string{refRoadmap102}, status: inProgress, kind: story},
					iss{ref: refRoadmap102, body: klaus, assignees: []string{alice}, status: upNext, kind: story},
				)
			},
			steps: []teamStep{{gateEverything: true, check: func(t *testing.T, r passResult) {
				s := r.items[refRoadmap102].Status
				eq(t, "phase", s.Phase, v1alpha1.PhaseReady)
				eq(t, "eligible", s.Eligible.Value, true)
				h := eventsFor(r.team.Events, refRoadmap102, EventShadowHeld)
				eq(t, "held rows", len(h), 1)
				eq(t, "held for", h[0].Data["person"], alice)
			}}},
		},
		{
			name: "decision-next-shadow-recorded",
			world: func() *world {
				return newWorld(
					iss{ref: refRoadmap101, body: klaus, assignees: []string{alice}, status: upNext, kind: story, prio: 1},
					iss{ref: refRoadmap102, body: muster, assignees: []string{alice}, status: upNext, kind: story, prio: 2},
				)
			},
			steps: []teamStep{{check: func(t *testing.T, r passResult) {
				qs := questionsOf(r.team.Questions, decision.PointNext, "")
				eq(t, "one next?", len(qs), 1)
				eq(t, "subject", qs[0].Subject, alice)
				eq(t, "mode", qs[0].Mode, v1alpha1.DecisionModeShadow)
				eq(t, "rule", qs[0].Rule, refRoadmap101)
				eq(t, "answers", joined(qs[0].Answers), "giantswarm/roadmap#101,giantswarm/roadmap#102")
				eq(t, "alice picks", joined(r.team.People[alice].Picks), refRoadmap101)
				eq(t, "#102", r.items[refRoadmap102].Status.Shadow.Pick == nil, true)
				replayed, err := decision.Replay(baseSpec(), decision.PointNext, qs[0].Inputs)
				eq(t, "replay err", err, nil)
				eq(t, "replay", replayed, qs[0].Rule)
			}}},
		},
		{
			name: "decision-mode-rule-skips-jev",
			world: func() *world {
				w := newWorld(iss{ref: refRoadmap101, body: klaus, status: upNext, kind: story})
				w.spec.DecisionPoints[decision.PointReady] = "rule"
				return w
			},
			steps: []teamStep{{check: func(t *testing.T, r passResult) {
				eq(t, "ready? mode", questionsOf(r.decisions[refRoadmap101].Questions, decision.PointReady, "")[0].Mode, "rule")
				eq(t, "next? mode", questionsOf(r.team.Questions, decision.PointNext, "")[0].Mode, v1alpha1.DecisionModeShadow)
				eq(t, "eligible", r.items[refRoadmap101].Status.Eligible.Value, true)
			}}},
		},
		{
			name: "decision-jev-gets-only-needed-fields",
			world: func() *world {
				return newWorld(
					iss{ref: refRoadmap101, title: "Retry transient errors", body: klaus, assignees: []string{alice}, status: upNext, kind: story},
					iss{ref: refRoadmap102, title: "Count muster tool calls", body: muster, assignees: []string{bob}, status: upNext, kind: story},
				)
			},
			steps: []teamStep{{check: func(t *testing.T, r passResult) {
				for _, ref := range []string{refRoadmap101, refRoadmap102} {
					q := questionsOf(r.decisions[ref].Questions, decision.PointReady, ref)[0]
					eq(t, ref+" fields", len(q.Fields), 2)
					eq(t, ref+" title", q.Fields["title"], r.items[ref].Status.Issue.Title)
					eq(t, ref+" body", q.Fields["body"], r.items[ref].Status.Issue.Body)
					_, hasAssignees := q.Inputs["assignees"]
					eq(t, ref+" no assignees", hasAssignees, false)
				}
			}}},
		},
		{
			name: "shadow-would-actions-recorded-once",
			world: func() *world {
				return newWorld(
					iss{ref: refRoadmap101, body: klaus, assignees: []string{alice}, status: upNext, kind: story},
					iss{ref: refRoadmap102, body: "We should look at sessions at some point.\n", assignees: []string{bob}, status: upNext, kind: story},
				)
			},
			steps: []teamStep{
				{check: func(t *testing.T, r passResult) {
					todo := eventsFor(r.team.Events, refRoadmap102, EventShadowAction)
					eq(t, "todo rows", len(todo), 1)
					eq(t, "todo type", todo[0].Data["type"], v1alpha1.ActionTodo)
					eq(t, "todo to", todo[0].Data["args"].(map[string]any)["to"], bob)
					eq(t, "pick", r.items[refRoadmap101].Status.Shadow.Pick.Person, alice)
					eq(t, "pick rows", len(eventsFor(r.team.Events, refRoadmap101, EventShadowAction)), 2)
				}},
				{change: func(w *world) { w.issue(refRoadmap102).title = "Look at klaus sessions" }, check: func(t *testing.T, r passResult) {
					eq(t, "nothing again", len(eventsOf(r.team.Events, EventShadowAction)), 0)
					eq(t, "one todo", joined(actionTypes(r.items[refRoadmap102].Status.Shadow)), "todo")
					eq(t, "decided again", len(questionsOf(r.decisions[refRoadmap102].Questions, decision.PointReady, "")), 1)
				}},
				{change: func(*world) {}, check: func(t *testing.T, r passResult) {
					eq(t, "nothing again", len(eventsOf(r.team.Events, EventShadowAction)), 0)
				}},
			},
		},
		{
			name: "decision-bug-severity-shadow",
			world: func() *world {
				return newWorld(iss{ref: "giantswarm/roadmap#130", body: "Sessions end with connection reset.\nRepo: giantswarm/klaus\n", status: inbox, kind: bug})
			},
			steps: []teamStep{{check: func(t *testing.T, r passResult) {
				s := r.items["giantswarm/roadmap#130"].Status
				eq(t, "phase", s.Phase, v1alpha1.PhaseBacklog)
				eq(t, "eligible", s.Eligible.Value, false)
				eq(t, "actions", joined(actionTypes(s.Shadow)), "set-severity")
				eq(t, "severity", s.Shadow.Severity, "medium")
				eq(t, "one row", len(eventsFor(r.team.Events, "giantswarm/roadmap#130", EventShadowAction)), 1)
				q := questionsOf(r.decisions["giantswarm/roadmap#130"].Questions, decision.PointBugSeverity, "")
				eq(t, "rule", q[0].Rule, "medium")
				eq(t, "answers", joined(q[0].Answers), "critical,high,medium,low")
			}}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := c.world()
			var prev map[string]*v1alpha1.Item
			now := t0
			for i, s := range c.steps {
				if s.change != nil {
					s.change(w)
				}
				r := w.pass(t, now, prev, s.gateEverything)
				t.Run(stepName(i, s.name), func(t *testing.T) { s.check(t, r) })
				prev = r.items
				now = now.Add(time.Minute)
			}
		})
	}
}

func stepName(i int, name string) string {
	if name != "" {
		return name
	}
	return fmt.Sprintf("step-%c", '1'+i)
}

// TestTeamEventSource: the shadow rows carry the pass's source (a webhook-triggered pass records
// source webhook).
func TestTeamEventSource(t *testing.T) {
	t.Parallel()
	w := newWorld(iss{ref: refRoadmap101, body: ac + "Repo: giantswarm/klaus\n", status: upNext, kind: story})
	items, _ := w.decided(t, t0, nil)
	td := Team(TeamView{Now: t0, Spec: w.spec, Team: w.team, People: w.people, Items: []*v1alpha1.Item{items[refRoadmap101]}, Source: v1alpha1.SourceWebhook})
	for _, e := range td.Events {
		eq(t, e.Type+" source", e.Source, v1alpha1.SourceWebhook)
		eq(t, e.Type+" team", e.Team, "bumblebee")
	}
	eq(t, "rows", len(td.Events), 4) // pick, assign, set-status, start-run
	keys := map[string]bool{}
	for _, a := range td.Shadows[refRoadmap101].Actions {
		if keys[a.Key] {
			t.Errorf("duplicate key %s", a.Key)
		}
		keys[a.Key] = true
	}
}
