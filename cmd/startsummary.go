package cmd

import (
	"fmt"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

// startSummary is what `supervisor start` prints under its line: the
// holder's first look in a few KB (leases, holds, lanes, what waits on
// it, the agents and their tasks), each part one `handover --section`
// away in full.
func (a *app) startSummary(st *state.State, sessions []*claude.Session, l *leaseList) error {
	ans, err := a.handoverAnswers(st)
	if err != nil {
		return err
	}
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(a.out, format+"\n", args...) }
	p("")
	a.promptLeases(p, l)
	a.promptHolds(p, a.activeHolds(st))
	a.promptLanes(p, a.laneViews(st))
	notes := a.splitNotes(st.Notes)
	due := 0
	for _, t := range st.Timers {
		if t.Looked(a.now) {
			due++
		}
	}
	section(p, "Waiting on you", false, "")
	p("- %d pinned notes (standing instructions), %d open notes of yours, %d the guide serves", len(notes.Pinned), len(notes.Own), notes.guided())
	p("- %d decisions answered since the last relay (%s), %d within %s", len(ans.Recent()), a.stamp(ans.Since), len(ans.All), shortDur(answeredWindow))
	p("- %d timers, %d of them due\n", len(st.Timers), due)
	section(p, "Agents", false, "")
	idle := 0
	for _, ag := range st.Agents {
		if ag.Task == "" {
			idle++
			continue
		}
		stopped := ""
		if _, live := claude.Live(sessions, ag.Party); !live {
			stopped = " (its CLI is not running)"
		}
		p("- %q on %s%s", truncate(ag.Name, 40), truncate(ag.Task, 80), stopped)
	}
	p("- %d idle\n", idle)
	p("Next: `beekeeper handover --prompt` for the whole pending state, `beekeeper handover --section <name>` for one part in full (%s).", handoverSections)
	return nil
}
