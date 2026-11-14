package cmd

import "github.com/giantswarm/beekeeper/internal/watch"

// The data the watch and the command share lives with the watch; the command
// keeps its names for it.
type (
	checkResult      = watch.CheckResult
	diskPath         = watch.DiskPath
	freeMark         = watch.FreeMark
	overtake         = watch.Overtake
	quietness        = watch.Quietness
	relayDues        = watch.RelayDues
	reportAction     = watch.ReportAction
	restartRequest   = watch.RestartRequest
	restartWatch     = watch.RestartWatch
	role             = watch.Role
	settledPark      = watch.SettledPark
	standbyWatch     = watch.StandbyWatch
	successorChain   = watch.SuccessorChain
	supervision      = watch.Supervision
	timerFire        = watch.TimerFire
	timerWakeOutcome = watch.TimerWakeOutcome
	usageHighLedger  = watch.UsageHighLedger
	watchLevel       = watch.WatchLevel
)
