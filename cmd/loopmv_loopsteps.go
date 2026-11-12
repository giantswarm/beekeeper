package cmd

import (
	"context"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// Roster runs in the command, which holds what it needs (loopRoster).
func (l *Loop) Roster(ctx context.Context, planned map[string]bool) error {
	return Cmd.LoopRoster(l, ctx, planned)
}

// filedIssues runs in the command, which holds what it needs (loopFiledIssues).
func (l *Loop) filedIssues(st *state.State) []superloop.FiledIssue {
	return Cmd.LoopFiledIssues(l, st)
}

// fireTimers runs in the command, which holds what it needs (loopFireTimers).
func (l *Loop) fireTimers(ctx context.Context, st *state.State) error {
	return Cmd.LoopFireTimers(l, ctx, st)
}
