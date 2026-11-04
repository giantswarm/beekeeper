package lease

import (
	"testing"
	"time"
)

func TestRound(t *testing.T) {
	for _, tc := range []struct {
		purpose string
		want    time.Duration
	}{
		{"60-minute round", time.Hour},
		{"proof for 60m", time.Hour},
		{"1h30m round", 90 * time.Minute},
		{"1h30m15s round", 90*time.Minute + 15*time.Second},
		{"a 1.5 hour round", 90 * time.Minute},
		{"a .5h round", 30 * time.Minute},
		{"a .5-hour round", 30 * time.Minute},
		{"a -.5h round", 45 * time.Minute},
		{"a 1h.5m round", time.Hour + 30*time.Second},
		{"version1.5h", 45 * time.Minute},
		{"a -.5h estimate then .5h round", 30 * time.Minute},
		{"30 minutes to prove it", 30 * time.Minute},
		{"2 HOURS", 2 * time.Hour},
		{"90-second check", 90 * time.Second},
		{"no duration", 45 * time.Minute},
		{"version 60main", 45 * time.Minute},
		{"0m round", 45 * time.Minute},
		{"-5m round", 45 * time.Minute},
		{"999999999999999999999h round", 45 * time.Minute},
	} {
		t.Run(tc.purpose, func(t *testing.T) {
			if got := (Holder{Purpose: tc.purpose}).Round(45 * time.Minute); got != tc.want {
				t.Errorf("Round = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestOverrun(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		since string
		want  time.Duration
	}{
		{now.Add(-78 * time.Minute).Format(time.RFC3339), 18 * time.Minute},
		{now.Add(-time.Hour).Format(time.RFC3339), 0},
		{now.Add(time.Hour).Format(time.RFC3339), 0},
		{"", 0},
		{"invalid", 0},
	} {
		h := Holder{Purpose: "60-minute round", Since: tc.since}
		if got, round := h.Overrun(now, 30*time.Minute); got != tc.want || round != time.Hour {
			t.Errorf("Overrun(%q) = %s, %s; want %s, 1h", tc.since, got, round, tc.want)
		}
	}
}

func TestFractionalRoundOverrun(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := Holder{Purpose: "a .5h round", Since: now.Add(-45 * time.Minute).Format(time.RFC3339)}
	if over, round := h.Overrun(now, time.Hour); over != 15*time.Minute || round != 30*time.Minute {
		t.Fatalf("Overrun = %s, %s; want 15m, 30m", over, round)
	}
}
