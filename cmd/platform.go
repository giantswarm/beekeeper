package cmd

import (
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/platform"
)

// plat is the machine beekeeper runs on: every read of it and every unit,
// capped run, link and notification goes through its parts; loadConfig
// configures it.
var plat = platform.Current(platform.Options{DesktopApp: config.DefaultDesktopApp})
