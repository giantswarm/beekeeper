package cmd

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A hand-over ends the old session's CLI and a relay ends the relieved
// run's work, but the desktop warms a CLI of a session again whenever it
// shows or reopens it. Such a revived CLI carries the name its successor
// now holds: a message by that name reaches neither for sure, the watch says
// SAME TITLE, and agents resume refuses the ambiguous name. The watch stops
// it, so only the successor answers to the name. A relay's relieved run
// carries a name of its own and stays the person's (keepsRole): its CLI,
// brought back by them or the desktop, runs on.

// superseded says who carries p's name or role in its place now that its
// session's work ended: the follow-up of a hand-over (a later start, or
// another session on the roster, under its name) or the successor of a
// relay that relieved it. "" when p's session is still its own: it is on the
// roster, holds a role, or beekeeper started no session under its name.
// since is when the relief was taken; zero for a hand-over.
func superseded(st *state.State, p state.Party, now time.Time) (by string, since time.Time) {
	if slices.ContainsFunc(st.Handovers, func(h state.Handover) bool { return h.Is(p) && proc.Alive(h.PID) }) {
		return "", time.Time{}
	}
	if slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(p) }) || holdsRole(st, p) {
		return "", time.Time{}
	}
	if pastRun(st, p, now) {
		for _, rl := range roles {
			if r := relievedIn(rl.Get(st), p); r != nil && !r.Taken.IsZero() {
				return fmt.Sprintf("%q, who took the %s's role", r.By.Name, rl.Name), r.Taken
			}
		}
	}
	i := slices.IndexFunc(st.Starts, func(x state.Start) bool { return x.Session != "" && x.Session == p.Session })
	if i < 0 || !handedOver(st, st.Starts[i]) {
		return "", time.Time{}
	}
	name := st.Starts[i].Name
	if j := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == name }); j >= 0 {
		return fmt.Sprintf("%q, session %s", name, st.Agents[j].Session), time.Time{}
	}
	return fmt.Sprintf("a later start of %q", name), time.Time{}
}

// endRevived stops a revived CLI; a variable for tests.
var endRevived = endSession

// stopUnits stops the start and wake units of session id still in their
// turn and its reopen units, which would show it in the desktop and warm a
// CLI of it again: the turns' units first, as their stop-posts start reopen
// units. It returns one line per unit.
func stopUnits(ctx context.Context, id string) []string {
	var out []string
	stop := func(u string) {
		if err := plat.Launcher.Stop(ctx, u); err != nil {
			out = append(out, fmt.Sprintf("stopping %s of session %s: %v", u, id, err))
			return
		}
		out = append(out, fmt.Sprintf("stopped %s of session %s, so the desktop does not reopen it", u, id))
	}
	for _, u := range turningUnits(ctx, id) {
		stop(u)
	}
	for _, u := range reopenUnits(ctx, id) {
		stop(u)
	}
	return out
}
