// Package systemd carries the systemd user units beekeeper ships, for
// beekeeper install to write.
package systemd

import (
	_ "embed"
	"strconv"
	"strings"
	"time"
)

// NotifyService is beekeeper-notify.service, the standby watch; its
// ExecStart names NotifyServiceExe, which install replaces with the binary.
//
//go:embed beekeeper-notify.service
var NotifyService string

// NotifyServiceExe is the binary the shipped units run.
const NotifyServiceExe = "%h/.local/bin/beekeeper"

// notifyWatch is NotifyService's watch as shipped, in its header comment
// and its ExecStart.
const notifyWatch = " watch --notify --standby"

// StandbyService is NotifyService, its watch sending desktop notifications
// only when notify (watch.notify); the header comment names the same watch
// as ExecStart.
func StandbyService(notify bool) string {
	if notify {
		return NotifyService
	}
	return strings.ReplaceAll(NotifyService, notifyWatch, " watch --standby")
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

// TeleportServiceForLoginTimeout allows both login attempts and a minute
// for status reads and cleanup, retaining the shipped fifteen-minute minimum.
// The finite outer timeout still ends a hung status command.
func TeleportServiceForLoginTimeout(loginTimeout time.Duration) string {
	// Round each attempt up to seconds before doubling. Multiplying a
	// duration directly could overflow for a large valid configuration.
	seconds := int64(loginTimeout / time.Second)
	if loginTimeout%time.Second > 0 {
		seconds++
	}
	budget := 2*seconds + 60
	if budget <= 15*60 {
		return TeleportService
	}
	return strings.Replace(TeleportService, "TimeoutStartSec=15min", "TimeoutStartSec="+strconv.FormatInt(budget, 10)+"s", 1)
}

// TeleportTimer is beekeeper-teleport.timer; install replaces
// TeleportTimerEvery with teleport.every's calendar event.
//
//go:embed beekeeper-teleport.timer
var TeleportTimer string

// TeleportTimerEvery is the timer's interval as shipped.
const TeleportTimerEvery = "OnCalendar=*:0/10"

// SupervisorLoopService runs the persistent supervisor loop: the supervisor.
//
//go:embed beekeeper-supervisor-loop.service
var SupervisorLoopService string
