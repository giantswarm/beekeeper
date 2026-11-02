package cmd

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// The timer a fired timer's command runs under (runTimer): an agents start
// it runs says which timer started the session, when it was due and how
// late it fired.
const (
	envTimerID   = "BEEKEEPER_TIMER_ID"
	envTimerDue  = "BEEKEEPER_TIMER_DUE"
	envTimerLate = "BEEKEEPER_TIMER_LATE"
)

// startTimer is the timer whose command runs an agents start: its id, its
// time and how late it fired and why (timerLate), empty when on time.
type startTimer struct {
	id   int
	due  time.Time
	late string
}

// timerOfStart reads the timer a timer's command hands an agents start, and
// takes it out of the environment, so the started session and its own
// starts never pass for the timer's. ok is false outside a timer.
func timerOfStart() (startTimer, bool) {
	id, idErr := strconv.Atoi(os.Getenv(envTimerID))
	due, dueErr := time.Parse(time.RFC3339, os.Getenv(envTimerDue))
	late := os.Getenv(envTimerLate)
	for _, k := range []string{envTimerID, envTimerDue, envTimerLate} {
		_ = os.Unsetenv(k)
	}
	if idErr != nil || dueErr != nil {
		return startTimer{}, false
	}
	return startTimer{id: id, due: due, late: late}, true
}

// line is the started session's first line and the start's log entry: the
// timer, its due time and the start in one zone, and, for a start more
// than one tick past its time or one the capacity gate held, how late it
// is and what held it: the timer's own late firing, the gate's wait.
func (s startTimer) line(now, started time.Time, gate, tick time.Duration) string {
	l := fmt.Sprintf("Started by beekeeper timer #%d: due %s, started %s", s.id, clockZone(now, s.due), clockZone(now, started))
	var why []string
	if s.late != "" {
		why = append(why, "the timer fired "+s.late)
	}
	if gate >= timerTick {
		why = append(why, "the capacity gate held the start "+dur(gate))
	}
	late := started.Sub(s.due)
	switch {
	case lateBy(late, tick):
		if len(why) == 0 {
			why = append(why, "nothing on record held it")
		}
		l += ", " + dur(late) + " late"
	case len(why) == 0:
		return l + "."
	}
	return l + ": " + strings.Join(why, "; ") + "."
}

// timerTicks is the longer of the watch's and the supervisor loop's tick,
// either of which fires a timer.
func (a *app) timerTicks() time.Duration {
	return max(a.cfg.Watch.Interval.Duration, a.cfg.Supervisor.LoopInterval.Duration)
}

// recordTimerStart says and logs the timer line of a start the capacity
// gate let through after gate, and returns it for the session's first line.
func (a *app) recordTimerStart(name string, tm startTimer, gate time.Duration) (string, error) {
	line := tm.line(a.now, a.now.Add(gate), gate, a.timerTicks())
	by, err := a.caller()
	if err != nil {
		return "", err
	}
	if err := a.store.Record(event(by, verbStart, "%s: %s", name, line)); err != nil {
		return "", err
	}
	_, err = fmt.Fprintln(a.out, line)
	return line, err
}

// recordTimerRefusal logs a timer's start the capacity gate refused, with
// the timer's due time and how late it fired; a failed record leaves the
// refusal itself to say it.
func (a *app) recordTimerRefusal(name string, tm startTimer, refusal error) {
	by, err := a.caller()
	if err != nil {
		return
	}
	l := fmt.Sprintf("beekeeper timer #%d (due %s) did not start it: the capacity gate refused: %v", tm.id, clockZone(a.now, tm.due), refusal)
	if tm.late != "" {
		l += "; the timer fired " + tm.late
	}
	_ = a.store.Record(event(by, verbStart, "%s: %s", name, l))
}
