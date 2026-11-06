package cmd

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
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

// The hidden flags of agents start a brokered start forwards its timer in:
// the broker's request carries the command's flags, never its environment.
const (
	flagTimerID   = "timer-id"
	flagTimerDue  = "timer-due"
	flagTimerLate = "timer-late"
)

// addTimerFlags adds agents start's hidden timer flags.
func addTimerFlags(c *cobra.Command) {
	f := c.Flags()
	f.String(flagTimerID, "", "the timer whose command runs the start (set by the timer, forwarded to the broker)")
	f.String(flagTimerDue, "", "that timer's due time, RFC 3339")
	f.String(flagTimerLate, "", "how late that timer fired and why")
	for _, n := range []string{flagTimerID, flagTimerDue, flagTimerLate} {
		_ = f.MarkHidden(n)
	}
}

// timerOfStart reads the timer whose command runs an agents start: from
// the hidden flags a brokered start forwarded, else from the environment a
// timer's command runs under (runTimer). It takes the timer out of the
// environment, so the started session and its own starts never pass for
// the timer's, and sets it on cmd's flags, so a start the broker runs
// carries it. ok is false outside a timer.
func timerOfStart(cmd *cobra.Command) (startTimer, bool, error) {
	f := cmd.Flags()
	vals := map[string]string{}
	if f.Changed(flagTimerID) {
		for _, n := range []string{flagTimerID, flagTimerDue, flagTimerLate} {
			v, _ := f.GetString(n)
			vals[n] = v
		}
	} else {
		vals[flagTimerID], vals[flagTimerDue], vals[flagTimerLate] = os.Getenv(envTimerID), os.Getenv(envTimerDue), os.Getenv(envTimerLate)
	}
	for _, k := range []string{envTimerID, envTimerDue, envTimerLate, envTimerCommand} {
		_ = os.Unsetenv(k)
	}
	id, idErr := strconv.Atoi(vals[flagTimerID])
	due, dueErr := time.Parse(time.RFC3339, vals[flagTimerDue])
	if idErr != nil || dueErr != nil {
		return startTimer{}, false, nil
	}
	for n, v := range vals {
		if err := f.Set(n, v); err != nil {
			return startTimer{}, false, err
		}
	}
	return startTimer{id: id, due: due, late: vals[flagTimerLate]}, true, nil
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
// gate let through after gate, started now, at the record, and returns it
// for the session's first line.
func (a *app) recordTimerStart(name string, tm startTimer, gate time.Duration) (string, error) {
	line := tm.line(a.now, time.Now(), gate, a.timerTicks())
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
