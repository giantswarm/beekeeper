package cmd

import "github.com/giantswarm/beekeeper/internal/loop"

// The data the loop and the command share lives with the loop; the command
// keeps its names for it.
type (
	answered              = loop.Answered
	deliveryFlow          = loop.DeliveryFlow
	deliveryWIP           = loop.DeliveryWIP
	diskHolding           = loop.DiskHolding
	eligibleWait          = loop.EligibleWait
	epicDelivery          = loop.EpicDelivery
	epicSnapshot          = loop.EpicSnapshot
	hostPlacementView     = loop.HostPlacementView
	throughputObservation = loop.ThroughputObservation
	unmergedPull          = loop.UnmergedPull
	watchDiskReading      = loop.WatchDiskReading
)
