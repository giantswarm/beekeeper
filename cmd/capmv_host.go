package cmd

import (
	"time"
)

// Command is the command package's functions capacity calls. They stay
// there until their own packages exist; a field goes when its function has
// moved to a package capacity can import.
type Command struct {
	Clock       func(now time.Time, t time.Time) string
	Dur         func(d time.Duration) string
	EffortText  func(level string) string
	Gib         func(mib int) int
	OmpProvider func(model string) string
	Truncate    func(s string, n int) string
}

// Cmd is the command's functions, set by the command at its start.
var Cmd Command
