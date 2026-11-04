package cmd

import (
	"strings"
	"testing"
	"time"
)

const timerPastClock = "13:00"

func TestTimerStartTime(t *testing.T) {
	now := time.Date(2026, 9, 24, 14, 30, 15, 0, time.Local)
	for _, tc := range []struct {
		input string
		want  time.Time
	}{
		{"14:30", now},
		{"2026-09-24 13:00", now},
		{now.Add(-time.Hour).Format(time.RFC3339), now},
		{"-1m", now},
		{"15:00", time.Date(2026, 9, 24, 15, 0, 0, 0, time.Local)},
		{"2026-09-25 13:00", time.Date(2026, 9, 25, 13, 0, 0, 0, time.Local)},
		{"5m", now.Add(5 * time.Minute)},
		{"", time.Time{}},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := timerStartTime(now, tc.input)
			if err != nil || !got.Equal(tc.want) {
				t.Fatalf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
	if _, err := timerStartTime(now, "invalid"); Code(err) != ExitUsage {
		t.Fatalf("invalid time: %v", err)
	}
}

// A past clock time names both times and asks for a date: it neither fires at
// once nor rolls silently.
func TestTimerPastClockTimeIsRefused(t *testing.T) {
	now := time.Date(2026, 9, 24, 14, 30, 15, 0, time.Local)
	_, err := timerStartTime(now, timerPastClock)
	if Code(err) != ExitRefused {
		t.Fatalf("past clock time: %v", err)
	}
	for _, want := range []string{timerPastClock, "14:30", "2026-09-25 13:00"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q: %v", want, err)
		}
	}
}

func TestTimerPastStartAcceptsUntil(t *testing.T) {
	now := time.Date(2026, 9, 24, 14, 30, 0, 0, time.Local)
	a := &app{now: now}
	for _, start := range []string{"2026-09-24 13:00", now.Add(-time.Hour).Format(time.RFC3339)} {
		t.Run(start, func(t *testing.T) {
			due, err := timerStartTime(now, start)
			if err != nil {
				t.Fatal(err)
			}
			timer, err := a.timerFrom(timerSpec{probe: probeHolds, until: "15:00"}, due)
			if err != nil {
				t.Fatal(err)
			}
			if !timer.Due.Equal(now) || !timer.Until.Equal(now.Add(30*time.Minute)) {
				t.Fatalf("unexpected timer: %+v", timer)
			}
			if got := timerWhen(now, timer); !strings.Contains(got, "from 14:30") {
				t.Fatalf("start not displayed: %s", got)
			}
		})
	}
	// Deadlines still support waiting overnight.
	due, _ := timerStartTime(now, "23:00")
	timer, err := a.timerFrom(timerSpec{probe: probeHolds, until: "06:00"}, due)
	if err != nil || !timer.Until.Equal(time.Date(2026, 9, 25, 6, 0, 0, 0, time.Local)) {
		t.Fatalf("overnight deadline: %+v, %v", timer, err)
	}
}
