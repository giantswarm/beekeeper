package cmd

import "github.com/giantswarm/beekeeper/internal/gate"

// The data the gate and the command share lives with the gate; the command
// keeps its names for it.
type (
	laneTurn = gate.LaneTurn
)
