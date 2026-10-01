// Package systemd carries the systemd user units beekeeper ships, for
// beekeeper install to write.
package systemd

import _ "embed"

// NotifyService is beekeeper-notify.service, the standby watch; its
// ExecStart names NotifyServiceExe, which install replaces with the binary.
//
//go:embed beekeeper-notify.service
var NotifyService string

// NotifyServiceExe is the binary NotifyService runs as shipped.
const NotifyServiceExe = "%h/.local/bin/beekeeper"
