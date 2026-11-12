package cmd

import (
	"context"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// roster runs in the command, which holds what it needs (loopRoster).
func (l *supervisorLoop) roster(ctx context.Context, planned map[string]bool) error {
	return loopRoster(l, ctx, planned)
}

// filedIssues runs in the command, which holds what it needs (loopFiledIssues).
func (l *supervisorLoop) filedIssues(st *state.State) []superloop.FiledIssue {
	return loopFiledIssues(l, st)
}

// fireTimers runs in the command, which holds what it needs (loopFireTimers).
func (l *supervisorLoop) fireTimers(ctx context.Context, st *state.State) error {
	return loopFireTimers(l, ctx, st)
}
