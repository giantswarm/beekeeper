package loop

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/superloop"
)

// LoopStallAfter is how long the refill may not run, or a start wait with
// room, before the loop raises the alarm; the alarm is raised once
// per episode until the flow moves again.
const LoopStallAfter = 15 * time.Minute

const LoopStallKey = "stall starts "

// startStall is the refill's flow as the loop measures it: when fill last
// ran, and since when (with which item and cause) it had room and the board
// offered work, yet started nothing.
type startStall struct {
	Ran   time.Time
	Since time.Time
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

const LoopReviewStallKey = "stall reviews "

// reviewWait is a review request the loop planned and has not started:
// since when, and the cause its last try read.
type reviewWait struct {
	since    time.Time
	url, why string
}

// reviewsPlanned keeps the review waits of this pass's review actions; a
// request no longer planned (started, merged, its head moved) waits no more.
func (l *Loop) reviewsPlanned(acts []superloop.Action) {
	now := l.Host.TimeNow()
	l.Mu.Lock()
	defer l.Mu.Unlock()
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
func (l *Loop) reviewTried(act superloop.Action, failures map[string]LoopFailure) {
	l.Mu.Lock()
	defer l.Mu.Unlock()
	w, ok := l.reviewWaits[act.Ref]
	if !ok {
		return
	}
	if l.completed[act.Key] {
		delete(l.reviewWaits, act.Ref)
		return
	}
	if f, failed := failures[act.Key]; failed && f.Message != "" {
		w.why = "its start failed: " + Cmd.OneLine(f.Message)
	} else {
		w.why = "its start waits: no room for a start on any host, held starts, an incident or an unread repository (the SUPERVISOR REVIEW WAITS lines name it)"
	}
	l.reviewWaits[act.Ref] = w
}

// checkReviewStall raises one alarm when a review request has waited
// LoopStallAfter without a reviewer: it names the oldest request, its
// cause and how many wait, once per oldest waiting episode.
func (l *Loop) checkReviewStall() error {
	now := l.Host.TimeNow()
	l.Mu.Lock()
	var oldest reviewWait
	waiting := 0
	for _, w := range l.reviewWaits {
		if now.Sub(w.since) < LoopStallAfter {
			continue
		}
		waiting++
		if oldest.since.IsZero() || w.since.Before(oldest.since) || w.since.Equal(oldest.since) && w.url < oldest.url {
			oldest = w
		}
	}
	if waiting == 0 {
		if l.Stall.reviewFiled > 0 {
			_, _ = fmt.Fprintf(l.Host.Out(), "SUPERVISOR STALL ENDED: no review request waits %s for a reviewer any more (stalled since %s)\n", LoopStallAfter, l.Stall.reviewSince.UTC().Format(time.RFC3339))
		}
		l.Stall.reviewSince, l.Stall.reviewFiled = time.Time{}, 0
		l.Mu.Unlock()
		return nil
	}
	if !oldest.since.Equal(l.Stall.reviewSince) {
		l.Stall.reviewSince, l.Stall.reviewFiled = oldest.since, 0
	}
	n := int(now.Sub(oldest.since) / LoopStallAfter)
	raised := l.Stall.reviewFiled
	l.Mu.Unlock()
	if raised > 0 || n == 0 {
		return nil
	}
	text := fmt.Sprintf("Reviews are stalled: %d review request(s) waited %s or longer for a reviewer; the oldest, %s, waits since %s: %s. Own the cause to the component it names; the alarm is delivered once per waiting episode.", waiting, LoopStallAfter, oldest.url, oldest.since.UTC().Format(time.RFC3339), oldest.why)
	line := fmt.Sprintf("%s reported at %s%s%s", LoopParty.Name, now.UTC().Format(time.RFC3339), FileReportSeparator, text)
	key := fmt.Sprintf("%s%s %d", LoopReviewStallKey, oldest.since.UTC().Format("20060102T150405Z"), n)
	if err := l.Record(superloop.Action{Kind: superloop.KindFile, Key: key}, line); err != nil {
		return err
	}
	l.Mu.Lock()
	if l.Stall.reviewSince.Equal(oldest.since) {
		l.Stall.reviewFiled = n
	}
	l.Mu.Unlock()
	return nil
}

// FillRan marks a run of the refill: the worker stream reached it.
func (l *Loop) FillRan() {
	l.Mu.Lock()
	defer l.Mu.Unlock()
	if l.Stall.ranFiled > 0 {
		_, _ = fmt.Fprintf(l.Host.Out(), "SUPERVISOR STALL ENDED: the capacity refill runs again (not since %s)\n", l.Stall.Ran.UTC().Format(time.RFC3339))
	}
	l.Stall.Ran, l.Stall.ranFiled = l.Host.TimeNow(), 0
}

// StallWait records that the refill had room yet started nothing, and why.
// The episode keeps its first time; the item and cause are the latest.
func (l *Loop) StallWait(ref, why string) {
	l.Mu.Lock()
	defer l.Mu.Unlock()
	if l.Stall.Since.IsZero() {
		l.Stall.Since = l.Host.TimeNow()
		l.Stall.filed = 0
	}
	l.Stall.ref, l.Stall.why = ref, why
}

// StallClear ends a start wait: a worker started, the board offers nothing
// or no slot is free.
func (l *Loop) StallClear() {
	l.Mu.Lock()
	defer l.Mu.Unlock()
	if l.Stall.filed > 0 {
		_, _ = fmt.Fprintf(l.Host.Out(), "SUPERVISOR STALL ENDED: worker starts flow again (stalled since %s)\n", l.Stall.Since.UTC().Format(time.RFC3339))
	}
	l.Stall.Since, l.Stall.ref, l.Stall.why, l.Stall.filed = time.Time{}, "", "", 0
}

// CheckStall raises one alarm, through the loop's file channel that guide
// watch says as GUIDE PROBLEM, when worker starts do not move: the refill
// has not run for LoopStallAfter (the worker stream is stuck ahead of it),
// or it had room and a board item for LoopStallAfter yet started nothing.
// The alarm names the cause it read, once per episode.
func (l *Loop) CheckStall() error {
	if err := l.checkReviewStall(); err != nil {
		return err
	}
	if l.Host.Cfg().Supervisor.WorkerBrief == "" {
		return nil
	}
	now := l.Host.TimeNow()
	l.Mu.Lock()
	if l.Stall.Ran.IsZero() {
		l.Stall.Ran = now // the loop's start is the baseline
	}
	s := l.Stall
	l.Mu.Unlock()
	var since time.Time
	var cause string
	filed := &l.Stall.filed
	switch {
	case now.Sub(s.Ran) >= LoopStallAfter:
		since, filed = s.Ran, &l.Stall.ranFiled
		cause = fmt.Sprintf("the capacity refill has not run since %s: %s", s.Ran.UTC().Format(time.RFC3339), l.refillBlocker())
	case !s.Since.IsZero() && now.Sub(s.Since) >= LoopStallAfter:
		since = s.Since
		item := "no board item read"
		if s.ref != "" {
			item = s.ref
		}
		cause = fmt.Sprintf("the refill had free slots since %s yet started no worker (%s): %s", s.Since.UTC().Format(time.RFC3339), item, s.why)
	default:
		return nil
	}
	n := int(now.Sub(since) / LoopStallAfter)
	l.Mu.Lock()
	raised := *filed
	l.Mu.Unlock()
	if raised > 0 || n == 0 {
		return nil
	}
	text := fmt.Sprintf("Worker starts are stalled: %s. Own the cause to the component it names; the alarm is delivered once per waiting episode.", cause)
	line := fmt.Sprintf("%s reported at %s%s%s", LoopParty.Name, now.UTC().Format(time.RFC3339), FileReportSeparator, text)
	key := fmt.Sprintf("%s%s %d", LoopStallKey, since.UTC().Format("20060102T150405Z"), n)
	if err := l.Record(superloop.Action{Kind: superloop.KindFile, Key: key}, line); err != nil {
		return err
	}
	l.Mu.Lock()
	*filed = n
	l.Mu.Unlock()
	return nil
}

// flowWait is one exact-head merge decision that has not settled.
// It is independent of refill progress: starting workers must not hide a
// merge lane that has stopped moving.
type flowWait struct {
	Act   superloop.Action
	Since time.Time
	// alarmed are the classes of hold the alarm named, last when it last
	// rang.
	alarmed []string
	last    time.Time
}

// The classes of what holds a merge decision. The stall alarm rings once per
// pending head and class, so a decision whose hold changes is named again.
const (
	waitInFlight = "in flight"
	waitLane     = "lane"
	WaitHold     = "hold"
	waitBudget   = "GitHub budget"
	waitOutage   = "outage"
	waitGate     = "merge gate"
	waitPolicy   = "merge policy"
	waitQueue    = "merge queue"
	WaitUnread   = "unread"
	waitRepair   = "repair"
	waitFailed   = "failed"
	waitRestart  = "restart"
	waitPass     = "pass"
	waitUnnamed  = "unnamed"
)

// mergeWait is ErrLoopLater with what the merge decision waits on: the stall
// alarm names it instead of a decision that silently never completes
// (https://github.com/giantswarm/beekeeper/issues/3820).
type mergeWait struct{ class, cause string }

func (w *mergeWait) Error() string { return w.cause + ": " + ErrLoopLater.Error() }
func (w *mergeWait) Unwrap() error { return ErrLoopLater }

// MergeHeld is a wait of class for a later pass, with its cause.
func MergeHeld(class, format string, args ...any) error {
	return &mergeWait{class: class, cause: fmt.Sprintf(format, args...)}
}

// laterCause is the cause a wrapped ErrLoopLater names, "" for a bare one.
func laterCause(err error) string {
	text := strings.TrimSpace(strings.TrimSuffix(err.Error(), ErrLoopLater.Error()))
	return strings.TrimSpace(strings.TrimSuffix(text, ":"))
}

// namedWait gives a wait read below the merge path its class and says what
// the decision was doing (prefix). A named wait and any other error stand.
func namedWait(err error, class, prefix string) error {
	var named *mergeWait
	if err == nil || !errors.Is(err, ErrLoopLater) || errors.As(err, &named) {
		return err
	}
	cause := laterCause(err)
	switch {
	case cause == "":
		cause = prefix
	case prefix != "":
		cause = prefix + ": " + cause
	}
	return &mergeWait{class: class, cause: cause}
}

// mergeNote is what a merge decision's last run read: the hold it waits on.
type mergeNote struct {
	Class, Cause string
	at           time.Time
}

// mergeRun is a merge decision dispatched to its lane's stream: since when
// its command runs, or the pull request whose command runs ahead of it.
type mergeRun struct {
	since  time.Time
	behind string
}

// MergeWaited keeps why a merge decision's run did not settle it.
func (l *Loop) MergeWaited(act superloop.Action, err error) {
	if act.Kind != superloop.KindGo {
		return
	}
	note := mergeNote{Class: waitUnnamed, Cause: laterCause(err), at: l.Host.TimeNow()}
	var named *mergeWait
	if errors.As(err, &named) {
		note.Class, note.Cause = named.class, named.cause
	}
	if note.Cause == "" {
		note.Cause = "its run waits for a later pass and named no cause"
	}
	l.Mu.Lock()
	defer l.Mu.Unlock()
	if l.MergeNotes == nil {
		l.MergeNotes = map[string]mergeNote{}
	}
	l.MergeNotes[act.Key] = note
}

// mergeQueued marks a lane's merge decisions dispatched: the first runs, the
// others wait behind it in the loop's stream for the lane.
func (l *Loop) mergeQueued(acts []superloop.Action) {
	l.Mu.Lock()
	defer l.Mu.Unlock()
	if l.mergeRuns == nil {
		l.mergeRuns = map[string]mergeRun{}
	}
	for i, act := range acts {
		run := mergeRun{}
		if i > 0 {
			run.behind = acts[0].URL
		}
		l.mergeRuns[act.Key] = run
	}
}

// MergeStarted marks acts[i] running and the rest of its stream behind it.
func (l *Loop) MergeStarted(acts []superloop.Action, i int) {
	now := l.Host.TimeNow()
	l.Mu.Lock()
	defer l.Mu.Unlock()
	if l.mergeRuns == nil {
		l.mergeRuns = map[string]mergeRun{}
	}
	l.mergeRuns[acts[i].Key] = mergeRun{since: now}
	for _, later := range acts[i+1:] {
		l.mergeRuns[later.Key] = mergeRun{behind: acts[i].URL}
	}
}

// MergeStreamEnded forgets the runs of a lane's stream that ended.
func (l *Loop) MergeStreamEnded(acts []superloop.Action) {
	l.Mu.Lock()
	defer l.Mu.Unlock()
	for _, act := range acts {
		delete(l.mergeRuns, act.Key)
	}
}

// flowCause reads what holds the pending merge decision of key now, by
// class: its command in flight, its lane's stream, or the newer of its last
// wait and its last failure. settled reports a decision completed since the
// pass planned it.
func (l *Loop) flowCause(key string, failures map[string]LoopFailure) (class, cause string, settled bool) {
	now := l.Host.TimeNow()
	l.Mu.Lock()
	defer l.Mu.Unlock()
	if l.completed[key] {
		return "", "", true
	}
	if l.inFlight[key] {
		run := l.mergeRuns[key]
		switch {
		case run.behind != "":
			return waitLane, fmt.Sprintf("it is queued in the loop's merge stream of its lane behind %s, whose merge command is still in flight", run.behind), false
		case !run.since.IsZero():
			return waitInFlight, fmt.Sprintf("its merge command has been in flight since %s: the merge gate holds it for its lane's turn, the merge-result check and GitHub's answer", run.since.UTC().Format(time.RFC3339)), false
		}
		return waitInFlight, "its command is still in flight", false
	}
	note, noted := l.MergeNotes[key]
	if f := failures[key]; f.Message != "" && (!noted || f.At.After(note.at)) {
		cause := "its last run failed: " + Cmd.OneLine(f.Message)
		if f.RetryAt.After(now) {
			cause += fmt.Sprintf(" (identical failures; the loop retries after %s)", f.RetryAt.UTC().Format(time.RFC3339))
		}
		return waitFailed, cause, false
	}
	if noted {
		return note.Class, note.Cause, false
	}
	return waitUnnamed, "the planner still requests this decision; no run of it recorded a completion, a wait or a failure", false
}

// flowAlarm rings for a merge decision pending LoopStallAfter or longer:
// once per pending head and class of hold, and no sooner than LoopStallAfter
// after the head's last alarm, so a hold that changes is named and one that
// flaps is not repeated.
func (l *Loop) flowAlarm(key string, wait *flowWait, class, cause string, pending int) error {
	now := l.Host.TimeNow()
	if now.Sub(wait.Since) < LoopStallAfter || slices.Contains(wait.alarmed, class) {
		return nil
	}
	if !wait.last.IsZero() && now.Sub(wait.last) < LoopStallAfter {
		return nil
	}
	// A restarted loop has run nothing yet: it names a restored wait once
	// its own run read the hold.
	if class == waitUnnamed && now.Sub(l.StartedAt) < LoopStallAfter {
		return nil
	}
	n := int(now.Sub(wait.Since) / LoopStallAfter)
	text := fmt.Sprintf("The supervisor %s decision for %s at %s has waited since %s: %s. Pending merge decisions: %d; the alarm is delivered once per pending head and cause.", wait.Act.Kind, wait.Act.URL, wait.Act.Head, wait.Since.UTC().Format(time.RFC3339), cause, pending)
	line := fmt.Sprintf("%s reported at %s%s%s", LoopParty.Name, now.UTC().Format(time.RFC3339), FileReportSeparator, text)
	alarmKey := fmt.Sprintf("stall decision %s %s %d %s", key, wait.Since.UTC().Format("20060102T150405Z"), n, class)
	if err := l.Record(superloop.Action{Kind: superloop.KindFile, Key: alarmKey}, line); err != nil {
		return err
	}
	wait.alarmed = append(wait.alarmed, class)
	wait.last = now
	return nil
}

// CheckFlowWaits measures the planner's pending merge decisions
// every pass. Holds and in-flight commands remain pending, so they cannot
// silently turn a stalled lane into a healthy one. A new head starts a new
// episode; a settled decision clears it. Unread pulls retain their episodes
// and are alarmed as unread: no pass plans or runs their decision.
func (l *Loop) CheckFlowWaits(in superloop.Input, actions []superloop.Action, failures map[string]LoopFailure) error {
	now := l.Host.TimeNow()
	pending := map[string]superloop.Action{}
	for _, act := range actions {
		if act.Kind == superloop.KindGo && !staleMergeDecision(in.Pulls[act.Ref], act.Head) {
			pending[act.Key] = act
		}
	}
	if l.FlowWaits == nil {
		l.FlowWaits = map[string]flowWait{}
	}
	var unread []string
	for key, wait := range l.FlowWaits {
		if _, ok := pending[key]; ok {
			continue
		}
		// An unread GitHub observation is not evidence of recovery.
		if _, observed := in.Pulls[wait.Act.Ref]; !observed {
			unread = append(unread, key)
			continue
		}
		if len(wait.alarmed) > 0 {
			_, _ = fmt.Fprintf(l.Host.Out(), "SUPERVISOR STALL ENDED: %s is no longer pending\n", key)
		}
		delete(l.FlowWaits, key)
	}
	keys := make([]string, 0, len(pending))
	for key := range pending {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	total := len(pending) + len(unread)
	for _, key := range keys {
		wait, exists := l.FlowWaits[key]
		if !exists {
			wait.Since = now
		}
		wait.Act = pending[key]
		class, cause, settled := l.flowCause(key, failures)
		if settled {
			delete(l.FlowWaits, key)
			continue
		}
		if err := l.flowAlarm(key, &wait, class, cause, total); err != nil {
			return err
		}
		l.FlowWaits[key] = wait
	}
	slices.Sort(unread)
	for _, key := range unread {
		wait := l.FlowWaits[key]
		class, cause, settled := l.flowCause(key, failures)
		if settled {
			delete(l.FlowWaits, key)
			continue
		}
		if class != waitInFlight && class != waitLane {
			class = WaitUnread
			cause = fmt.Sprintf("the loop has no current observation of %s (the SUPERVISOR LOOP cannot read and SUPERVISOR PULL WATCH lines name why), so no pass plans or runs its decision", wait.Act.Ref)
		}
		if err := l.flowAlarm(key, &wait, class, cause, total); err != nil {
			return err
		}
		l.FlowWaits[key] = wait
	}
	// A decision no longer pending leaves no note behind.
	l.Mu.Lock()
	for key := range l.MergeNotes {
		if _, waits := l.FlowWaits[key]; !waits {
			delete(l.MergeNotes, key)
		}
	}
	l.Mu.Unlock()
	return nil
}

// flowWaitsUnplanned keeps the alarm alive on a pass that ended before it
// planned: held GitHub observations, a restart or a failed pass run no merge
// decision, and must not silence the ones already pending. why names what
// ended the pass.
func (l *Loop) flowWaitsUnplanned(class, why string) error {
	keys := make([]string, 0, len(l.FlowWaits))
	for key := range l.FlowWaits {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		wait := l.FlowWaits[key]
		held, cause, settled := l.flowCause(key, nil)
		if settled {
			delete(l.FlowWaits, key)
			continue
		}
		if held != waitInFlight && held != waitLane {
			held, cause = class, why
		}
		if err := l.flowAlarm(key, &wait, held, cause, len(keys)); err != nil {
			return err
		}
		l.FlowWaits[key] = wait
	}
	return nil
}

// Missing observations retain pending decisions. Only a terminal state or a
// known replacement head proves that this merge decision has been overtaken.
func staleMergeDecision(p superloop.Pull, head string) bool {
	return p.State == superloop.Merged || p.State == superloop.Closed ||
		(p.Head != "" && p.Head != head)
}
