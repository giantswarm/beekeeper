package loop

import (
	"context"
	"time"

	"github.com/giantswarm/beekeeper/internal/board"
)

const (
	BoardProgressFile   = "supervisor-board-progress.json"
	BoardProgressWindow = 30 * time.Minute
	BoardProgressFlag   = "progress-item"
)

// BoardProgress survives loop restarts. Only successfully observed top items
// renew Seen; unread observations cannot authorize a pace exception.
type BoardProgress struct {
	BoardAt time.Time               `json:"boardAt"`
	Items   []board.Candidate       `json:"items"`
	Entries map[string]ItemProgress `json:"entries"`
}

type ItemProgress struct {
	Signal   string    `json:"signal"`
	Changed  time.Time `json:"changed"`
	Seen     time.Time `json:"seen"`
	Attempt  time.Time `json:"attempt,omitzero"`
	Reported string    `json:"reported,omitempty"`
}

func (p ItemProgress) Stalled(now time.Time) bool {
	return !p.Changed.IsZero() && now.Sub(p.Changed) >= BoardProgressWindow &&
		!p.Seen.After(now) && now.Sub(p.Seen) <= 2*time.Minute
}

type progressContextKey struct{}

// WithProgressItem scopes the exception to commands dispatched for this item,
// never to the parallel refill or a provider's global pace setting.
func WithProgressItem(ctx context.Context, ref string) context.Context {
	return context.WithValue(ctx, progressContextKey{}, ref)
}

func progressArgs(ctx context.Context, args []string) []string {
	ref, _ := ctx.Value(progressContextKey{}).(string)
	if ref == "" {
		return args
	}
	for i, arg := range args {
		if arg == LoopCapacity || arg == agentsName && i+1 < len(args) && args[i+1] == agentStartName {
			return append(args, "--"+BoardProgressFlag, ref)
		}
	}
	return args
}
