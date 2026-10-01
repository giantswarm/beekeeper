// Package systemd carries the systemd user units beekeeper ships, for
// beekeeper install to write.
package systemd

import _ "embed"

// NotifyService is beekeeper-notify.service, the standby watch; its
// ExecStart names NotifyServiceExe, which install replaces with the binary.
//
//go:embed beekeeper-notify.service
var NotifyService string

// NotifyServiceExe is the binary the shipped units run.
const NotifyServiceExe = "%h/.local/bin/beekeeper"

// TeleportService is beekeeper-teleport.service, the Teleport login's
// keeper; its ExecStart names NotifyServiceExe.
//
//go:embed beekeeper-teleport.service
var TeleportService string

// TeleportTimer is beekeeper-teleport.timer; install replaces
// TeleportTimerEvery with teleport.every.
//
//go:embed beekeeper-teleport.timer
var TeleportTimer string

// TeleportTimerEvery is the timer's interval as shipped.
const TeleportTimerEvery = "OnUnitActiveSec=10m"
