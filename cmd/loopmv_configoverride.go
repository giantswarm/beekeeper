package cmd

import (
	"context"

	"github.com/giantswarm/beekeeper/internal/state"
)

// settleOverrides runs in the command, which holds what it needs (loopSettleOverrides).
func (l *Loop) settleOverrides(ctx context.Context, st *state.State) {
	loopSettleOverrides(l, ctx, st)
}
