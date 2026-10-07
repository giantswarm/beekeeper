//go:build linux && !nosystemd

package platform

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/proc"
)

// Name is the platform this build runs on.
func Name() string { return "linux_systemd" }

// current is linux_systemd: /proc, PSI and cgroup v2 for the machine,
// systemd user units and scopes for the launcher and the capper, the
// journal for the OOM kills, evdev for the person's input, D-Bus for
// notifications.
func current(o Options) Platform {
	return Platform{
		Machine:     systemdMachine{},
		Launcher:    systemdLauncher{},
		Capper:      systemdCapper{},
		Opener:      systemdOpener{app: o.DesktopApp},
		Input:       evdevInput{},
		Setup:       systemdSetup{},
		NewNotifier: func() Notifier { return &desktop{} },
	}
}

// userManager points systemctl and systemd-run at the user's service manager.
const userManager = "--user"

// cgroupRoot is where cgroup v2 is mounted.
const cgroupRoot = "/sys/fs/cgroup"

type systemdMachine struct{}

func (systemdMachine) Mem() (machine.Mem, error)          { return machine.ReadMem() }
func (systemdMachine) Load() ([3]float64, error)          { return machine.ReadLoad() }
func (systemdMachine) Forks() (uint64, error)             { return machine.ReadForks() }
func (systemdMachine) MemoryPressure() (float64, error)   { return machine.ReadPSIFull60() }
func (systemdMachine) CPUPressure() (float64, error)      { return machine.ReadCPUPSISome10() }
func (systemdMachine) Processes() (*proc.Table, error)    { return proc.Read() }
func (systemdMachine) Started(pid int) (time.Time, error) { return proc.Started(pid) }

func (systemdMachine) OOMDSwap(ctx context.Context) (machine.OOMDSwap, error) {
	return machine.ReadOOMDSwap(ctx)
}

func (systemdMachine) DesktopScope() (*machine.Scope, error) {
	if p := machine.FindScope(); p != "" {
		return machine.ReadScope(p), nil
	}
	return nil, nil
}

func (systemdMachine) MemcapScope(unit string) *machine.Scope {
	if p := machine.FindMemcapScope(unit); p != "" {
		return machine.ReadScope(p)
	}
	return nil
}

func (systemdMachine) ScopePIDs(path string) []int { return pidsIn(path) }

func (systemdMachine) CgroupPIDs(cg string) []int { return pidsIn(filepath.Join(cgroupRoot, cg)) }

// pidsIn lists the processes of the cgroup v2 directory dir.
func pidsIn(dir string) []int {
	raw, err := os.ReadFile(filepath.Clean(filepath.Join(dir, "cgroup.procs")))
	if err != nil {
		return nil
	}
	var out []int
	for _, f := range strings.Fields(string(raw)) {
		if pid, err := strconv.Atoi(f); err == nil {
			out = append(out, pid)
		}
	}
	return out
}

func (systemdMachine) OOMPolicy(unit string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", userManager, "show", unit, "-p", "OOMPolicy", "--value").Output() // #nosec G204 -- the unit is the desktop scope's cgroup name, one argument
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(string(out))
}

func (systemdMachine) OOMKills(ctx context.Context, since time.Time) ([]machine.OOMKill, error) {
	out, err := exec.CommandContext(ctx, "journalctl", "-k", "--no-pager", "-o", "short-iso", //nolint:gosec // fixed arguments and a formatted time
		"--since", since.Local().Format("2006-01-02 15:04:05")).Output()
	if err != nil {
		return nil, err
	}
	return machine.ParseOOM(string(out)), nil
}

func (systemdMachine) OomdKills(ctx context.Context, since time.Time) ([]string, error) {
	out, err := exec.CommandContext(ctx, "journalctl", "-u", "systemd-oomd", "--no-pager", "-o", "short-iso", //nolint:gosec // fixed arguments and a formatted time
		"--since", since.Local().Format("2006-01-02 15:04:05")).Output()
	if err != nil {
		return nil, err
	}
	return machine.ParseOomd(string(out)), nil
}

func (systemdMachine) ServiceLog(ctx context.Context, unit, grep string) ([]byte, error) {
	return exec.CommandContext(ctx, "journalctl", "-u", unit, "--no-pager", "-o", "cat", "--since", "-24h", "-g", grep).Output() //nolint:gosec // the configured unit name
}

// systemdLauncher runs units as transient systemd user services: they get
// the user manager's environment, not the caller's session variables, and
// outlive the caller.
type systemdLauncher struct{}

func (systemdLauncher) Available() bool { return userSystemd() }

func (systemdLauncher) Start(u Unit) error {
	out, err := exec.Command("systemd-run", runArgs(u)...).CombinedOutput() //nolint:gosec // starting the unit is the purpose
	if err != nil {
		return fmt.Errorf("systemd-run: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// runArgs is systemd-run's command line for u.
func runArgs(u Unit) []string {
	args := []string{userManager, "--collect", "--quiet", "--unit=" + u.Name}
	if u.KeepChildren {
		args = append(args, "-p", "KillMode=process")
	} else {
		args = append(args, "-p", "KillMode=mixed")
	}
	if u.TermIsSuccess {
		args = append(args, "-p", "SuccessExitStatus=143 SIGTERM")
	}
	if u.Dir != "" {
		args = append(args, "--working-directory="+u.Dir)
	}
	if len(u.StopPost) > 0 {
		// "-": the stop-post's own end, a kill included, never fails the unit.
		args = append(args, "-p", "ExecStopPost=-"+strings.Join(u.StopPost, " "))
	}
	if u.StopTimeout > 0 {
		args = append(args, "-p", "TimeoutStopSec="+seconds(u.StopTimeout))
	}
	if u.MaxRuntime > 0 {
		args = append(args, "-p", "RuntimeMaxSec="+seconds(u.MaxRuntime))
	}
	for _, e := range u.Env {
		args = append(args, "--setenv="+e)
	}
	return append(append(args, "--"), u.Argv...)
}

// seconds is d as a service manager's time value in whole seconds.
func seconds(d time.Duration) string { return strconv.Itoa(int(d.Seconds())) }

func (systemdLauncher) Freeze(ctx context.Context, name string) error {
	return systemctlUser(ctx, "freeze", name)
}

func (systemdLauncher) Thaw(ctx context.Context, name string) error {
	return systemctlUser(ctx, "thaw", name)
}

func (systemdLauncher) Stop(ctx context.Context, name string) error {
	return systemctlUser(ctx, "stop", name)
}

// systemctlUser runs one systemctl --user verb on the units.
func systemctlUser(ctx context.Context, verb string, units ...string) error {
	out, err := exec.CommandContext(ctx, "systemctl", append([]string{userManager, verb}, units...)...).CombinedOutput() //nolint:gosec // the units beekeeper named
	if err != nil {
		return fmt.Errorf("systemctl --user %s: %w: %s", verb, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (systemdLauncher) State(ctx context.Context, name string) string {
	out, _ := exec.CommandContext(ctx, "systemctl", userManager, "show", "-p", "ActiveState", "--value", name).Output() //nolint:gosec // the unit beekeeper named
	return strings.TrimSpace(string(out))
}

func (systemdLauncher) Running(ctx context.Context, stopping bool, patterns ...string) []string {
	states := "--state=active,activating"
	if stopping {
		states += ",deactivating"
	}
	return listUnits(ctx, states, patterns)
}

func (systemdLauncher) Failed(ctx context.Context, patterns ...string) []string {
	return listUnits(ctx, "--state=failed", patterns)
}

func (systemdLauncher) ResetFailed(ctx context.Context, units ...string) error {
	if len(units) == 0 {
		return nil
	}
	return systemctlUser(ctx, "reset-failed", units...)
}

// listUnits lists the units in states matching the patterns.
func listUnits(ctx context.Context, states string, patterns []string) []string {
	args := append([]string{userManager, "list-units", "--all", "--plain", "--no-legend", states}, patterns...)
	out, _ := exec.CommandContext(ctx, "systemctl", args...).Output() //nolint:gosec // the units beekeeper named
	var units []string
	for line := range strings.Lines(string(out)) {
		if f := strings.Fields(line); len(f) > 0 {
			units = append(units, f[0])
		}
	}
	return units
}

// userSystemd reports whether a user service manager can start units and
// scopes. "degraded" (one failed unit somewhere) is a desktop's normal state.
func userSystemd() bool {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return false
	}
	out, _ := exec.Command("systemctl", userManager, "is-system-running").Output()
	switch strings.TrimSpace(string(out)) {
	case "running", "degraded", "starting", "maintenance":
		return true
	}
	return false
}

// memcapSlice is the slice capped runs' scopes sit in.
const memcapSlice = "memcap.slice"

// systemdCapper runs commands in transient scopes under memcap.slice.
type systemdCapper struct{}

func (systemdCapper) Available() bool { return userSystemd() }

// Capped: a Makefile or script that runs a capped command again runs inside
// the outer scope, which already holds the slot and the cap.
func (systemdCapper) Capped() bool {
	raw, err := os.ReadFile("/proc/self/cgroup")
	return err == nil && strings.Contains(string(raw), "/"+memcapSlice+"/")
}

// slice is the cap's slice, memcap.slice itself for none.
func (c Cap) slice() string {
	if c.Slice == "" {
		return memcapSlice
	}
	return c.Slice
}

// CapSlot sets the slot slice's limits for this boot, and memcap.slice's
// CPU budget, which every slot shares.
func (systemdCapper) CapSlot(c Cap) error {
	for _, p := range sliceProperties(c) {
		out, err := exec.Command("systemctl", append([]string{userManager, "set-property", "--runtime"}, p...)...).CombinedOutput() //nolint:gosec // our own sizes
		if err != nil {
			return fmt.Errorf("capping %s: %v: %s", p[0], err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// sliceProperties are a slot cap's set-property calls, each a unit and its
// assignments: memcap.slice's CPU budget (with a CPUWeight; an empty
// CPUQuota lifts the quota), then the slot slice's memory.
func sliceProperties(c Cap) [][]string {
	var out [][]string
	if c.CPUWeight > 0 {
		out = append(out, []string{memcapSlice, "CPUQuota=" + c.CPUQuota, "CPUWeight=" + strconv.Itoa(c.CPUWeight)})
	}
	return append(out, []string{c.slice(), "MemoryMax=" + c.Max, "MemorySwapMax=" + c.Swap})
}

// Command is argv in a transient scope in the slot's slice. systemd-run's
// own ${VAR} expansion is off (default-on for --scope since systemd 258):
// the argument list reaches the command verbatim, so a wrapped `zsh -c`
// keeps ${=files}, ${(f)x}, ${pipestatus[1]} and $$.
func (systemdCapper) Command(name string, c Cap, argv []string) (*exec.Cmd, error) {
	return exec.Command("systemd-run", scopeArgs(name, c, argv)...), nil //nolint:gosec // running the caller's command is the purpose
}

// scopeArgs is systemd-run's argument list for the scope name around argv:
// the slot's slice, the memory cap, and the command at RunNice, which
// systemd-run applies itself in --scope mode.
func scopeArgs(name string, c Cap, argv []string) []string {
	return append([]string{userManager, "--scope", "--quiet", "--expand-environment=no", "--unit=" + name,
		"--slice=" + c.slice(), "--nice=" + strconv.Itoa(RunNice), "-p", "MemoryMax=" + c.Max, "-p", "MemorySwapMax=" + c.Swap,
		"-p", "OOMPolicy=continue", "--"}, argv...)
}

// Adopt starts the transient scope name around the running process pid
// (the service manager's StartTransientUnit with PIDs, which systemd-run
// --scope uses for itself) and waits until the process is in it.
func (systemdCapper) Adopt(pid int, name string, c Cap) error {
	limit, err := sizeBytes(c.Max)
	if err != nil {
		return err
	}
	swap, err := sizeBytes(c.Swap)
	if err != nil {
		return err
	}
	conn, err := connect()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	type property struct {
		Name  string
		Value dbus.Variant
	}
	props := []property{
		{"PIDs", dbus.MakeVariant([]uint32{uint32(pid)})}, //nolint:gosec // a process id
		{"Slice", dbus.MakeVariant(c.slice())},
		{"MemoryMax", dbus.MakeVariant(limit)},
		{"MemorySwapMax", dbus.MakeVariant(swap)},
		{"OOMPolicy", dbus.MakeVariant("continue")},
	}
	var job dbus.ObjectPath
	err = conn.Object("org.freedesktop.systemd1", "/org/freedesktop/systemd1").
		Call("org.freedesktop.systemd1.Manager.StartTransientUnit", 0, name+".scope", "fail", props, []struct {
			Name  string
			Props []property
		}{}).Store(&job)
	if err != nil {
		return fmt.Errorf("starting %s.scope: %w", name, err)
	}
	cgroup := filepath.Join("/proc", strconv.Itoa(pid), "cgroup")
	for range 250 {
		if raw, err := os.ReadFile(cgroup); err != nil { //nolint:gosec // the adopted process's cgroup
			return fmt.Errorf("process %d: %w", pid, err)
		} else if strings.Contains(string(raw), "/"+name+".scope") {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("process %d is not in %s.scope after 5s", pid, name)
}

// sizeBytes is a systemd size with base-1024 suffixes, or "infinity", in
// bytes, as the service manager's memory properties take it.
func sizeBytes(s string) (uint64, error) {
	if s == "infinity" {
		return math.MaxUint64, nil
	}
	shift := 0
	switch {
	case strings.HasSuffix(s, "K"), strings.HasSuffix(s, "k"):
		shift = 10
	case strings.HasSuffix(s, "M"), strings.HasSuffix(s, "m"):
		shift = 20
	case strings.HasSuffix(s, "G"), strings.HasSuffix(s, "g"):
		shift = 30
	case strings.HasSuffix(s, "T"), strings.HasSuffix(s, "t"):
		shift = 40
	}
	digits := s
	if shift > 0 {
		digits = s[:len(s)-1]
	}
	n, err := strconv.ParseUint(digits, 10, 64)
	if err != nil || n > math.MaxUint64>>shift {
		return 0, fmt.Errorf("invalid size %q: want a number with an optional K, M, G or T suffix, or infinity", s)
	}
	return n << shift, nil
}

// systemdOpener hands links to the desktop app, starting it in a scope of
// its own under app.slice when it does not run.
type systemdOpener struct{ app string }

// Running: Electron rewrites its command line into one string, so the
// arguments are its fields; the helpers carry --type=.
func (o systemdOpener) Running(t *proc.Table) time.Time {
	for _, p := range t.ByPID {
		args := strings.Fields(p.Cmdline())
		if len(args) > 0 && filepath.Base(args[0]) == o.app &&
			!slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--type=") }) {
			return p.Start
		}
	}
	return time.Time{}
}

// Open starts the app where the desktop starts it too, so it outlives the
// unit or watch that started it.
func (o systemdOpener) Open(ctx context.Context, url string, running bool) error {
	if running {
		return exec.CommandContext(ctx, o.app, url).Run() //nolint:gosec // a claude:// link built from the state's local_ id
	}
	c := exec.Command("systemd-run", userManager, "--scope", "--quiet", "--slice=app.slice", //nolint:gosec // as above
		"--unit=app-com.anthropic.Claude-beekeeper-"+strconv.FormatInt(time.Now().Unix(), 10), o.app, url)
	if err := c.Start(); err != nil {
		return err
	}
	return c.Process.Release()
}
