package cmd

import "github.com/giantswarm/beekeeper/internal/capacity"

// The data capacity and the command share lives with capacity; the command
// keeps its names for it.
type (
	hostPlacementView = capacity.HostPlacementView
	watchDiskReading  = capacity.WatchDiskReading
)
