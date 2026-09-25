package merge

import (
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

func TestStalled(t *testing.T) {
	const repo, ttl, after = "giantswarm/agent-platform", 15 * time.Minute, 5 * time.Minute
	now := time.Date(2026, 9, 25, 1, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	const live = 100 // the pid whose gate call is in the gate
	alive := func(pid int) bool { return pid == live }
	present := func(m state.Merge) bool { return Present(m, now, ttl, alive) }
	arrival := ago(20 * time.Minute)
	arrived := func(pid int) (time.Time, bool) { return arrival, pid == live }
	seed := func(pr int, by string, joined time.Duration) state.Merge {
		return state.Merge{Repo: repo, PR: pr, Lane: "ap", By: state.Party{Name: by}, Phase: state.Waiting, Seeded: true, Joined: ago(joined), Seen: ago(joined)}
	}
	in := func(m state.Merge) state.Merge { m.PID, m.Seen = live, now; return m }
	left := func(m state.Merge, d time.Duration) state.Merge { m.PID, m.Seen = 7, ago(d); return m }
	outside := func(m state.Merge) state.Merge { m.Outside = true; return m }
	for _, c := range []struct {
		name    string
		merges  []state.Merge
		arrival time.Duration
		want    int // the PR of the waiting merge, 0 for no stall
		since   time.Time
	}{
		{"arrived seed behind an absent seed", []state.Merge{seed(676, "GPU", time.Hour), in(seed(671, "five", time.Hour))}, 20 * time.Minute, 671, arrival},
		{"not yet stallAfter", []state.Merge{seed(676, "GPU", time.Hour), in(seed(671, "five", time.Hour))}, 3 * time.Minute, 0, time.Time{}},
		{"an arrived unseeded merge passes absent seeds", []state.Merge{seed(676, "GPU", time.Hour), in(state.Merge{Repo: repo, PR: 684, Lane: "ap", Phase: state.Waiting, Joined: ago(time.Minute)})}, 20 * time.Minute, 0, time.Time{}},
		{"behind a merge that left the gate", []state.Merge{left(seed(678, "GPU", time.Hour), 8*time.Minute), in(seed(671, "five", time.Hour))}, 20 * time.Minute, 671, ago(8 * time.Minute)},
		{"behind a merge that just left the gate", []state.Merge{left(seed(678, "GPU", time.Hour), 2*time.Minute), in(seed(671, "five", time.Hour))}, 20 * time.Minute, 0, time.Time{}},
		{"behind a merge settled outside the gate", []state.Merge{outside(seed(172, "GPU", time.Hour)), in(seed(671, "five", time.Hour))}, 20 * time.Minute, 0, time.Time{}},
		{"a merge runs", []state.Merge{{Repo: repo, PR: 1, Lane: "ap", Phase: state.Running, PID: live}, seed(676, "GPU", time.Hour), in(seed(671, "five", time.Hour))}, 20 * time.Minute, 0, time.Time{}},
		{"the lane's last merge just ended", []state.Merge{{Repo: repo, PR: 1, Lane: "ap", Phase: state.Settling, Finished: ago(time.Minute)}, seed(676, "GPU", time.Hour), in(seed(671, "five", time.Hour))}, 20 * time.Minute, 0, time.Time{}},
		{"nobody in the gate", []state.Merge{seed(676, "GPU", time.Hour), seed(671, "five", time.Hour)}, 20 * time.Minute, 0, time.Time{}},
	} {
		arrival = ago(c.arrival)
		if c.since.IsZero() && c.want != 0 {
			c.since = arrival
		}
		s, ok := Queue(&state.State{Merges: c.merges}, "ap").Stalled(now, after, present, alive, arrived)
		switch {
		case ok != (c.want != 0):
			t.Errorf("%s: stalled %v, want %v (%+v)", c.name, ok, c.want != 0, s)
		case ok && (s.Merge.PR != c.want || !s.Since.Equal(c.since) || len(s.Behind) != 1):
			t.Errorf("%s: %s since %s behind %d, want #%d since %s", c.name, s.Merge.Key(), s.Since, len(s.Behind), c.want, c.since)
		}
	}
}
