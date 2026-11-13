package cmd

import (
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// parkRetiredEvent ends the park of a worker whose task another worker
// reported done: nobody wakes it and no fresh session continues it.
const parkRetiredEvent = "agents.park-retired"

// taskIssue is the issue ag's task serves, owner/repo#n: the one its open
// serve record names, else the first issue its task names by URL; "" when
// it names none. A pull request is no task issue: a reviewer serves the
// pull request it reviews, not the task of its author.
func taskIssue(st *state.State, ag state.Agent) string {
	issue := func(raw string) string {
		if strings.Contains(strings.ToLower(raw), "/pull/") {
			return ""
		}
		if r, ok := parseRef(raw); ok {
			return refName(r)
		}
		return ""
	}
	for _, r := range st.Records {
		if r.Session.Is(ag.Party) && r.Ended.IsZero() {
			if ref := issue(r.Issue); ref != "" {
				return ref
			}
		}
	}
	for _, task := range []string{ag.Task, ag.LastTask} {
		for _, m := range refURL.FindAllString(task, -1) {
			if ref := issue(m); ref != "" {
				return ref
			}
		}
	}
	return ""
}

// wakesAgent reports whether timer t wakes ag: by its name, its session id
// or its desktop session id.
func wakesAgent(t state.Timer, ag state.Agent) bool {
	return t.Wake != "" && (strings.EqualFold(t.Wake, ag.Name) ||
		ag.Session != "" && t.Wake == ag.Session || ag.HostSession != "" && t.Wake == ag.HostSession)
}

// retireTaskParks retires the parks of the other workers on the task issue
// done just reported done, by: the second session a hand start, a relay or a
// continuation left parked on the same task. Woken, by the issue's close or
// a timer, it would start a fresh session that only finds the work done. Its
// task ends as done's did, without a report of its own; its serve records
// and the timers that wake it go, and the doctor takes it off the roster.
// A park whose continuation is starting already, a desk review or CI park
// (the loop owns that pull request's flow) and a role holder's park stay.
func retireTaskParks(st *state.State, by state.Party, done state.Agent, issue string, now time.Time) []state.Event {
	if issue == "" || slices.ContainsFunc(st.DeskReviews, func(r state.DeskReview) bool { return sameReviewSession(r.Reviewer, done.Party) }) {
		return nil
	}
	var evs []state.Event
	for i := range st.Agents {
		ag := &st.Agents[i]
		p := ag.Park
		if ag.Is(done.Party) || p == nil || p.Continuing || p.Checks || p.ReviewHead != "" || isDeskReviewWait(p.Waits) ||
			ag.Task == "" || ag.Done || roleRun(st, ag.Party) || !strings.EqualFold(taskIssue(st, *ag), issue) {
			continue
		}
		parked := parkedText(p, now)
		reportIdle(ag, now)
		ag.Done = true
		endNotes(st, ag.Party, now)
		for _, r := range clearRecords(st, ag.Party) {
			evs = append(evs, event(by, "session.unserve", "%s: %s", r.Session.Name, recordText(r)))
		}
		retired := *ag
		st.Timers = slices.DeleteFunc(st.Timers, func(t state.Timer) bool {
			if !wakesAgent(t, retired) {
				return false
			}
			evs = append(evs, event(by, "timer.done", "#%d %s", t.ID, t.What))
			return true
		})
		evs = append(evs, event(by, parkRetiredEvent, "%s: %s; %s reported %s done, so no session continues it",
			ag.Name, parked, done.Name, issue))
	}
	return evs
}
