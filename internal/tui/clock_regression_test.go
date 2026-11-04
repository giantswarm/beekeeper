package tui

import (
	"testing"
	"time"
)

func TestClockUsesCalendarDaysAcrossDST(t *testing.T) {
	zone, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	previous := time.Local
	time.Local = zone
	t.Cleanup(func() { time.Local = previous })
	for _, tc := range []struct {
		name string
		now  time.Time
		at   time.Time
		want string
	}{
		{
			name: "tomorrow after a 23 hour day",
			now:  time.Date(2026, 3, 29, 12, 0, 0, 0, zone),
			at:   time.Date(2026, 3, 30, 0, 30, 0, 0, zone),
			want: "+1d 00:30",
		},
		{
			name: "day after tomorrow after a 23 hour day",
			now:  time.Date(2026, 3, 29, 12, 0, 0, 0, zone),
			at:   time.Date(2026, 3, 31, 0, 30, 0, 0, zone),
			want: "Mar 31 00:30",
		},
		{
			name: "late today on a 25 hour day",
			now:  time.Date(2026, 10, 25, 12, 0, 0, 0, zone),
			at:   time.Date(2026, 10, 25, 23, 30, 0, 0, zone),
			want: "23:30",
		},
		{
			name: "late tomorrow after a 25 hour day",
			now:  time.Date(2026, 10, 25, 12, 0, 0, 0, zone),
			at:   time.Date(2026, 10, 26, 23, 30, 0, 0, zone),
			want: "+1d 23:30",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := clock(tc.at, tc.now); got != tc.want {
				t.Errorf("clock = %q, want %q", got, tc.want)
			}
		})
	}
}
