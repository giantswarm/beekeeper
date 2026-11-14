package decide

import (
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/decision"
	"github.com/giantswarm/beekeeper/internal/history"
)

// t0 is the base fixture's clock: Monday 2026-11-16 09:00 UTC.
var t0 = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

const (
	upNext     = "Up Next ➡️"
	inProgress = "In Progress ⛏️"
	inbox      = "Inbox 📥"
	backlog    = "Backlog 📦"
	done       = "Done ✅"
	story      = "Story 📑"
	task       = "Task 🗒️"
	bug        = "Bug 🐞"
	epic       = "Epic 🎯"
	bumblebee  = "Bumblebee🐝"
	ac         = "### Acceptance criteria\n- [ ] it works\n"
)

// People and items of the fixtures.
const (
	alice = "alice"
	bob   = "bob"

	refRoadmap90  = "giantswarm/roadmap#90"
	refRoadmap100 = "giantswarm/roadmap#100"
	refRoadmap101 = "giantswarm/roadmap#101"
	refRoadmap102 = "giantswarm/roadmap#102"
	refRoadmap103 = "giantswarm/roadmap#103"
	refRoadmap110 = "giantswarm/roadmap#110"
	refRoadmap111 = "giantswarm/roadmap#111"
	refRoadmap120 = "giantswarm/roadmap#120"
	refRoadmap201 = "giantswarm/roadmap#201"
	refRoadmap202 = "giantswarm/roadmap#202"
	refRoadmap206 = "giantswarm/roadmap#206"
)

func at(d time.Duration) time.Time { return t0.Add(d) }

func dur(d time.Duration) metav1.Duration { return metav1.Duration{Duration: d} }

func intp(i int) *int { return &i }

// baseSpec is the Installation of fixtures/base.yaml.
func baseSpec() *v1alpha1.InstallationSpec {
	return &v1alpha1.InstallationSpec{
		Org:           "giantswarm",
		Board:         v1alpha1.BoardSpec{Project: 273},
		Mode:          v1alpha1.ModeDryRun,
		Retry:         v1alpha1.RetrySpec{Initial: dur(10 * time.Second), Cap: dur(5 * time.Minute)},
		ItemRetention: dur(168 * time.Hour),
		JEV:           v1alpha1.JEVSpec{ConfidenceFloor: 0.5},
		DecisionPoints: map[string]string{
			decision.PointReady: v1alpha1.DecisionModeShadow, decision.PointNext: v1alpha1.DecisionModeShadow,
			decision.PointParallelSafe: v1alpha1.DecisionModeShadow, decision.PointBugSeverity: v1alpha1.DecisionModeShadow,
		},
		Waits: v1alpha1.WaitsSpec{
			CI:                v1alpha1.CIWaitSpec{DeadlineFactor: 3, DefaultDeadline: dur(time.Hour), MinHistory: 3, AtDeadline: v1alpha1.AtDeadlineReobserve},
			Review:            v1alpha1.FixedWaitSpec{Deadline: dur(48 * time.Hour), AtDeadline: v1alpha1.AtDeadlineReobserve},
			Dependency:        v1alpha1.FixedWaitSpec{Deadline: dur(168 * time.Hour), AtDeadline: v1alpha1.AtDeadlineReobserve},
			MergeResult:       v1alpha1.FixedWaitSpec{Deadline: dur(2 * time.Hour), AtDeadline: v1alpha1.AtDeadlineReobserve},
			Grace:             dur(5 * time.Minute),
			OverdueBackoffCap: dur(10 * time.Minute),
		},
		Ready: v1alpha1.ReadySpec{
			AcceptanceCriteria: `(?im)^#{1,4}\s*acceptance criteria`,
			TargetRepo:         `(?im)^(repo|repository):\s*([\w.-]+/[\w.-]+)`,
			PlanningRepos:      []string{"giantswarm/roadmap", "giantswarm/giantswarm"},
			Todo:               "refine with `/gs-plans:gs-plan`",
		},
		Footprint: `(?im)^(footprint|paths):\s*(.+)$`,
	}
}

func baseTeam() *v1alpha1.Team {
	t := &v1alpha1.Team{Spec: v1alpha1.TeamSpec{
		BoardOption: bumblebee, GithubTeam: "team-bumblebee",
		OptIn: v1alpha1.OptIn{Work: []string{"implementable"}},
	}}
	t.Name = "bumblebee"
	return t
}

func person(login string, items, epics int) *v1alpha1.Person {
	p := &v1alpha1.Person{Spec: v1alpha1.PersonSpec{Login: login, Teams: []string{"bumblebee"}, WIP: v1alpha1.WIP{Items: items, Epics: epics}}}
	p.Name = login
	return p
}

// iss is a fixture issue, as in a scenario's pre_configuration.
type iss struct {
	ref, title, body string
	assignees        []string
	state            string // open unless set
	createdAt        string
	parent           string
	blockedBy        []string
	status, kind     string // board; no board when status is empty
	team             string // Bumblebee unless set
	prio             int    // 0: unset
}

// world is a scenario's pre_configuration, reduced to what the pure core sees.
type world struct {
	spec    *v1alpha1.InstallationSpec
	team    *v1alpha1.Team
	people  []*v1alpha1.Person
	issues  []iss
	changes map[string][]*v1alpha1.Change // by issue ref
}

func newWorld(issues ...iss) *world {
	return &world{
		spec:   baseSpec(),
		team:   baseTeam(),
		people: []*v1alpha1.Person{person(alice, 1, 1), person(bob, 1, 1)},
		issues: issues,
	}
}

func (w *world) participants() []string {
	var out []string
	for _, p := range w.people {
		out = append(out, p.Spec.Login)
	}
	return out
}

func (w *world) issue(ref string) *iss {
	for i := range w.issues {
		if w.issues[i].ref == ref {
			return &w.issues[i]
		}
	}
	return nil
}

// tracked reports whether a fixture issue becomes an Item: on the board, in the active statuses (and
// Inbox for bugs), open.
func (s iss) tracked() bool {
	if s.status == "" {
		return false
	}
	return activeStatus(norm(s.status), norm(s.kind)) && (s.state == "" || s.state == v1alpha1.IssueOpen)
}

// item builds the observed Item of a fixture issue.
func (w *world) item(s iss) *v1alpha1.Item {
	r, _ := v1alpha1.ParseRef(s.ref)
	state := s.state
	if state == "" {
		state = v1alpha1.IssueOpen
	}
	it := &v1alpha1.Item{}
	it.Name = v1alpha1.ItemName(s.ref)
	it.Status.Issue = &v1alpha1.ItemIssue{Repo: r.FullRepo(), Number: r.Number, Title: s.title, State: state, Body: s.body}
	if s.createdAt != "" {
		ct, err := time.Parse(time.RFC3339, s.createdAt)
		if err != nil {
			panic(err)
		}
		it.Status.Issue.CreatedAt = &metav1.Time{Time: ct}
	}
	team := s.team
	if team == "" {
		team = bumblebee
	}
	it.Status.Board = &v1alpha1.ItemBoard{Status: s.status, Kind: s.kind, Team: team}
	if s.prio != 0 {
		it.Status.Board.Priority = intp(s.prio)
	}
	it.Status.Assignees = s.assignees
	it.Status.Parent = s.parent
	for _, b := range s.blockedBy {
		st := v1alpha1.IssueOpen
		if bi := w.issue(b); bi != nil && bi.state != "" {
			st = bi.state
		}
		it.Status.BlockedBy = append(it.Status.BlockedBy, v1alpha1.Blocker{Ref: b, State: st})
	}
	for _, c := range w.issues {
		if c.parent != s.ref {
			continue
		}
		st := c.state
		if st == "" {
			st = v1alpha1.IssueOpen
		}
		it.Status.SubIssues = append(it.Status.SubIssues, v1alpha1.SubIssue{Ref: c.ref, State: st, Status: c.status})
	}
	for _, c := range w.changes[s.ref] {
		it.Status.Changes = append(it.Status.Changes, ChangeRef(c))
	}
	return it
}

// decided runs decide.Item over every tracked fixture issue (twice, so that a sub-item's phase reaches
// its epic) and returns the Items as written, by ref, with their decisions.
func (w *world) decided(t *testing.T, now time.Time, prev map[string]*v1alpha1.Item) (map[string]*v1alpha1.Item, map[string]ItemDecision) {
	t.Helper()
	items := map[string]*v1alpha1.Item{}
	for _, s := range w.issues {
		if s.tracked() || prev[s.ref] != nil {
			items[s.ref] = w.item(s)
		}
	}
	decisions := map[string]ItemDecision{}
	for range 2 {
		for _, s := range w.issues {
			it, ok := items[s.ref]
			if !ok {
				continue
			}
			fresh := w.item(s)
			// Sub-issues that are Items carry their decided phase.
			for i, sub := range fresh.Status.SubIssues {
				if si, ok := items[sub.Ref]; ok {
					fresh.Status.SubIssues[i].Phase = si.Status.Phase
				}
			}
			if p, ok := prev[s.ref]; ok {
				carryDecided(&fresh.Status, &p.Status)
			} else if it.Status.Phase != "" {
				carryDecided(&fresh.Status, &it.Status)
			}
			v := ItemView{
				Now: now, Spec: w.spec, Participants: w.participants(), Item: fresh,
				Changes: w.changes[s.ref], Trigger: TriggerStartup, Source: v1alpha1.SourceStartup,
			}
			if fresh.Status.Board.Team == w.team.Spec.BoardOption {
				v.Team = w.team
			}
			if p, ok := items[s.parent]; ok {
				v.Parent = p
			}
			d := Item(v)
			out := fresh.DeepCopy()
			out.Status = d.Status
			items[s.ref] = out
			// Keep the rows of both rounds: the second one decides on what the first wrote.
			d.Events = append(decisions[s.ref].Events, d.Events...)
			decisions[s.ref] = d
		}
	}
	return items, decisions
}

// carryDecided copies the decided fields of a previous status onto freshly observed facts.
func carryDecided(dst, src *v1alpha1.ItemStatus) {
	dst.Class, dst.Phase, dst.Owner = src.Class, src.Phase, src.Owner
	dst.Eligible, dst.Wait, dst.Shadow, dst.LeftAt = src.Eligible, src.Wait, src.Shadow, src.LeftAt
	dst.TargetRepos, dst.AcceptanceCriteria, dst.Footprint, dst.SubItems = src.TargetRepos, src.AcceptanceCriteria, src.Footprint, src.SubItems
}

// pass decides every Item, then the team, and writes the shadows back.
type passResult struct {
	items     map[string]*v1alpha1.Item
	decisions map[string]ItemDecision
	team      TeamDecision
}

func (w *world) pass(t *testing.T, now time.Time, prev map[string]*v1alpha1.Item, gateEverything bool) passResult {
	t.Helper()
	items, decisions := w.decided(t, now, prev)
	var list []*v1alpha1.Item
	for _, s := range w.issues {
		if it, ok := items[s.ref]; ok {
			list = append(list, it)
		}
	}
	td := Team(TeamView{Now: now, Spec: w.spec, Team: w.team, People: w.people, Items: list, GateEverything: gateEverything, Source: v1alpha1.SourceStartup})
	for ref, sh := range td.Shadows {
		items[ref].Status.Shadow = sh
	}
	return passResult{items: items, decisions: decisions, team: td}
}

func eventsOf(evs []history.Event, typ string) []history.Event {
	var out []history.Event
	for _, e := range evs {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func eventsFor(evs []history.Event, item, typ string) []history.Event {
	var out []history.Event
	for _, e := range evs {
		if e.Type == typ && e.Item == item {
			out = append(out, e)
		}
	}
	return out
}

func questionsOf(qs []decision.Question, point, subject string) []decision.Question {
	var out []decision.Question
	for _, q := range qs {
		if q.Point == point && (subject == "" || q.Subject == subject) {
			out = append(out, q)
		}
	}
	return out
}

func teamPicks(td TeamDecision) []string {
	var out []string
	for _, p := range td.Team.Picks {
		out = append(out, p.Person+":"+p.Item)
	}
	return out
}

func teamHeld(td TeamDecision) []string {
	var out []string
	for _, h := range td.Team.Held {
		out = append(out, h.Person+":"+h.Item+":"+h.Gate)
	}
	return out
}

func actionTypes(sh *v1alpha1.Shadow) []string {
	if sh == nil {
		return nil
	}
	var out []string
	for _, a := range sh.Actions {
		out = append(out, a.Type)
	}
	return out
}

func actionOf(sh *v1alpha1.Shadow, typ string) *v1alpha1.ShadowAction {
	if sh == nil {
		return nil
	}
	for i := range sh.Actions {
		if sh.Actions[i].Type == typ {
			return &sh.Actions[i]
		}
	}
	return nil
}

func joined(s []string) string { return strings.Join(s, ",") }

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %#v, want %#v", what, got, want)
	}
}
