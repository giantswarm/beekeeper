package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestTimerTime(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for input, want := range map[string]time.Time{
		litNow: now, "7d": now.Add(168 * time.Hour), "1w": now.Add(168 * time.Hour),
		"1w2d3h4m5s": now.Add(219*time.Hour + 4*time.Minute + 5*time.Second),
		"1.5d":       now.Add(36 * time.Hour), ".5w": now.Add(84 * time.Hour),
		"-1d": now.Add(-24 * time.Hour), "30m": now.Add(30 * time.Minute),
		"1ms": now.Add(time.Millisecond), "": {},
		"2026-11-01T12:00:00Z": time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC),
	} {
		got, err := timerTime(now, input)
		if err != nil || !got.Equal(want) {
			t.Errorf("timerTime(%q) = %v, %v; want %v", input, got, err, want)
		}
	}
	for _, input := range []string{"soon", "d", "1dd", "1d-2h", "1e2d", "999999999999999999999d", "15251w"} {
		if _, err := timerTime(now, input); Code(err) != ExitUsage {
			t.Errorf("timerTime(%q) error = %v", input, err)
		}
	}
}

func TestTimerAddNowWithDayAndWeekDeadline(t *testing.T) {
	for _, until := range []string{"7d", "1w", "168h"} {
		t.Run(until, func(t *testing.T) {
			a, _, _ := notifyingWatch(t, t.TempDir(), false)
			watchApp(a.Host).as = "timer-test"
			c := watchApp(a.Host).timerCmd()
			c.SetArgs([]string{"add", "now", "check readiness", "--probe", "true", untilFlag, until})
			if err := c.Execute(); err != nil {
				t.Fatal(err)
			}
			st, err := watchApp(a.Host).store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Timers) != 1 {
				t.Fatalf("timers = %+v", st.Timers)
			}
			timer := st.Timers[0]
			if !timer.Due.Equal(watchApp(a.Host).now) || !timer.Until.Equal(watchApp(a.Host).now.Add(168*time.Hour)) {
				t.Fatalf("timer = %+v", timer)
			}
		})
	}
}

func TestTimerAddHelpListsUnits(t *testing.T) {
	a := &app{}
	c := a.timerCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetArgs([]string{"add", "--help"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"now", "w, d, h, m, s", "24 hours", "168 hours"} {
		if !strings.Contains(out.String(), text) {
			t.Errorf("help lacks %q", text)
		}
	}
}
