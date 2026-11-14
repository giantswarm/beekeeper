package cmd

import (
	"context"
	"fmt"
	"slices"

	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// ProbeLogins runs the probe of every open login note and returns the
// notes whose probe exits 0.
func ProbeLogins(ctx context.Context, notes []state.Note) []int {
	var passed []int
	for _, n := range notes {
		if n.Kind != noteLogin || n.Until == "" {
			continue
		}
		if probePasses(ctx, n.Until) {
			passed = append(passed, n.ID)
		}
	}
	return passed
}

// probePasses reports whether a probe, a shell command given on this
// machine, exits 0 within probeTimeout.
func probePasses(ctx context.Context, probe string) bool {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return proc.CommandContext(pctx, "sh", "-c", probe).Run() == nil //nolint:gosec // the probe its filer gave on this machine
}

// CloseProbed closes the open login notes among passed and returns their
// watch lines and events.
func CloseProbed(st *state.State, passed []int) ([]string, []state.Event) {
	var lines []string
	var evs []state.Event
	st.Notes = slices.DeleteFunc(st.Notes, func(n state.Note) bool {
		if n.Kind != noteLogin || !slices.Contains(passed, n.ID) {
			return false
		}
		lines = append(lines, fmt.Sprintf("NOTE CLOSED: #%d, its probe passed: %s", n.ID, Cmd.Truncate(n.Text, 200)))
		evs = append(evs, Cmd.Event(WatchParty, noteDone, "#%d closed, its probe passed (%s): %s", n.ID, n.Until, n.Text))
		return true
	})
	return lines, evs
}
