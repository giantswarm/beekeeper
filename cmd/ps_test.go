package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
)

// ps keeps PID, parent, age and the program readable and leaves out every
// value a command line may carry; a filter matches only the masked line.
func TestPsRowsMaskCommandLines(t *testing.T) {
	now := time.Now()
	tb := table(
		&proc.Process{PID: 10, PPID: 1, Comm: "docker", Start: now.Add(-2 * time.Hour), RSSKiB: 4096, //nolint:gosec // made-up values ps must drop
			Args: strings.Fields("/usr/bin/docker run -e PASSWORD=x1 --token x2 -p x3 image")},
		&proc.Process{PID: 11, PPID: 10, Comm: "electron", Start: now.Add(-time.Minute),
			Args: []string{"electron --api-key=x4 --type=zygote"}},
		&proc.Process{PID: 2, PPID: 0, Comm: "kthreadd", Start: now.Add(-time.Hour)},
	)
	rows := psRows(tb, now, nil)
	if len(rows) != 3 || rows[0].PID != 2 || rows[1].PID != 10 || rows[2].PID != 11 {
		t.Fatalf("rows = %+v", rows)
	}
	for _, r := range rows {
		for _, v := range []string{"x1", "x2", "x3", "x4", "PASSWORD", "zygote"} {
			if strings.Contains(r.Command, v) {
				t.Errorf("row %d shows %q: %q", r.PID, v, r.Command)
			}
		}
	}
	if r := rows[1]; r.Command != "docker run -e --token -p" || !r.Masked || r.PPID != 1 || r.Age != "2h00m" || r.RSSMiB != 4 {
		t.Errorf("docker row = %+v", r)
	}
	if r := rows[0]; r.Command != "[kthreadd]" || r.Masked {
		t.Errorf("kernel thread row = %+v", r)
	}
	if got := psRows(tb, now, []string{"x1"}); len(got) != 0 {
		t.Errorf("a filter on a left-out value matched %+v", got)
	}
	if got := psRows(tb, now, []string{"11", "kthreadd"}); len(got) != 2 || got[0].PID != 2 || got[1].PID != 11 {
		t.Errorf("filters by PID and name = %+v", got)
	}
}
