package cmd

import (
	"time"
)

// passChangeGap is the shortest time between the end of a supervisor pass
// and a pass a state change wakes. Every session, runner and command writes
// the state; without a floor a busy desk keeps the loop passing back to back,
// each pass re-reading the state and the event log.
var passChangeGap = 5 * time.Second
