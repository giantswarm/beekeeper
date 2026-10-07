//go:build linux && !nosystemd

package platform

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/giantswarm/beekeeper/contrib/systemd"
)

// The units install writes: the standby service and the Teleport keeper.
const (
	notifyUnit          = "beekeeper-notify.service"
	sandboxUnit         = "beekeeper-sandbox.service"
	teleportServiceUnit = "beekeeper-teleport.service"
	teleportTimerUnit   = "beekeeper-teleport.timer"
)

// systemctl is the service manager's command.
const systemctl = "systemctl"

// The memory guard's shares of RAM and swap. The slice bounds every capped
// run together (two build slots of 14% each); the desktop scope's limits
// leave the rest of the machine its share when the sessions grow.
const (
	sliceHigh        = 0.23
	sliceMax         = 0.28
	desktopHigh      = 0.46
	desktopMax       = 0.56
	desktopSwapShare = 0.25
)

// memoryGuard is the desktop scope's drop-in file name.
const memoryGuard = "50-memory-guard.conf"

// systemdSetup installs systemd user units: the standby service, the
// sandbox broker, the Teleport login's keeper, the slice capped runs sit in, and the desktop
// scope's memory guard.
type systemdSetup struct{}

func (systemdSetup) Available() bool { return userSystemd() }

func (systemdSetup) Files(s SetupSpec) ([]File, []string) {
	dir := filepath.Join(s.ConfigDir, "systemd", "user")
	exe := func(unit string) []byte { return []byte(strings.ReplaceAll(unit, systemd.NotifyServiceExe, s.Exe)) }
	files := []File{
		{Path: filepath.Join(dir, notifyUnit), Content: exe(systemd.NotifyService), Service: true},
		{Path: filepath.Join(dir, sandboxUnit), Content: exe(systemd.SandboxService), Service: true},
	}
	if s.TeleportEvery > 0 {
		every := "OnUnitActiveSec=" + strconv.Itoa(int(s.TeleportEvery.Seconds())) + "s"
		files = append(files,
			File{Path: filepath.Join(dir, teleportServiceUnit), Content: exe(systemd.TeleportService)},
			File{Path: filepath.Join(dir, teleportTimerUnit), Content: []byte(strings.Replace(systemd.TeleportTimer, systemd.TeleportTimerEvery, every, 1)), Service: true})
	}
	if s.RAMMiB <= 0 {
		return files, []string{"memory guard: the machine's RAM is unreadable"}
	}
	ram := strconv.Itoa(s.RAMMiB>>10) + " GiB of RAM"
	files = append(files, File{Path: filepath.Join(dir, memcapSlice), Content: fmt.Appendf(nil, `# The slice beekeeper run puts its capped commands into, sized by beekeeper
# install for %s: above MemoryHigh the runs are throttled together, at
# MemoryMax the kernel kills the biggest one. No swap: a build that does not
# fit stops instead of pushing the desktop's pages out. CPUQuota bounds the
# cores the runs use together (memcap.cpuQuota), CPUWeight their share
# against the desktop's slices while both want the cores (memcap.cpuWeight);
# beekeeper run sets both again at every slot it takes, so the configuration
# rules without a reinstall.
[Unit]
Description=beekeeper run: memory- and CPU-capped build and test commands

[Slice]
MemoryHigh=%s
MemoryMax=%s
MemorySwapMax=0
CPUQuota=%s
CPUWeight=%d
`, ram, share(s.RAMMiB, sliceHigh), share(s.RAMMiB, sliceMax), s.CPUQuota, s.CPUWeight)})
	prefix, ok := scopePrefix(s.DesktopScope)
	if !ok {
		return files, []string{"desktop scope memory guard: no Claude Desktop scope runs; run install again while the app runs"}
	}
	return append(files, File{Path: filepath.Join(dir, prefix+".scope.d", memoryGuard), Content: fmt.Appendf(nil, `# The memory guard for the Claude desktop app's scope and every session, MCP
# server, shell and build it starts, sized by beekeeper install for %s.
# Above MemoryHigh the tree is reclaimed and throttled; at MemoryMax the
# kernel kills inside the scope, the biggest process first, instead of
# systemd-oomd killing the whole scope; OOMPolicy=continue keeps the scope
# running after such a kill.
[Scope]
MemoryHigh=%s
MemoryMax=%s
MemorySwapMax=%s
OOMPolicy=continue
`, ram, share(s.RAMMiB, desktopHigh), share(s.RAMMiB, desktopMax), share(s.SwapMiB, desktopSwapShare))}), nil
}

// scopePrefix is the drop-in prefix of a scope unit,
// app-com.anthropic.Claude-5776.scope's app-com.anthropic.Claude-: systemd
// reads its drop-ins for every scope the app starts.
func scopePrefix(unit string) (string, bool) {
	name, ok := strings.CutSuffix(unit, ".scope")
	i := strings.LastIndexByte(name, '-')
	if !ok || i < 0 {
		return "", false
	}
	return name[:i+1], true
}

// share is fraction of mib as a systemd size.
func share(mib int, fraction float64) string {
	return strconv.Itoa(int(fraction*float64(mib))) + "M"
}

func (systemdSetup) Started(ctx context.Context, path string) bool {
	unit := filepath.Base(path)
	return systemctlIs(ctx, "is-enabled", unit, "enabled") && systemctlIs(ctx, "is-active", unit, "active")
}

// systemctlIs reports whether systemctl --user verb says want of unit.
func systemctlIs(ctx context.Context, verb, unit, want string) bool {
	out, _ := exec.CommandContext(ctx, systemctl, userManager, verb, unit).Output() //nolint:gosec // a fixed verb and the unit install writes
	return strings.TrimSpace(string(out)) == want
}

func (systemdSetup) Reload() []string { return []string{systemctl, userManager, "daemon-reload"} }

func (systemdSetup) Start(path string) []string {
	return []string{systemctl, userManager, "enable", "--now", filepath.Base(path)}
}

func (systemdSetup) Stop(path string) []string {
	return []string{systemctl, userManager, "disable", "--now", filepath.Base(path)}
}
