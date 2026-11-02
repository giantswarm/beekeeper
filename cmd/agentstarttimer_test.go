package cmd

import (
	"testing"
	"time"
)

// A timer's start prints its due time and the start in one zone, and a
// start past its time says how late it is and what held it: the timer's
// late firing, the capacity gate's wait.
func TestTimerStartLineSaysHowLateAndWhy(t *testing.T) {
	z := relayNow.Format("MST")
	due := relayNow.Add(-14 * time.Minute)
	for name, c := range map[string]struct {
		tm      startTimer
		started time.Time
		gate    time.Duration
		want    string
	}{
		"on time": {startTimer{id: 7, due: relayNow}, relayNow.Add(20 * time.Second), 0,
			"Started by beekeeper timer #7: due 02:00 " + z + ", started 02:00 " + z + "."},
		"fired late": {startTimer{id: 7, due: due, late: "14m late: " + timerHeldDown}, relayNow, 0,
			"Started by beekeeper timer #7: due 01:46 " + z + ", started 02:00 " + z + ", 14m late: the timer fired 14m late: " + timerHeldDown + "."},
		"held by the capacity gate": {startTimer{id: 7, due: due}, relayNow, 13 * time.Minute,
			"Started by beekeeper timer #7: due 01:46 " + z + ", started 02:00 " + z + ", 14m late: the capacity gate held the start 13m."},
		"late with nothing on record": {startTimer{id: 7, due: due}, relayNow, 0,
			"Started by beekeeper timer #7: due 01:46 " + z + ", started 02:00 " + z + ", 14m late: nothing on record held it."},
	} {
		if got := c.tm.line(relayNow, c.started, c.gate, timerTick); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, c.want)
		}
	}
}

// The timer reaches agents start through the environment, once: the
// started session's own starts never pass for the timer's.
func TestTimerOfStartReadsAndClearsTheEnvironment(t *testing.T) {
	t.Setenv(envTimerID, "9")
	t.Setenv(envTimerDue, "2026-11-02T07:30:00Z")
	t.Setenv(envTimerLate, "14m late: down")
	tm, ok := timerOfStart()
	if !ok || tm.id != 9 || !tm.due.Equal(time.Date(2026, 11, 2, 7, 30, 0, 0, time.UTC)) || tm.late != "14m late: down" {
		t.Fatalf("%+v %v", tm, ok)
	}
	if _, ok := timerOfStart(); ok {
		t.Fatal("read the timer twice")
	}
}
