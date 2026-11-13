package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/post"
	"github.com/giantswarm/beekeeper/internal/state"
)

// applyReport changes only the supplied state. Admission uses a snapshot;
// consumption repeats validation against the current state under the writer lock.
func (a *app) applyReport(st *state.State, p *sessionReport) ([]state.Event, error) {
	switch p.Kind {
	case reportKindPark:
		return a.applyParkReport(st, p)
	case reportKindIdle:
		return a.applyIdleReport(st, p)
	case reportKindProblem:
		i := a.callerAgent(st, p.By)
		if i < 0 {
			return nil, refused("session is no longer registered")
		}
		return a.recordWorkerProblem(st, &st.Agents[i], p.By, p.Finding)
	case paramNote:
		return a.applyNoteReport(st, p)
	case reportKindServe:
		return a.applyServeReport(st, p)
	}
	return nil, refused("unknown report operation %q", p.Kind)
}

func (a *app) applyParkReport(st *state.State, p *sessionReport) ([]state.Event, error) {
	me, on, waits, external := p.By, p.On, p.Waits, p.External
	postMerge := p.PostMerge
	id, isNote := parkNote(on)
	kind, reviewHead := p.RefKind, p.ReviewHead

	i := a.callerAgent(st, me)
	if i >= 0 && (st.Agents[i].Task == "" || st.Agents[i].Done) {
		return nil, refused("%q has no task: park keeps a task, an idle agent has nothing to wait for", st.Agents[i].Name)
	}
	if i < 0 && wakeID(me) == "" {
		return nil, refused("%s has no session id to wake once the wait settles: park inside a session, or beekeeper agents register first", me.Name)
	}
	if isNote {
		if !slices.ContainsFunc(st.Notes, func(n state.Note) bool { return n.ID == id }) {
			return nil, refused("note #%d is not open: its answer is in beekeeper log --verb %s", id, answerRecorded)
		}
		on = fmt.Sprintf("#%d", id)
	}
	if target, ok := parkHold(on); ok && !holdStands(st, target, a.now) {
		return nil, refused("nothing holds %s: run the refused command again, there is nothing to park on", target)
	}
	park := &state.Park{External: external, By: me, At: a.now.UTC(), On: on, Waits: waits, RefKind: kind, ReviewHead: reviewHead}
	if i >= 0 && (reviewHead != "" || isDeskReviewWait(waits)) {
		var declaration *string
		if len(postMerge) > 0 {
			declaration = &postMerge[0]
		}
		park.PostMerge = reviewPostMerge(st, on, st.Agents[i].Park, declaration)
		park.Worktree = p.Worktree
	}
	name := me.Name
	// One park per session: a new one replaces the last, also one it
	// took off the roster before it registered.
	st.Parks = slices.DeleteFunc(st.Parks, func(p state.Park) bool { return p.By.Is(me) })
	// self is the session that parks: under --as the agent's own,
	// whose session record and start the park concerns.
	self := me
	if i >= 0 {
		self = st.Agents[i].Party
		if on != "" {
			if rp, ok := st.ReviewParkFor(on); ok && (reviewHead != "" || strings.EqualFold(rp.Agent, st.Agents[i].Name) || rp.Party.Is(self)) {
				inheritReviewParkOnAgent(st, &st.Agents[i], rp, a.now.UTC())
			}
		}
		st.Agents[i].Park = park
		recordAgentReviewPark(st, st.Agents[i])
		name = st.Agents[i].Name
	} else {
		// A session off the roster parks too: the watch wakes it by
		// its session id once the wait settles, as it does an agent.
		st.Parks = append(st.Parks, *park)
	}
	for j := range st.Records {
		if st.Records[j].Session.Is(self) {
			st.Records[j].Waits = waits
		}
	}
	var outcomes []state.Event
	if i >= 0 {
		outcomes = a.recordTaskOutcome(st, st.Agents[i], state.OutcomeParked, park.On+": "+park.Waits)
	}
	return append(outcomes, event(me, parkEvent, "%s: %s", name, parkedText(park, a.now))), nil
}

func (a *app) applyIdleReport(st *state.State, p *sessionReport) ([]state.Event, error) {
	me, report, found, finished := p.By, p.Text, slices.Clone(p.Problems), p.Finished

	i := a.callerAgent(st, me)
	if i < 0 {
		return nil, refused("this session is not registered: `beekeeper agents register` first")
	}
	ag := &st.Agents[i]
	if m := post.Unlinked(strings.Join(append([]string{report}, found...), "\n"), agentHere(st, ag)); len(m) > 0 {
		return nil, refused("report refused, it lacks: %s", strings.Join(m, "; "))
	}
	if slices.ContainsFunc(st.DeskReviews, func(round state.DeskReview) bool { return sameReviewSession(round.Reviewer, ag.Party) }) && !sameReviewSession(me, ag.Party) {
		return nil, refused("independent review completion requires its assigned session, not --as")
	}
	lookup := func(_ context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
		if p.preview {
			reads := make(map[github.PR]github.RefState, len(refs))
			for _, r := range refs {
				reads[r] = github.RefState{Kind: github.RefPullRequest, State: github.Merged}
			}
			return reads, nil
		}
		return p.pullStates, nil
	}
	outcomes, err := a.completionOutcomeWith(a.context(), st, *ag, finished, p.NoChange, false, "", lookup)
	if err != nil {
		return nil, err
	}
	found = freshWorkerProblems(ag, found)
	issue := taskIssue(st, *ag)
	reportIdle(ag, a.now)
	endNotes(st, ag.Party, a.now)
	ag.Done = finished
	verb := litAgentsIdle
	if finished {
		verb = executionDone
	}
	evs := append(outcomes, event(me, verb, "%s done: %s", ag.Name, ag.LastTask))
	if finished {
		for _, r := range clearRecords(st, ag.Party) {
			evs = append(evs, event(me, "session.unserve", "%s: %s", r.Session.Name, recordText(r)))
		}
		evs = append(evs, retireTaskParks(st, me, *ag, issue, a.now)...)
	}
	if report != "" || len(found) > 0 {
		st.WorkerReports = append(st.WorkerReports, state.WorkerReport{Finished: finished, By: me, At: a.now.UTC(), Task: ag.LastTask, Text: report, Problems: found, TellTo: reviewStarter(st, *ag, report)})
	}
	if report != "" {
		evs = append(evs, event(me, "agents.report", "%s: %s", ag.Name, report))
	}
	for _, p := range found {
		evs = append(evs, event(me, "agents.problem", "%s: %s", ag.Name, p))
	}
	return evs, nil
}

func (a *app) applyServeReport(st *state.State, p *sessionReport) ([]state.Event, error) {
	me, r := p.By, p.Record
	if p.Clear {
		var events []state.Event
		for _, old := range clearRecords(st, me) {
			events = append(events, event(me, "session.unserve", "%s: %s", old.Session.Name, recordText(old)))
		}
		return events, nil
	}

	r.Session = servedBy(st, p.Session)
	if rosterRole(st, me) == workerRole && !r.Session.Is(me) {
		return nil, refused("%q is another session: a worker records only its own, `beekeeper sessions serve <its own name> %s`", r.Session.Name, r.Issue)
	}
	st.Records = slices.DeleteFunc(st.Records, func(o state.Record) bool { return o.Session.Is(r.Session) })
	st.Records = append(st.Records, r)
	return []state.Event{about(event(me, verbServe, "%s: %s", r.Session.Name, recordText(r)), r.Issue)}, nil
}

func (a *app) applyNoteReport(st *state.State, p *sessionReport) ([]state.Event, error) {
	me, n, foldInto, checked, stage, forWho := p.By, p.Note, p.FoldInto, p.Checked, p.Stage, p.Note.For
	fold := func(o *state.Note, why string) []state.Event {
		o.Text += fmt.Sprintf(" | Also from %s: %s", me.Name, n.Text)
		if o.Held == nil {
			o.Held, o.AlsoHeld = n.Held, n.AlsoHeld
		}
		p.NoteID = o.ID
		return []state.Event{event(me, "note.folded", "into #%d (%s): %s", o.ID, why, n.Text)}
	}

	if foldInto != 0 {
		i := slices.IndexFunc(st.Notes, func(o state.Note) bool { return o.ID == foldInto })
		if i < 0 {
			return nil, refused("note #%d is not open: nothing to fold into", foldInto)
		}
		if o := &st.Notes[i]; !strings.EqualFold(o.For, n.For) {
			return nil, refused("note #%d is for %q, this one for %q: a note folds into one for the same person", o.ID, o.For, n.For)
		}
		return fold(&st.Notes[i], "--fold-into"), nil
	} else if checked {
		if o, ref := foldTarget(st.Notes, &n); o != nil {
			if c := foldConflict(o, &n); len(c) > 0 {
				return nil, refused("note #%d for %s asks the same question on %s, but %s: fold into it with --fold-into %d (its default and recommendation stand) or ask a different question",
					o.ID, o.For, ref, strings.Join(c, " and "), o.ID)
			}
			return fold(o, ref+", the same question"), nil
		}
	}
	st.NextNote++
	n.ID = st.NextNote
	p.NoteID = n.ID
	st.Notes = append(st.Notes, n)
	return append([]state.Event{event(me, noteAddedEvent, "#%d %s", n.ID, n.Text)}, stage.events(me, n.ID, forWho)...), nil
}
