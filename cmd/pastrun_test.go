package cmd

import (
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// A role's past run is never resumed or reopened: the holder and the
// successor an open relay names are not past runs, a run a taken relay
// relieved is, and a worker is not.
func TestPastRun(t *testing.T) {
	now := time.Date(2026, 10, 6, 0, 2, 0, 0, time.UTC)
	holder := state.Party{Session: "h", Name: guideRole.runName(10)}
	next := state.Party{Session: "n", Name: guideRole.runName(11)}
	old := state.Party{Session: "o", Name: guideRole.runName(9)}
	worker := state.Party{Session: "w", Name: "Board pull 192"}
	st := &state.State{Guide: &state.Role{
		Holder:   &state.Supervisor{Party: holder, Since: now.Add(-time.Hour)},
		Relay:    &state.Relay{From: holder, To: next, At: now.Add(-time.Minute), Expires: now.Add(time.Hour)},
		Relieved: []state.Relief{{Party: old, Taken: now.Add(-6 * time.Hour)}},
	}}
	for _, c := range []struct {
		p    state.Party
		want bool
	}{{holder, false}, {next, false}, {old, true}, {worker, false}} {
		if got := pastRun(st, c.p, now); got != c.want {
			t.Errorf("pastRun(%q) = %v, want %v", c.p.Name, got, c.want)
		}
	}
}
