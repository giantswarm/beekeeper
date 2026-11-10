package cmd

import (
	"fmt"
	"slices"
	"time"

	"github.com/giantswarm/beekeeper/internal/superloop"
)

// loopStallAfter is how long the refill may not run, or a start wait with
// room, before the loop raises the alarm; the alarm is raised once
// per episode until the flow moves again.
const loopStallAfter = 15 * time.Minute

const loopStallKey = "stall starts "

// startStall is the refill's flow as the loop measures it: when fill last
// ran, and since when (with which item and cause) it had room and the board
// offered work, yet started nothing.
type startStall struct {
	ran   time.Time
	since time.Time
	ref   string
	why   string
	// filed and ranFiled count the alarms raised for the start wait and
	// for a refill that does not run.
	filed, ranFiled int
	// reviewSince is the oldest review wait the alarm was raised for, and
	// reviewFiled the alarms raised for it.
	reviewSince time.Time
	reviewFiled int
}

const loopReviewStallKey = "stall reviews "

// reviewWait is a review request the loop planned and has not started:
// since when, and the cause its last try read.
type reviewWait struct {
	since    time.Time
	url, why string
}

// reviewsPlanned keeps the review waits of this pass's review actions; a
// request no longer planned (started, merged, its head moved) waits no more.
func (l *supervisorLoop) reviewsPlanned(acts []superloop.Action) {
	now := l.a.timeNow()
	l.mu.Lock()
	defer l.mu.Unlock()
	waits := make(map[string]reviewWait, len(acts))
	for _, act := range acts {
		w, ok := l.reviewWaits[act.Ref]
		if !ok {
			w = reviewWait{since: now, url: act.URL, why: "its start has not run yet"}
		}
		waits[act.Ref] = w
	}
	l.reviewWaits = waits
}

// reviewTried records the outcome of a review action's run: a started
// reviewer ends its wait, anything else keeps it with the cause it read.
func (l *supervisorLoop) reviewTried(act superloop.Action, failures map[string]loopFailure) {
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.reviewWaits[act.Ref]
	if !ok {
		return
	}
	if l.completed[act.Key] {
		delete(l.reviewWaits, act.Ref)
		return
	}
	if f, failed := failures[act.Key]; failed && f.Message != "" {
		w.why = "its start failed: " + oneLine(f.Message)
	} else {
		w.why = "its start waits: no room for a start on any host, held starts, an incident or an unread repository (the SUPERVISOR REVIEW WAITS lines name it)"
	}
	l.reviewWaits[act.Ref] = w
}

// checkReviewStall raises one alarm when a review request has waited
// loopStallAfter without a reviewer: it names the oldest request, its
// cause and how many wait, once per oldest waiting episode.
func (l *supervisorLoop) checkReviewStall() error {
	now := l.a.timeNow()
	l.mu.Lock()
	var oldest reviewWait
	waiting := 0
	for _, w := range l.reviewWaits {
		if now.Sub(w.since) < loopStallAfter {
			continue
		}
		waiting++
		if oldest.since.IsZero() || w.since.Before(oldest.since) || w.since.Equal(oldest.since) && w.url < oldest.url {
			oldest = w
		}
	}
	if waiting == 0 {
		if l.stall.reviewFiled > 0 {
			_, _ = fmt.Fprintf(l.a.out, "SUPERVISOR STALL ENDED: no review request waits %s for a reviewer any more (stalled since %s)\n", loopStallAfter, l.stall.reviewSince.UTC().Format(time.RFC3339))
		}
		l.stall.reviewSince, l.stall.reviewFiled = time.Time{}, 0
		l.mu.Unlock()
		return nil
	}
	if !oldest.since.Equal(l.stall.reviewSince) {
		l.stall.reviewSince, l.stall.reviewFiled = oldest.since, 0
	}
	n := int(now.Sub(oldest.since) / loopStallAfter)
	raised := l.stall.reviewFiled
	l.mu.Unlock()
	if raised > 0 || n == 0 {
		return nil
	}
	text := fmt.Sprintf("Reviews are stalled: %d review request(s) waited %s or longer for a reviewer; the oldest, %s, waits since %s: %s. Own the cause to the component it names; the alarm is delivered once per waiting episode.", waiting, loopStallAfter, oldest.url, oldest.since.UTC().Format(time.RFC3339), oldest.why)
	line := fmt.Sprintf("%s reported at %s%s%s", loopParty.Name, now.UTC().Format(time.RFC3339), fileReportSeparator, text)
	key := fmt.Sprintf("%s%s %d", loopReviewStallKey, oldest.since.UTC().Format("20060102T150405Z"), n)
	if err := l.record(superloop.Action{Kind: superloop.KindFile, Key: key}, line); err != nil {
		return err
	}
	l.mu.Lock()
	if l.stall.reviewSince.Equal(oldest.since) {
		l.stall.reviewFiled = n
	}
	l.mu.Unlock()
	return nil
}

// fillRan marks a run of the refill: the worker stream reached it.
func (l *supervisorLoop) fillRan() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stall.ranFiled > 0 {
		_, _ = fmt.Fprintf(l.a.out, "SUPERVISOR STALL ENDED: the capacity refill runs again (not since %s)\n", l.stall.ran.UTC().Format(time.RFC3339))
	}
	l.stall.ran, l.stall.ranFiled = l.a.timeNow(), 0
}

// stallWait records that the refill had room yet started nothing, and why.
// The episode keeps its first time; the item and cause are the latest.
func (l *supervisorLoop) stallWait(ref, why string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stall.since.IsZero() {
		l.stall.since = l.a.timeNow()
		l.stall.filed = 0
	}
	l.stall.ref, l.stall.why = ref, why
}

// stallClear ends a start wait: a worker started, the board offers nothing
// or no slot is free.
func (l *supervisorLoop) stallClear() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stall.filed > 0 {
		_, _ = fmt.Fprintf(l.a.out, "SUPERVISOR STALL ENDED: worker starts flow again (stalled since %s)\n", l.stall.since.UTC().Format(time.RFC3339))
	}
	l.stall.since, l.stall.ref, l.stall.why, l.stall.filed = time.Time{}, "", "", 0
}

// checkStall raises one alarm, through the loop's file channel that guide
// watch says as GUIDE PROBLEM, when worker starts do not move: the refill
// has not run for loopStallAfter (the worker stream is stuck ahead of it),
// or it had room and a board item for loopStallAfter yet started nothing.
// The alarm names the cause it read, once per episode.
func (l *supervisorLoop) checkStall() error {
	if err := l.checkReviewStall(); err != nil {
		return err
	}
	if l.a.cfg.Supervisor.WorkerBrief == "" {
		return nil
	}
	now := l.a.timeNow()
	l.mu.Lock()
	if l.stall.ran.IsZero() {
		l.stall.ran = now // the loop's start is the baseline
	}
	s := l.stall
	l.mu.Unlock()
	var since time.Time
	var cause string
	filed := &l.stall.filed
	switch {
	case now.Sub(s.ran) >= loopStallAfter:
		since, filed = s.ran, &l.stall.ranFiled
		cause = fmt.Sprintf("the capacity refill has not run since %s: the worker stream is still busy with done, intake or next actions of an earlier pass, so no board item gets a worker", s.ran.UTC().Format(time.RFC3339))
	case !s.since.IsZero() && now.Sub(s.since) >= loopStallAfter:
		since = s.since
		item := "no board item read"
		if s.ref != "" {
			item = s.ref
		}
		cause = fmt.Sprintf("the refill had free slots since %s yet started no worker (%s): %s", s.since.UTC().Format(time.RFC3339), item, s.why)
	default:
		return nil
	}
	n := int(now.Sub(since) / loopStallAfter)
	l.mu.Lock()
	raised := *filed
	l.mu.Unlock()
	if raised > 0 || n == 0 {
		return nil
	}
	text := fmt.Sprintf("Worker starts are stalled: %s. Own the cause to the component it names; the alarm is delivered once per waiting episode.", cause)
	line := fmt.Sprintf("%s reported at %s%s%s", loopParty.Name, now.UTC().Format(time.RFC3339), fileReportSeparator, text)
	key := fmt.Sprintf("%s%s %d", loopStallKey, since.UTC().Format("20060102T150405Z"), n)
	if err := l.record(superloop.Action{Kind: superloop.KindFile, Key: key}, line); err != nil {
		return err
	}
	l.mu.Lock()
	*filed = n
	l.mu.Unlock()
	return nil
}

// flowWait is one exact-head merge decision that has not settled.
// It is independent of refill progress: starting workers must not hide a
// merge lane that has stopped moving.
type flowWait struct {
	ref   string
	since time.Time
	filed int
}

// checkFlowWaits measures the planner's pending merge decisions
// every pass. Holds and in-flight commands remain pending, so they cannot
// silently turn a stalled lane into a healthy one. A new head starts a new
// episode; a settled decision clears it. Unread pulls retain their episodes.
func (l *supervisorLoop) checkFlowWaits(in superloop.Input, actions []superloop.Action, failures map[string]loopFailure) error {
	now := l.a.timeNow()
	pending := map[string]superloop.Action{}
	for _, act := range actions {
		if act.Kind == superloop.KindGo {
			pending[act.Key] = act
		}
	}
	if l.flowWaits == nil {
		l.flowWaits = map[string]flowWait{}
	}
	for key, wait := range l.flowWaits {
		if _, ok := pending[key]; ok {
			continue
		}
		// An unread GitHub observation is not evidence of recovery.
		ref := wait.ref
		if _, observed := in.Pulls[ref]; !observed {
			continue
		}
		if wait.filed > 0 {
			_, _ = fmt.Fprintf(l.a.out, "SUPERVISOR STALL ENDED: %s is no longer pending\n", key)
		}
		delete(l.flowWaits, key)
	}
	keys := make([]string, 0, len(pending))
	for key := range pending {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		act := pending[key]
		wait, exists := l.flowWaits[key]
		if !exists {
			wait.since, wait.ref = now, act.Ref
		}
		n := int(now.Sub(wait.since) / loopStallAfter)
		if n > 0 && wait.filed == 0 {
			cause := "the planner still requests this decision; no successful completion recorded"
			l.mu.Lock()
			failure := failures[key]
			completed := l.completed[key]
			if l.inFlight[key] {
				cause = "its command is still in flight"
			}
			l.mu.Unlock()
			if completed {
				delete(l.flowWaits, key)
				continue
			}
			if failure.Message != "" {
				cause = oneLine(failure.Message)
			}
			text := fmt.Sprintf("The supervisor %s decision for %s at %s has waited since %s: %s. Pending merge decisions: %d; the alarm is delivered once per pending head.", act.Kind, act.URL, act.Head, wait.since.UTC().Format(time.RFC3339), cause, len(pending))
			line := fmt.Sprintf("%s reported at %s%s%s", loopParty.Name, now.UTC().Format(time.RFC3339), fileReportSeparator, text)
			alarmKey := fmt.Sprintf("stall decision %s %s %d", key, wait.since.UTC().Format("20060102T150405Z"), n)
			if err := l.record(superloop.Action{Kind: superloop.KindFile, Key: alarmKey}, line); err != nil {
				return err
			}
			wait.filed = n
		}
		l.flowWaits[key] = wait
	}
	return nil
}
