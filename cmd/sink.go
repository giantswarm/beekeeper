package cmd

import (
	"github.com/giantswarm/beekeeper/internal/config"
)

// The verbs of the events the sink may dispatch, besides verbMerged.
const (
	verbServe        = config.SinkServe
	verbBoardClaim   = config.SinkBoardClaim
	verbPlanGrilling = config.SinkPlanGrilling
	verbPlanReady    = config.SinkPlanReady
)
