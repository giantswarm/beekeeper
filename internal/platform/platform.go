// Package platform is the one place beekeeper reaches into the machine it
// runs on: memory, pressure, processes and OOM kills (Machine), detached
// units of work (Launcher), memory-capped runs (Capper), claude:// links
// (Opener), the person's input (Input), desktop notifications (Notifier),
// and the standby service and memory guard beekeeper install puts in place
// (Setup). The build selects
// one implementation: linux_systemd on Linux (systemd user units, cgroup v2,
// /proc, D-Bus), a stub on other systems and on Linux built with the
// nosystemd tag, whose parts return a NotAvailableError; the stub installs
// the standby service as a launch agent on darwin.
package platform

import (
	"context"
	"errors"
	"os/exec"
	"time"

	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/notify"
	"github.com/giantswarm/beekeeper/internal/proc"
)

// ErrNotAvailable is what every NotAvailableError is.
var ErrNotAvailable = errors.New("not available on this platform")

// NotAvailableError is a platform part this build does not have.
type NotAvailableError struct {
	// Part names what is missing, "Launcher.Start" for instance.
	Part string
}

func (e *NotAvailableError) Error() string { return e.Part + ": not available on " + Name() }

// Is makes errors.Is(err, ErrNotAvailable) true.
func (e *NotAvailableError) Is(target error) bool { return target == ErrNotAvailable }

// Missing reports whether err is a platform part this build does not have.
func Missing(err error) bool { return errors.Is(err, ErrNotAvailable) }

// Unavailable is the one line a section prints in place of what it reads
// through a part this build does not have: "<section>: not available on
// <platform>".
func Unavailable(section string) string { return section + ": not available on " + Name() }

// Machine reads the machine: memory, pressure, processes, the scopes the
// desktop app and the capped runs sit in, and the OOM kills.
type Machine interface {
	Mem() (machine.Mem, error)
	// Load is the 1, 5 and 15 minute load average.
	Load() ([3]float64, error)
	// Forks is the machine's fork counter since boot.
	Forks() (uint64, error)
	// MemoryPressure is the share of the last minute every task stalled on
	// memory (PSI memory full avg60).
	MemoryPressure() (float64, error)
	// CPUPressure is the share of the last 10 seconds some task waited for
	// a CPU (PSI cpu some avg10).
	CPUPressure() (float64, error)
	Processes() (*proc.Table, error)
	// Started is when process pid started.
	Started(pid int) (time.Time, error)
	// DesktopScope is the largest Claude Desktop scope, nil when none runs;
	// an error when this build cannot read scopes.
	DesktopScope() (*machine.Scope, error)
	// MemcapScope is the capped run's scope unit, nil when it has ended.
	MemcapScope(unit string) *machine.Scope
	// ScopePIDs lists the processes of the scope at path (a Scope's Path).
	ScopePIDs(path string) []int
	// CgroupPIDs lists the processes of cgroup cg as a process's cgroup
	// names it.
	CgroupPIDs(cg string) []int
	// OOMPolicy is unit's OOM policy, "?" when unreadable.
	OOMPolicy(unit string) string
	// OOMDSwap is the userspace OOM killer's swap rule: the share of swap
	// past which it kills and the cgroups it watches for it.
	OOMDSwap(ctx context.Context) (machine.OOMDSwap, error)
	// SwapoffRuns reports whether a swapoff is running.
	SwapoffRuns() bool
	// OOMKills are the kernel's OOM kills since the given time, oldest first.
	OOMKills(ctx context.Context, since time.Time) ([]machine.OOMKill, error)
	// OomdKills are the userspace OOM killer's kill lines since the given time.
	OomdKills(ctx context.Context, since time.Time) ([]string, error)
	// ServiceLog is the last day of the system service unit's log, the lines
	// matching the regular expression grep.
	ServiceLog(ctx context.Context, unit, grep string) ([]byte, error)
}

// Unit is a detached, named unit of work: it outlives its starter.
type Unit struct {
	Name string
	// Dir is the working directory, empty for the launcher's own.
	Dir string
	// Env are KEY=VALUE pairs set for Argv.
	Env  []string
	Argv []string
	// KeepChildren leaves what Argv started running when Argv ends, as a
	// terminal would; otherwise a stop ends them too.
	KeepChildren bool
	// TermIsSuccess counts an end by SIGTERM as success: a stop as asked.
	TermIsSuccess bool
	// StopPost runs once Argv has ended, for up to StopTimeout. It reports
	// its own outcome: its exit or kill never fails the unit.
	StopPost    []string
	StopTimeout time.Duration
}

// Launcher starts and inspects units.
type Launcher interface {
	// Available reports whether units can be started here.
	Available() bool
	Start(u Unit) error
	// Freeze suspends every process of the unit, Thaw resumes them.
	Freeze(ctx context.Context, name string) error
	Thaw(ctx context.Context, name string) error
	// Stop stops the unit and every process in it.
	Stop(ctx context.Context, name string) error
	// State is the unit's state ("active", "inactive", "failed", ...);
	// empty when unreadable.
	State(ctx context.Context, name string) string
	// Running lists the units matching the patterns that are active or
	// starting, and with stopping those running their stop too.
	Running(ctx context.Context, stopping bool, patterns ...string) []string
	// Failed lists the failed units matching the patterns.
	Failed(ctx context.Context, patterns ...string) []string
	// ResetFailed clears the failed state of the units.
	ResetFailed(ctx context.Context, units ...string) error
}

// Cap bounds a capped run.
type Cap struct {
	// Max is the memory limit, Swap the swap limit ("12G", "0").
	Max, Swap string
	// Slice is the slice of the build slot the run holds or shares, under
	// memcap.slice: every run of the slot shares its cap. "": memcap.slice.
	Slice string
	// CPUQuota and CPUWeight are memcap.slice's CPU budget, which every
	// slot shares: its CPUQuota ("1200%", 100% a core; "": none) and its
	// CPUWeight against the desktop's slices (100 each). A zero CPUWeight
	// leaves the slice's CPU as it is.
	CPUQuota  string
	CPUWeight int
}

// Capper runs commands under a memory cap.
type Capper interface {
	// Available reports whether capped runs can be started here.
	Available() bool
	// Capped reports whether this process runs under a cap already.
	Capped() bool
	// CapSlot caps c.Slice at c before its slot holder's run starts: the
	// runs that share the slot share that cap.
	CapSlot(c Cap) error
	// Command is argv to run capped in the scope name, in c.Slice; the
	// caller starts and waits for it.
	Command(name string, c Cap, argv []string) (*exec.Cmd, error)
	// Adopt moves the running process pid into a new capped scope name, in
	// c.Slice, and returns once it is there: a command the sandbox keeps
	// from the service manager asks the host for its scope this way.
	Adopt(pid int, name string, c Cap) error
}

// Opener hands claude:// links to the desktop app.
type Opener interface {
	// Running is when the desktop app's main process started, zero when it
	// does not run.
	Running(t *proc.Table) time.Time
	// Open hands url to the running app, or starts the app on it.
	Open(ctx context.Context, url string, running bool) error
}

// Input tells when the person last typed or pointed.
type Input interface {
	// Watch watches the keyboards and pointers until ctx ends and returns
	// when input last arrived: the start of the watch until the first.
	Watch(ctx context.Context) (last func() time.Time, err error)
}

// Notifier shows desktop notifications.
type Notifier interface {
	notify.Sender
	Close() error
}

// File is one file beekeeper install writes.
type File struct {
	Path    string
	Content []byte
	// Service marks a file that defines a unit install enables and starts:
	// the standby service, the Teleport keeper's timer.
	Service bool
}

// SetupSpec is the machine beekeeper install sets up.
type SetupSpec struct {
	// Home is the user's home, ConfigDir their configuration directory
	// ($XDG_CONFIG_HOME, else ~/.config).
	Home, ConfigDir string
	// Exe is the absolute path of the binary the standby service runs.
	Exe string
	// RAMMiB and SwapMiB size the memory guard; zero RAM writes none.
	RAMMiB, SwapMiB int
	// CPUQuota and CPUWeight are memcap.slice's CPU budget
	// (memcap.cpuQuota, memcap.cpuWeight), as in a Cap.
	CPUQuota  string
	CPUWeight int
	// DesktopScope is the unit name of the running Claude Desktop scope,
	// empty when none runs.
	DesktopScope string
	// TeleportEvery is how often the Teleport login's keeper reads the
	// expiry (teleport.every); zero writes no keeper.
	TeleportEvery time.Duration
}

// Setup is what beekeeper install puts in place for this platform beside
// the hooks and the config: the standby service (beekeeper watch --notify
// --standby), the memory guard and the Teleport login's keeper, as files
// and the service manager's commands. A command is an argument vector.
type Setup interface {
	// Available reports whether this platform runs the standby service.
	Available() bool
	// Files are the standby service's, the memory guard's and the keeper's
	// files for spec, and a line for each part this machine gets none of.
	Files(spec SetupSpec) (files []File, skipped []string)
	// Started reports whether the unit defined by the file at path is
	// enabled and running.
	Started(ctx context.Context, path string) bool
	// Reload makes the service manager read changed files, nil when it
	// needs no command for that.
	Reload() []string
	// Start enables and starts the unit defined by the file at path;
	// Stop stops and disables it.
	Start(path string) []string
	Stop(path string) []string
}

// Platform is one build's implementation of every part.
type Platform struct {
	Machine  Machine
	Launcher Launcher
	Capper   Capper
	Opener   Opener
	Input    Input
	Setup    Setup
	// NewNotifier opens a notifier; the caller closes it.
	NewNotifier func() Notifier
}

// Options are the configured parts of a platform.
type Options struct {
	// DesktopApp is the Claude desktop app's executable (claude.desktopApp):
	// it starts the app, or hands a claude:// link to the running one.
	DesktopApp string
}

// Current is the platform this build runs on.
func Current(o Options) Platform { return current(o) }
