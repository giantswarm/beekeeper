package tui

import (
	"testing"
)

func TestDedupAlertsKeepsTheEarliestInstant(t *testing.T) {
	for _, tc := range []struct {
		name    string
		earlier string
		later   string
	}{
		{
			name:    "different UTC offsets",
			earlier: "2026-09-26T10:00:00+02:00",
			later:   "2026-09-26T09:00:00Z",
		},
		{
			name:    "optional fractional seconds",
			earlier: "2026-09-26T09:00:00Z",
			later:   "2026-09-26T09:00:00.1Z",
		},
		{
			name:    "legacy duration ages",
			earlier: "2h",
			later:   "12m",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, stamps := range [][2]string{{tc.earlier, tc.later}, {tc.later, tc.earlier}} {
				as := []Alert{
					{Severity: "page", Alertname: "TargetDown", Cluster: "test", Where: "ns/app", Since: stamps[0]},
					{Severity: "page", Alertname: "TargetDown", Cluster: "test", Where: "ns/app", Since: stamps[1]},
				}
				got := dedupAlerts(as)
				if len(got) != 1 || got[0].n != 2 || got[0].Since != tc.earlier {
					t.Fatalf("dedupAlerts(%v) = %+v, want one row since %s counting both firings", stamps, got, tc.earlier)
				}
			}
		})
	}
}
