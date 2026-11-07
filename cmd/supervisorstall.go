package cmd

import (
	"fmt"
	"time"

	"github.com/giantswarm/beekeeper/internal/superloop"
)

// loopStallAfter is how long the refill may not run, or a start wait with
// room, before the loop raises the alarm; the alarm repeats each
// loopStallAfter until the flow moves again.
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
// The alarm names the cause it read and repeats each loopStallAfter until
// the flow moves again.
func (l *supervisorLoop) checkStall() error {
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
	if n <= raised {
		return nil
	}
	text := fmt.Sprintf("Worker starts are stalled: %s. Own the cause to the component it names; the alarm repeats every %s until a worker starts or nothing is left to start.", cause, loopStallAfter)
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
