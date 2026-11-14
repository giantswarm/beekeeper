package decide

import (
	"fmt"
	"sort"
	"time"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/decision"
	"github.com/giantswarm/beekeeper/internal/history"
)

// detailGateEverything is the held detail of the gate_everything test fault.
const detailGateEverything = "gate-everything"

// candidate is an eligible team item in the team's order.
type candidate struct {
	it         *v1alpha1.Item
	ref        string
	rank       int
	unblocks   bool
	activeEpic bool
	// epic is the ref of the item's parent epic, when it has one.
	epic string
}

type skip struct {
	seq          int
	person       string
	ref          string
	gate, detail string
	rank         int
	firstTurn    bool
}

type pick struct{ person, ref string }

// Team decides a team's order (§1.4 next?), the picks per person, the items gates hold, every team
// Item's shadow with its would-actions, and the next? and parallel-safe? questions.
func Team(v TeamView) TeamDecision {
	d := TeamDecision{People: map[string]v1alpha1.PersonStatus{}, Shadows: map[string]*v1alpha1.Shadow{}}
	if v.Team == nil {
		return d
	}
	spec := v.Spec
	if spec == nil {
		spec = &v1alpha1.InstallationSpec{}
	}
	team := v.Team.Name
	src := v.Source
	if src == "" {
		src = v1alpha1.SourceEngine
	}

	people := append([]*v1alpha1.Person(nil), v.People...)
	sort.SliceStable(people, func(i, j int) bool { return login(people[i]) < login(people[j]) })
	var logins []string
	byLogin := map[string]*v1alpha1.Person{}
	for _, p := range people {
		logins = append(logins, login(p))
		byLogin[login(p)] = p
	}

	byRef := map[string]*v1alpha1.Item{}
	for _, it := range v.Items {
		if r := ItemRef(it); r != "" {
			byRef[r] = it
		}
	}
	// Work that unblocks in-flight work: open blockers of any tracked implementable item in flight.
	unblocking := map[string]bool{}
	for _, it := range v.Items {
		if it.Status.Class != v1alpha1.ClassImplementable || !inFlight(it.Status.Phase) {
			continue
		}
		for _, b := range it.Status.BlockedBy {
			if b.State == v1alpha1.IssueOpen {
				unblocking[b.Ref] = true
			}
		}
	}

	// The team's items, its in-flight count and its order of eligible items.
	var items []*v1alpha1.Item
	inFlightCount := 0
	var order []*candidate
	for _, it := range v.Items {
		if !inTeam(it, v.Team) {
			continue
		}
		items = append(items, it)
		if it.Status.Class == v1alpha1.ClassImplementable && inFlight(it.Status.Phase) {
			inFlightCount++
		}
		if it.Status.Eligible == nil || !it.Status.Eligible.Value {
			continue
		}
		c := &candidate{it: it, ref: ItemRef(it), unblocks: unblocking[ItemRef(it)]}
		if p := byRef[it.Status.Parent]; p != nil && p.Status.Class == v1alpha1.ClassEpic {
			c.epic = it.Status.Parent
			c.activeEpic = p.Status.Phase == v1alpha1.PhaseActive
		}
		order = append(order, c)
	}
	sort.SliceStable(order, func(i, j int) bool { return orderLess(order[i], order[j]) })
	rank := map[string]int{}
	for i, c := range order {
		c.rank = i + 1
		rank[c.ref] = c.rank
	}

	// Picking: participants take turns in login order, each taking the next startable item that is
	// theirs or nobody's, as if the earlier picks of the pass had started.
	free := map[string]int{}
	epicLimit := map[string]int{}
	for _, l := range logins {
		p := byLogin[l]
		free[l] = max(p.Spec.WIP.Items, 0)
		epicLimit[l] = max(p.Spec.WIP.Epics, 0)
	}
	var capItems, capEpics *int
	if v.Team.Spec.Cap != nil {
		capItems, capEpics = v.Team.Spec.Cap.Items, v.Team.Spec.Cap.Epics
	}
	pickedBy := map[string]string{}
	picksOf := map[string][]string{}
	var picks []pick
	epicHeld := map[string]bool{}
	epicsOf := map[string]int{}
	repoHolders := map[string][]*candidate{}
	asked := map[[2]string]string{}
	var skips []skip
	seq := 0
	nextCands := map[string][]decision.Candidate{}
	turns := map[string]int{}

	slotOwner := func(c *candidate, person string) string {
		if e := byRef[c.epic]; e != nil {
			if _, ok := byLogin[e.Status.Owner]; ok {
				return e.Status.Owner
			}
		}
		return person
	}
	parallelSafe := func(c, holder *candidate, repo string) string {
		key := [2]string{c.ref, holder.ref}
		if a, ok := asked[key]; ok {
			return a
		}
		q := decision.ParallelSafeQuestion(spec, team, c.ref, holder.ref, repo, c.it.Status.Footprint, holder.it.Status.Footprint)
		d.Questions = append(d.Questions, q)
		asked[key] = q.Rule
		return q.Rule
	}
	gate := func(c *candidate, person string) (string, string) {
		if v.GateEverything {
			return v1alpha1.GateTeamCap, detailGateEverything
		}
		if c.unblocks {
			return "", "" // work that unblocks in-flight work is exempt from all gates
		}
		if capItems != nil && len(picks) >= *capItems {
			return v1alpha1.GateTeamCap, fmt.Sprintf("cap.items %d", *capItems)
		}
		if c.epic != "" && !epicHeld[c.epic] {
			owner := slotOwner(c, person)
			if epicsOf[owner] >= epicLimit[owner] {
				return v1alpha1.GateEpicWIP, c.epic
			}
			if capEpics != nil && len(epicHeld) >= *capEpics {
				return v1alpha1.GateTeamCap, fmt.Sprintf("cap.epics %d", *capEpics)
			}
		}
		for _, repo := range c.it.Status.TargetRepos {
			for _, h := range repoHolders[repo] {
				if parallelSafe(c, h, repo) != decision.AnswerYes {
					return v1alpha1.GateRepoConcurrency, repo + " taken by " + h.ref
				}
			}
		}
		return "", ""
	}
	take := func(c *candidate, person string) {
		pickedBy[c.ref] = person
		picksOf[person] = append(picksOf[person], c.ref)
		picks = append(picks, pick{person: person, ref: c.ref})
		if c.epic != "" && !epicHeld[c.epic] {
			epicHeld[c.epic] = true
			epicsOf[slotOwner(c, person)]++
		}
		for _, repo := range c.it.Status.TargetRepos {
			repoHolders[repo] = append(repoHolders[repo], c)
		}
	}

	for {
		progress := false
		for _, l := range logins {
			if len(picksOf[l]) >= free[l] {
				continue
			}
			first := turns[l] == 0
			turns[l]++
			needCands := len(picksOf[l]) == 0
			var cands []decision.Candidate
			picked := ""
			for _, c := range order {
				if pickedBy[c.ref] != "" || !forPerson(c.it, l) {
					continue
				}
				if picked != "" {
					if !needCands {
						break
					}
					cands = append(cands, nextCandidate(c, ""))
					continue
				}
				if g, det := gate(c, l); g != "" {
					seq++
					skips = append(skips, skip{seq: seq, person: l, ref: c.ref, gate: g, detail: det, rank: c.rank, firstTurn: first})
					if needCands {
						cands = append(cands, nextCandidate(c, g))
					}
					continue
				}
				take(c, l)
				picked = c.ref
				if needCands {
					cands = append(cands, nextCandidate(c, ""))
				}
			}
			if picked != "" {
				progress = true
				if needCands {
					nextCands[l] = cands
				}
			}
		}
		if !progress {
			break
		}
	}

	// Held: the items skipped above a person's last pick, or the top candidate of a person who gets no
	// pick; each item for the first person in turn order, never an item someone picked.
	var heldList []skip
	for _, l := range logins {
		lastRank := 0
		if ps := picksOf[l]; len(ps) > 0 {
			lastRank = rank[ps[len(ps)-1]]
		}
		seen := map[string]bool{}
		for _, s := range skips {
			if s.person != l || seen[s.ref] {
				continue
			}
			if lastRank > 0 && s.rank < lastRank {
				seen[s.ref] = true
				heldList = append(heldList, s)
			}
			if lastRank == 0 && s.firstTurn {
				heldList = append(heldList, s)
				break
			}
		}
	}
	sort.SliceStable(heldList, func(i, j int) bool { return heldList[i].seq < heldList[j].seq })
	heldBy := map[string]skip{}
	var held []skip
	for _, s := range heldList {
		if _, dup := heldBy[s.ref]; dup || pickedBy[s.ref] != "" {
			continue
		}
		heldBy[s.ref] = s
		held = append(held, s)
	}

	// Team and Person status.
	d.Team = v1alpha1.TeamStatus{InFlight: inFlightCount, LastDecisionAt: mtp(v.Now)}
	for _, p := range picks {
		d.Team.Picks = append(d.Team.Picks, v1alpha1.TeamPick{Person: p.person, Item: p.ref})
	}
	for _, s := range held {
		d.Team.Held = append(d.Team.Held, v1alpha1.TeamHeld{Person: s.person, Item: s.ref, Gate: s.gate})
	}
	for _, l := range logins {
		p := byLogin[l]
		ps := v1alpha1.PersonStatus{
			Free:  v1alpha1.WIP{Items: max(p.Spec.WIP.Items, 0), Epics: max(p.Spec.WIP.Epics, 0)},
			Picks: picksOf[l],
		}
		for _, s := range held {
			if s.person == l {
				ps.Held = append(ps.Held, v1alpha1.PersonHeld{Item: s.ref, Gate: s.gate})
			}
		}
		d.People[l] = ps
	}

	// next?: one decision per person and pass, answered by the person's first pick.
	for _, l := range logins {
		if len(picksOf[l]) == 0 {
			continue
		}
		d.Questions = append(d.Questions, decision.NextQuestion(spec, team, l, nextCands[l]))
	}

	// Every team Item's shadow, and rows for what is new on it.
	optIn := optedIn(v.Team)
	sort.SliceStable(items, func(i, j int) bool {
		ri, rj := rank[ItemRef(items[i])], rank[ItemRef(items[j])]
		if (ri > 0) != (rj > 0) {
			return ri > 0
		}
		if ri != rj {
			return ri < rj
		}
		return refLess(ItemRef(items[i]), ItemRef(items[j]))
	})
	for _, it := range items {
		ref := ItemRef(it)
		sh := &v1alpha1.Shadow{Rank: rank[ref]}
		if p := pickedBy[ref]; p != "" {
			sh.Pick = &v1alpha1.ShadowPick{Person: p, Rank: rank[ref]}
			if len(it.Status.Assignees) == 0 {
				sh.Actions = append(sh.Actions, action(ref, v1alpha1.ActionAssign, p, map[string]string{"login": p}))
			}
			sh.Actions = append(sh.Actions,
				action(ref, v1alpha1.ActionSetStatus, p, map[string]string{"status": StatusInProgress}),
				action(ref, v1alpha1.ActionStartRun, p, map[string]string{"purpose": PurposeImplement}))
		}
		if s, ok := heldBy[ref]; ok {
			sh.HeldBy = &v1alpha1.ShadowHeld{Person: s.person, Gate: s.gate, Detail: s.detail}
		}
		readyAnswer, severity := itemQuestions(spec, it, optIn, logins)
		if readyAnswer != "" {
			sh.Ready = readyAnswer
			if readyAnswer == decision.AnswerNotReady {
				to := it.Status.Owner
				if to == "" {
					to = "team:" + team
				}
				text := spec.Ready.Todo
				sh.Actions = append(sh.Actions, action(ref, v1alpha1.ActionTodo, to, map[string]string{"to": to, "text": text}))
			}
		}
		if severity != "" {
			sh.Severity = severity
			sh.Actions = append(sh.Actions, action(ref, v1alpha1.ActionSetSeverity, severity, map[string]string{"severity": severity}))
		}
		if sh.Pick == nil && sh.HeldBy == nil && len(sh.Actions) == 0 && sh.Rank == 0 && sh.Ready == "" && sh.Severity == "" {
			sh = nil
		}
		d.Shadows[ref] = sh
		d.Events = append(d.Events, shadowEvents(v.Now, team, ref, src, it.Status.Shadow, sh)...)
	}
	return d
}

func login(p *v1alpha1.Person) string {
	if p.Spec.Login != "" {
		return p.Spec.Login
	}
	return p.Name
}

// inTeam reports whether an Item is on the team's board option.
func inTeam(it *v1alpha1.Item, team *v1alpha1.Team) bool {
	if ItemRef(it) == "" || (it.Status.InScope != nil && !*it.Status.InScope) {
		return false
	}
	if it.Status.Board != nil {
		return it.Status.Board.Team == team.Spec.BoardOption
	}
	return it.Labels[v1alpha1.LabelTeam] == team.Name
}

// forPerson: an item is for its owner, or for anyone when it is unassigned and has no owner (an
// unassigned sub-item belongs to its epic's owner).
func forPerson(it *v1alpha1.Item, person string) bool {
	if it.Status.Owner != "" {
		return it.Status.Owner == person
	}
	return len(it.Status.Assignees) == 0
}

// orderLess is the order of §1.4 next?: work that unblocks in-flight work, items in Active epics,
// Priority (lower first, unset last), age (older first). Incidents and main repairs, which would come
// first, are never eligible in phase 1.
func orderLess(a, b *candidate) bool {
	if a.unblocks != b.unblocks {
		return a.unblocks
	}
	if a.activeEpic != b.activeEpic {
		return a.activeEpic
	}
	pa, pb := priority(a.it), priority(b.it)
	switch {
	case pa != nil && pb == nil:
		return true
	case pa == nil && pb != nil:
		return false
	case pa != nil && pb != nil && *pa != *pb:
		return *pa < *pb
	}
	ca, cb := createdAt(a.it), createdAt(b.it)
	switch {
	case !ca.IsZero() && cb.IsZero():
		return true
	case ca.IsZero() && !cb.IsZero():
		return false
	case !ca.Equal(cb):
		return ca.Before(cb)
	}
	return refLess(a.ref, b.ref)
}

func priority(it *v1alpha1.Item) *int {
	if it.Status.Board == nil {
		return nil
	}
	return it.Status.Board.Priority
}

func createdAt(it *v1alpha1.Item) time.Time {
	if it.Status.Issue == nil || it.Status.Issue.CreatedAt == nil {
		return time.Time{}
	}
	return it.Status.Issue.CreatedAt.Time
}

func nextCandidate(c *candidate, held string) decision.Candidate {
	nc := decision.Candidate{Ref: c.ref, Unblocks: c.unblocks, ActiveEpic: c.activeEpic, Held: held}
	if c.it.Status.Issue != nil {
		nc.Title = c.it.Status.Issue.Title
	}
	if p := priority(c.it); p != nil {
		pv := *p
		nc.Priority = &pv
	}
	if t := createdAt(c.it); !t.IsZero() {
		nc.CreatedAt = t.UTC().Format(time.RFC3339)
	}
	return nc
}

// itemQuestions returns the ready? and bug-severity? answers the shadow carries, when they are asked
// (the same code checks and rules as decide.Item's questions).
func itemQuestions(spec *v1alpha1.InstallationSpec, it *v1alpha1.Item, optIn bool, participants []string) (ready, severity string) {
	st := &it.Status
	if st.Issue == nil {
		return "", ""
	}
	kind, status := boardKind(st), boardStatus(st)
	open := st.Issue.State == v1alpha1.IssueOpen || st.Issue.State == ""
	scoped := st.InScope == nil || *st.InScope
	elsewhere := len(st.Assignees) > 0 && firstParticipant(st.Assignees, participants) == ""
	if askReady(scoped, open, st.Phase, st.Class, optIn, elsewhere) {
		ready = decision.ReadyRule(spec.Ready, decision.ReadyFacts{
			Title: st.Issue.Title, Body: st.Issue.Body, Repo: st.Issue.Repo, Bug: kind == kindBug,
		})
	}
	if askSeverity(scoped, open, kind, status, optIn, elsewhere) {
		severity = decision.BugSeverityRule()
	}
	return ready, severity
}

// action is a would-action with a key that is stable for as long as the action means the same thing.
func action(ref, typ, who string, args map[string]string) v1alpha1.ShadowAction {
	return v1alpha1.ShadowAction{Key: ref + "/" + typ + "/" + who, Type: typ, Args: args}
}

// shadowEvents records what is new on a shadow: a pick for a new person, a new hold, actions whose key
// wasn't there before.
func shadowEvents(now time.Time, team, ref, src string, prev, cur *v1alpha1.Shadow) []history.Event {
	if cur == nil {
		return nil
	}
	var evs []history.Event
	add := func(typ string, data map[string]any) {
		e := event(now, typ, src, data)
		e.Item, e.Team = ref, team
		evs = append(evs, e)
	}
	if cur.Pick != nil && (prev == nil || prev.Pick == nil || prev.Pick.Person != cur.Pick.Person) {
		add(EventShadowPick, map[string]any{"person": cur.Pick.Person, "rank": cur.Pick.Rank})
	}
	if cur.HeldBy != nil && (prev == nil || prev.HeldBy == nil ||
		prev.HeldBy.Person != cur.HeldBy.Person || prev.HeldBy.Gate != cur.HeldBy.Gate) {
		add(EventShadowHeld, map[string]any{"person": cur.HeldBy.Person, "gate": cur.HeldBy.Gate})
	}
	had := map[string]bool{}
	if prev != nil {
		for _, a := range prev.Actions {
			had[a.Key] = true
		}
	}
	for _, a := range cur.Actions {
		if had[a.Key] {
			continue
		}
		args := map[string]any{}
		for k, v := range a.Args {
			args[k] = v
		}
		add(EventShadowAction, map[string]any{"key": a.Key, "type": a.Type, "args": args})
	}
	return evs
}
