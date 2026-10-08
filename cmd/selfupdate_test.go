package cmd

import (
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// An update names the beekeeper processes it leaves on the old binary, pid
// and command each, and says nothing when there are none.
func TestLeftBehind(t *testing.T) {
	stale := []*proc.Process{
		{PID: 3, Comm: project.Name, Args: strings.Fields("beekeeper watch --standby")},
		{PID: 5, Comm: project.Name, Args: strings.Fields("beekeeper agents reopen")},
		{PID: 7, Comm: project.Name, Args: strings.Fields("beekeeper gate -- devctl pr merge o/r 1")},
		{PID: 9, Comm: project.Name, Args: strings.Fields("beekeeper merge-child /s/merges/o_r-1")},
	}
	want := "Left on v0.107.0 until restarted, their saves of the state refused once v0.107.1 writes it:\n" +
		"  pid 3: beekeeper watch --standby\n" +
		"  pid 5: beekeeper agents reopen\n" +
		"  pid 7: beekeeper gate -- (re-executes v0.107.1 at its next step)\n" +
		"  pid 9: beekeeper merge-child /s/merges/o_r-1 (re-executes v0.107.1 at its next step)\n"
	if got := leftBehind(stale, "v0.107.0", "v0.107.1"); got != want {
		t.Errorf("left behind:\n%s\nwant:\n%s", got, want)
	}
	if got := leftBehind(nil, "v0.107.0", "v0.107.1"); got != "" {
		t.Errorf("none left behind said %q", got)
	}
}
