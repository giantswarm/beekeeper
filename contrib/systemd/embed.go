// Package systemd carries the systemd user units beekeeper ships, for
// beekeeper install to write.
package systemd

import (
	_ "embed"
	"strings"
)

// NotifyService is beekeeper-notify.service, the standby watch; its
// ExecStart names NotifyServiceExe, which install replaces with the binary.
//
//go:embed beekeeper-notify.service
var NotifyService string

// NotifyServiceExe is the binary the shipped units run.
const NotifyServiceExe = "%h/.local/bin/beekeeper"

// notifyExecStart is NotifyService's command line as shipped.
const notifyExecStart = "ExecStart=" + NotifyServiceExe + " watch --notify --standby\n"

// StandbyService is NotifyService, its watch sending desktop notifications
// only when notify (watch.notify).
func StandbyService(notify bool) string {
	if notify {
		return NotifyService
	}
	return strings.Replace(NotifyService, notifyExecStart, "ExecStart="+NotifyServiceExe+" watch --standby\n", 1)
}

// SandboxService is beekeeper-sandbox.service, the agent sandbox's
// broker; its ExecStart names NotifyServiceExe.
//
//go:embed beekeeper-sandbox.service
var SandboxService string

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
