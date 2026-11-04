package free

import (
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
)

func TestReportsMaskProcessArguments(t *testing.T) {
	for _, summary := range []bool{false, true} {
		name := "report"
		if summary {
			name = "summary"
		}
		t.Run(name, func(t *testing.T) {
			r, out := newRun(t, Options{Summary: summary, HeavyMiB: 1, Only: []string{Orphans}, Orphan: 30 * time.Minute})
			r.Table = proc.NewTable(
				&proc.Process{PID: 10, PPID: 2, Comm: "worker", RSSKiB: 4096, Start: now.Add(-time.Hour),
					Args: []string{"worker", "--token", "fixture-token", "PASSWORD=fixture-password", "https://user:fixture-url@host.invalid/path"}},
				&proc.Process{PID: 20, PPID: 1, Comm: "node", RSSKiB: 4096, Start: now.Add(-time.Hour),
					Args: []string{"node", "processChild.js", "--token", "fixture-orphan"}},
			)
			r.anonKiB = func(int) int { return 2048 }
			r.Do()
			for _, value := range []string{"fixture-token", "fixture-password", "fixture-url", "fixture-orphan"} {
				if strings.Contains(out.String(), value) {
					t.Errorf("process argument %q reached %s output", value, name)
				}
			}
			if !strings.Contains(out.String(), "--token") || !strings.Contains(out.String(), "(masked)") {
				t.Errorf("masked output should retain flag names and identify omitted values:\n%s", out)
			}
		})
	}
}
