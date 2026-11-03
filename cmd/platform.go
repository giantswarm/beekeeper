package cmd

import (
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/platform"
)

// plat is the machine beekeeper runs on: every read of it and every unit,
// capped run, link and notification goes through its parts; loadConfig
// configures it.
var plat = platform.Current(platform.Options{DesktopApp: config.DefaultDesktopApp})

// testMachine is a test's machine: loadConfig rebuilds plat when a
// configuration loads, and a test that holds the machine's guards in hand
// keeps its machine across that reload. nil outside the tests that set it.
var testMachine platform.Machine

// The sections snapshot and watch read through a platform part that a build
// can lack; such a section prints one platform.Unavailable line instead.
const (
	secLoad     = "load"
	secPressure = "pressure"
	secMemory   = "memory"
	secScope    = "desktop scope"
	secSessions = "sessions"
	secOOM      = "OOM kills"
)
