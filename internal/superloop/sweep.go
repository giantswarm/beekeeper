package superloop

import (
	"cmp"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// OrphanWait is how long an open pull request of the desk may go without a
// live owner before the sweep gives it one.
const OrphanWait = 15 * time.Minute

// MaxSweeps is how many owners the sweep gives one head of a pull request:
// a head that loses its owner again after that goes to the guide.
const MaxSweeps = 2

// sweepTag marks a key the sweep wrote: "<what> <ref>@<head> sweep <since>".
const sweepTag = " sweep "

// sweepKey is the key of the sweep's action what (review, go, fix) for
// ref's head, for the stretch without a live owner that began at since:
// one action for each such stretch.
func sweepKey(what, ref, head string, since time.Time) string {
	return fmt.Sprintf("%s %s@%s%s%s", what, ref, head, sweepTag, since.UTC().Format(time.RFC3339Nano))
}

// IsSweep reports whether key is a key the sweep wrote.
func IsSweep(key string) bool { return strings.Contains(key, sweepTag) }

// KeyHead is the head a key names after its ref ("<what> <ref>@<head>…"),
// "" when it names none.
func KeyHead(key string) string {
	_, rest, ok := strings.Cut(key, "@")
	if !ok {
		return ""
	}
	head, _, _ := strings.Cut(rest, " ")
	return head
}

// sweepLedger is what the taken actions say for the sweep: the owners it
// gave each head (by ref@head), and when the loop last acted on each pull
// request, a leave aside.
type sweepLedger struct {
	sweeps  map[string]int
	lastAct map[string]time.Time
}

// sweptBy reads the sweep's ledger from taken.
func sweptBy(taken []Taken) sweepLedger {
	sw := sweepLedger{sweeps: map[string]int{}, lastAct: map[string]time.Time{}}
	active, _ := episodeTaken(taken)
	for _, t := range active {
		if IsSweep(t.Key) {
			sw.sweeps[t.Ref+"@"+KeyHead(t.Key)]++
		}
		if t.Ref != "" && t.Kind != KindLeave && t.At.After(sw.lastAct[t.Ref]) {
			sw.lastAct[t.Ref] = t.At
		}
	}
	return sw
}

// leftSwept is the reason of the leave that hands a head the sweep gave
// MaxSweeps owners to the guide.
func leftSwept(head string) string { return "swept@" + short(head) }

// Ownerless are the open pull requests among in.Pulls the desk owns and no
// live session owns, sorted: the loop keeps since when each has been so
// (Input.Ownerless). A draft is nobody's to take over: it does not ask for
// a review, and an epic's plan waits for its person.
func Ownerless(in Input) []string {
	l := newLedger(in.Taken, in.Answers)
	absorbed := absorbedByMerged(in)
	var out []string
	for ref, p := range in.Pulls {
		p.Ref = cmp.Or(p.Ref, ref)
		if absorbed[strings.ToLower(ref)] == "" && orphanable(in, l, ref, p) && liveOwner(in, l, ref, p) == "" {
			out = append(out, ref)
		}
	}
	sort.Strings(out)
	return out
}

// orphanable reports whether p is a pull request the sweep may give an
// owner: open, not a draft, the desk's own, still tracked.
func orphanable(in Input, l ledger, ref string, p Pull) bool {
	p.Ref = cmp.Or(p.Ref, ref)
	if p.State != Open || p.Head == "" || p.Draft {
		return false
	}
	if absorbedByMerged(in)[strings.ToLower(ref)] != "" {
		return false
	}
	if reason, _ := in.Desk.unread(ref); reason != "" {
		return false
	}
	o, _, _ := in.Desk.owner(p, l.overridden[ref])
	return o == ours
}

// liveOwner is the name of the agent on the roster that owns ref, "" when
// none does. A saved review park is no live owner: its agent left the
// roster. An agent owns ref when it is parked on it for its own sake (a
// review request, CI), parked on or serving ref or an issue ref closes,
// carries ref's worker name or the one the loop acts for, names it or such
// an issue in its task, or reviews it.
// One parked on ref waiting for another worker's pull request is a waiter,
// whose owner planOverlap finds.
func liveOwner(in Input, l ledger, ref string, p Pull) string {
	names := func(r string) bool {
		return r != "" && (strings.EqualFold(r, ref) || slices.ContainsFunc(p.Closes, func(c string) bool { return strings.EqualFold(c, r) }))
	}
	for _, w := range in.Workers {
		if w.Saved || w.Name == "" {
			continue
		}
		parkedHere := strings.EqualFold(w.ParkedOn, ref)
		switch {
		case parkedHere && (w.ReviewRequested || !waitsOnOtherPull(w, l)),
			!parkedHere && names(w.ParkedOn),
			names(w.Serves),
			slices.ContainsFunc(w.Mentions, names),
			strings.EqualFold(w.Name, PullWorkerName(ref)),
			l.reviewer[ref] != "" && strings.EqualFold(w.Name, l.reviewer[ref]),
			reviews(ref, w.Name, len(l.rounds[ref])):
			return w.Name
		}
	}
	return ""
}

// reviews reports whether name is the reviewer of one of ref's rounds.
func reviews(ref, name string, rounds int) bool {
	for rd := 1; rd <= max(rounds, 1); rd++ {
		if strings.EqualFold(name, ReviewerName(ref, rd)) {
			return true
		}
	}
	return false
}

// planSweep gives ref, a pull request without a live owner since
// in.Ownerless says, and since the loop last acted on it, for OrphanWait,
// the owner its head needs: a fixer for a
// head that conflicts, is red, carries a review's unfixed changes request,
// or is behind a base its merge does not update; the merge go again for a
// green head whose go was given and never carried out; a reviewer for a
// green head awaiting its one review; a worker to take it over and park
// once that review gave its verdict, for the loop's go without a second
// review. A head with checks still running waits. Each
// stretch without an owner gets one action, a head at most MaxSweeps; an
// ownership question still open is the person's. Another desk's worker's
// pull request (in.Elsewhere) gets the first review of its green head
// without the wait.
func planSweep(in Input, l ledger, sw sweepLedger, ref string, p Pull, leave func(string, string, string, string, string), add func(Action)) {
	since, ok := in.Ownerless[ref]
	if !ok || since.IsZero() {
		return
	}
	// Another desk's worker waits on its own desk for this desk's review:
	// its green head gets the review now, whatever it last did on GitHub.
	if in.Elsewhere[ref] == "" || !awaitsFirstReview(l, ref, p) {
		// The loop's own last act on it (a start, a wake, the sweep's last
		// action) gives its owner the wait again: one action for each stretch.
		if last := sw.lastAct[ref]; last.After(since) {
			since = last
		}
		// Activity on GitHub, a push or a review, is somebody's: the wait runs
		// from it.
		if p.Updated.After(since) {
			since = p.Updated
		}
		if in.Now.Before(since.Add(OrphanWait)) {
			return
		}
	}
	if !orphanable(in, l, ref, p) || liveOwner(in, l, ref, p) != "" {
		return
	}
	if e, ok := l.asked[ref]; ok && e.answer == nil {
		return
	}
	agent := cmp.Or(l.reviewer[ref], PullWorkerName(ref))
	if sw.sweeps[ref+"@"+p.Head] >= MaxSweeps {
		leave(ref, p.URL, agent, leftSwept(p.Head), fmt.Sprintf("%s at head %s has no live owner again after the %d the sweep gave it: the guide decides who takes it on", p.URL, short(p.Head), MaxSweeps))
		return
	}
	base := Action{Ref: ref, URL: p.URL, Agent: agent, Head: p.Head}
	idle := fmt.Sprintf("%s has had no live owner since %s", p.URL, since.UTC().Format(time.RFC3339))
	fix := func(why string) {
		a := base
		a.Key, a.Kind = sweepKey("fix", ref, p.Head, since), KindRework
		a.Message = fmt.Sprintf("%s: %s. You own it now: bring head %s to green CI without conflicts, then park from its worktree for the desk review (beekeeper agents park --on %s \"desk review\"). Never force-push.", idle, why, short(p.Head), ref)
		add(a)
	}
	review := func(key string) {
		a := base
		a.Key, a.Kind, a.Round = key, KindReview, len(l.rounds[ref])+1
		add(a)
	}
	rs := l.rounds[ref]
	var v *Review
	if len(rs) > 0 {
		v = p.verdict(rs[len(rs)-1], l.pushed(p))
	}
	reviewedHere := slices.ContainsFunc(rs, func(rd round) bool { return sameHead(rd.head, p.Head) })
	// once is the pull request's one desk review, when a round gave it:
	// no second review starts, whoever pushed past it.
	onceRound, once := p.reviewed(rs, l.pushed(p))
	if once != nil && once.Verdict == Approved {
		base.RepairAgent = ReviewerName(ref, onceRound)
	}
	switch {
	case p.Mergeable == mergeableConflicting || p.MergeState == mergeStateDirty:
		base.Agent = cmp.Or(base.RepairAgent, base.Agent)
		fix(fmt.Sprintf("head %s conflicts with %s: merge %s into the branch and resolve the conflicts", short(p.Head), baseName(p), baseName(p)))
	case len(p.Red) > 0:
		base.Agent = cmp.Or(base.RepairAgent, base.Agent)
		var names []string
		for _, c := range p.Red {
			names = append(names, strings.TrimSpace(c.Name+" "+c.URL))
		}
		fix(fmt.Sprintf("CI is red on head %s (%s): fix it, or rerun a check that failed for no reason of the change", short(p.Head), strings.Join(names, "; ")))
	case v != nil && v.Verdict == ChangesRequested && sameHead(v.Head, p.Head):
		fix(fmt.Sprintf("review round %d asked for changes on head %s and nobody fixed them: %s, word for word:\n\n%s\n\nFix every finding", len(rs), short(p.Head), v.URL, v.Body))
	case len(rs) == 0 && ownerChangesRequested(p):
		fix(fmt.Sprintf("a review asked for changes on head %s and nobody fixed them: answer and fix its findings", short(p.Head)))
	case !p.Green:
		// Its checks run, or have not reported yet: nothing to own yet.
	case l.has(goKey(ref, p.Head)) && (!in.RequireReview || (v != nil && v.Verdict == Approved && in.approvalCovers(p, v.Head))):
		a := goAction(in, base, fmt.Sprintf("the merge go for head %s was given and never carried out; %s", short(p.Head), idle))
		a.Key = sweepKey("go", ref, p.Head, since)
		add(a)
	case v != nil && v.Verdict == Approved && in.approvalCovers(p, v.Head):
		// planPull gives its go.
	case once != nil:
		// Commits past the one review that nobody parked on: a worker
		// takes them over and parks.
		a := base
		a.Key, a.Kind = sweepKey("fix", ref, p.Head, since), KindRework
		afterPark := "no second review runs, the loop merges it"
		if in.RequireReview && !(once.Verdict == Approved && in.approvalCovers(p, once.Head)) {
			afterPark = "the loop reviews the new head and merges it once approved"
		}
		a.Message = fmt.Sprintf("%s: its one desk review, round %d, %s head %s (%s), and head %s carries commits past it that nobody parked on. You own it now: finish them (fix every finding the review left), bring head %s to green CI without conflicts, then park from its worktree (beekeeper agents park --on %s \"desk review\"): %s. Never force-push.", idle, onceRound, once.Verdict, short(once.Head), cmp.Or(once.URL, p.URL), short(p.Head), short(p.Head), ref, afterPark)
		add(a)
	case reviewedHere || l.has(reviewKey(ref, p.Head)):
		// A review of this head started and ended without a verdict.
		review(sweepKey("review", ref, p.Head, since))
	default:
		review(reviewKey(ref, p.Head))
	}
}

// ownerChangesRequested reports whether the latest verdict of p's owning
// team on its head asks for changes.
func ownerChangesRequested(p Pull) bool {
	reviews := ownerReviews(p, time.Time{})
	for i := len(reviews) - 1; i >= 0; i-- {
		if r := reviews[i]; r.Verdict != "" && sameHead(r.Head, p.Head) {
			return r.Verdict == ChangesRequested
		}
	}
	return false
}

// Unread is why the loop leaves ref alone without reading it (its reason
// and its words), "" when it may read it: outside Orgs or in Never.
func (d Desk) Unread(ref string) (reason, why string) { return d.unread(ref) }
