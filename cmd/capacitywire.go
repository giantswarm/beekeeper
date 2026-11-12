package cmd

import (
	"github.com/giantswarm/beekeeper/internal/capacity"
)

// Capacity calls the command through Cmd.
func init() {
	capacity.Cmd = capacity.Command{
		Clock:       clock,
		Dur:         dur,
		EffortText:  effortText,
		Gib:         gib,
		OmpProvider: ompProvider,
		Truncate:    truncate,
	}
}
