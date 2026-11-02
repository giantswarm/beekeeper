package cmd

import (
	"slices"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/omp"
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
// started session's own starts never pass for the timer's. An auto start
// whose choice goes to the broker carries the timer in the forwarded
// flags, and the broker's start reads the same timer from them.
func TestTimerOfStartReachesTheBrokeredStart(t *testing.T) {
	want := startTimer{id: 9, due: time.Date(2026, 11, 2, 7, 30, 0, 0, time.UTC), late: "14m late: down"}
	t.Setenv(envTimerID, "9")
	t.Setenv(envTimerDue, "2026-11-02T07:30:00Z")
	t.Setenv(envTimerLate, want.late)
	a := &app{cfg: &config.Config{}}
	c := a.agentStartCmd()
	if err := c.ParseFlags([]string{autoHarnessFlag}); err != nil {
		t.Fatal(err)
	}
	tm, ok, err := timerOfStart(c)
	if err != nil || !ok || tm.id != want.id || !tm.due.Equal(want.due) || tm.late != want.late {
		t.Fatalf("%+v %v %v", tm, ok, err)
	}
	if _, ok, _ := timerOfStart(a.agentStartCmd()); ok {
		t.Fatal("a second start read the timer from the environment")
	}

	if err := forwardBrokeredChoice(c, omp.Harness, sparkModel, "omp / spark/m (provider omp)"); err != nil {
		t.Fatal(err)
	}
	argv, err := a.scratchArgv(c, []string{"w", briefMD})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"--timer-id=9", "--timer-due=2026-11-02T07:30:00Z", "--timer-late=" + want.late} {
		if !slices.Contains(argv, f) {
			t.Fatalf("the brokered start's argv %q lacks %s", argv, f)
		}
	}
	host := a.agentStartCmd()
	if err := host.ParseFlags(argv[1:slices.Index(argv, "--")]); err != nil {
		t.Fatal(err)
	}
	tm, ok, err = timerOfStart(host)
	if err != nil || !ok || tm.id != want.id || !tm.due.Equal(want.due) || tm.late != want.late {
		t.Fatalf("the broker's start read %+v %v %v", tm, ok, err)
	}
}
