package tui

import (
	"testing"
	"time"
)

func TestClockCalendarDays(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	previousLocal := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = previousLocal })

	for _, date := range []struct {
		name  string
		month time.Month
		day   int
	}{
		{"spring forward", time.March, 29},
		{"fall back", time.October, 25},
		{"year rollover", time.December, 31},
	} {
		t.Run(date.name, func(t *testing.T) {
			midnight := time.Date(2026, date.month, date.day, 0, 0, 0, 0, loc)
			now := time.Date(2026, date.month, date.day, 12, 0, 0, 0, loc)
			tomorrow := midnight.AddDate(0, 0, 1)
			following := midnight.AddDate(0, 0, 2)
			for _, tc := range []struct {
				name string
				at   time.Time
				want string
			}{
				{"yesterday", midnight.Add(-time.Minute), midnight.Add(-time.Minute).Format("Jan 2 15:04")},
				{"today starts", midnight, "00:00"},
				{"today ends", tomorrow.Add(-30 * time.Minute), "23:30"},
				{"tomorrow starts", tomorrow, "+1d 00:00"},
				{"tomorrow early", tomorrow.Add(30 * time.Minute), "+1d 00:30"},
				{"tomorrow ends", following.Add(-time.Minute), "+1d 23:59"},
				{"following day starts", following, following.Format("Jan 2 15:04")},
			} {
				t.Run(tc.name, func(t *testing.T) {
					// UTC inputs must still be classified and displayed in local time.
					if got := clock(tc.at.UTC(), now.UTC()); got != tc.want {
						t.Errorf("clock() = %q, want %q", got, tc.want)
					}
				})
			}
		})
	}
}
