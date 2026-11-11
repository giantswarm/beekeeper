package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"

	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/pkg/project"
)

const supervisorCommand = "supervisor"

// supervisorGroup is the command group of the supervisor's commands.
const supervisorGroup = "supervising"

// loopVersionFile is the running loop's version marker, beside the state.
const loopVersionFile = "supervisor-loop-version.json"

type loopVersionRecord struct {
	PID     int    `json:"pid"`
	Version string `json:"version"`
}

// loopRunningVersion returns the binary version of the current process.
var loopRunningVersion = project.Version

// installedLoopBinary returns the path of the installed beekeeper binary.
var installedLoopBinary = os.Executable

// loopVersionOf returns the binary version of the executable at path.
var loopVersionOf = func(ctx context.Context, path string) string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output() //nolint:gosec // the running or installed beekeeper binary
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	if len(fields) >= 2 && fields[0] == project.Name {
		return normalizeLoopVersion(fields[1])
	}
	if len(fields) > 0 {
		return normalizeLoopVersion(fields[0])
	}
	return ""
}

func normalizeLoopVersion(ver string) string {
	ver = strings.TrimSpace(ver)
	if ver == "" {
		return ""
	}
	if !strings.HasPrefix(ver, "v") && ver[0] >= '0' && ver[0] <= '9' {
		return "v" + ver
	}
	return ver
}

func (a *app) writeLoopVersion() error {
	return a.store.WriteFile(loopVersionFile, loopVersionRecord{
		PID:     os.Getpid(),
		Version: normalizeLoopVersion(loopRunningVersion()),
	})
}

func (a *app) clearLoopVersion() {
	_ = a.store.WriteFile(loopVersionFile, loopVersionRecord{})
}

func (a *app) supervisorCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   supervisorCommand,
		Short: "Show the supervisor loop, or run it",
		Long: `The supervisor is the supervisor loop (` + "`beekeeper supervisor loop`" + `), which
the beekeeper-supervisor-loop user service runs: beekeeper install always
installs it. No session holds the supervisor's role and nothing starts or
relays one: while the loop is down the standby watch restarts its systemd
service (Linux; on macOS launchd restarts a failed loop itself), and says
SUPERVISOR LOOP DOWN and notifies until it runs again.

Without a subcommand, shows whether the loop runs. A stopped loop is a valid status.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.supervisorStatus(cmd.Context()) },
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Show whether the supervisor loop runs",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return a.supervisorStatus(cmd.Context()) },
	}
	c.AddCommand(status, a.supervisorLoopCmd())
	return c
}

// loopPID is the PID of the running supervisor loop, 0 when none runs.
func (a *app) loopPID() int {
	var pid int
	found, err := a.store.ReadFile(loopPIDFile, &pid)
	if err != nil || !found || pid <= 0 || !proc.Alive(pid) {
		return 0
	}
	return pid
}

// loopServiceHint is the command that shows the loop's user service on
// this platform.
func loopServiceHint() string {
	if runtime.GOOS == "darwin" {
		return "`launchctl list " + loopLaunchdLabel + "`"
	}
	return "`systemctl --user status " + loopUnit + "`"
}

// loopLaunchdLabel is the loop's launchd agent on macOS.
const loopLaunchdLabel = "com.giantswarm.beekeeper.supervisor-loop"

// supervisorView is the supervisor's status for --json.
type supervisorView struct {
	Hosts    []hostPlacementView `json:"hosts,omitempty"`
	Epics    string              `json:"epics"`
	Delivery *deliveryView       `json:"delivery,omitempty"`
	Loop     bool                `json:"loop"`
	PID      int                 `json:"pid,omitempty"`
	Version  string              `json:"version,omitempty"`
	// Held says what the loop holds of its GitHub reads, Pulls the open
	// pull requests nobody owns: both for whoever waits on the loop.
	Held  string `json:"held,omitempty"`
	Pulls string `json:"pulls,omitempty"`
}

// supervisorStatus reports the loop state; a stopped loop is a valid status.
func (a *app) supervisorStatus(ctx ...context.Context) error {
	c := context.Background()
	if len(ctx) > 0 && ctx[0] != nil {
		c = ctx[0]
	}
	var pid int
	if _, err := a.store.ReadFile(loopPIDFile, &pid); err != nil {
		return fmt.Errorf("reading supervisor loop PID: %w", err)
	}
	if pid <= 0 || !proc.Alive(pid) {
		pid = 0
	}
	var ver string
	if pid > 0 {
		ver = a.loopVersion(c, pid)
	}
	held, pulls := a.loopWaitLines()
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	delivery := readDelivery(st, a.cfg.Delivery.WIPLimit, a.timeNow(), a.deliveryPolicy())
	epics, err := a.epicStatus(st)
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(supervisorView{Hosts: a.hostPlacementViews(st), Epics: epics, Delivery: &delivery, Loop: pid > 0, PID: pid, Version: ver, Held: held, Pulls: pulls})
	}
	for _, host := range a.hostPlacementViews(st) {
		if _, err := fmt.Fprintln(a.out, host.line()); err != nil {
			return err
		}
	}
	if pid == 0 {
		_, err := fmt.Fprintf(a.out, "the supervisor loop does not run (%s)\n%s\n%s\n", loopServiceHint(), delivery.line(), epics)
		return err
	}
	line := fmt.Sprintf("the supervisor loop runs (PID %d)", pid)
	if ver != "" {
		line = fmt.Sprintf("the supervisor loop runs (PID %d, %s)", pid, ver)
	}
	for _, l := range []string{line, held, pulls, delivery.line(), epics} {
		if l == "" {
			continue
		}
		if _, err := fmt.Fprintln(a.out, l); err != nil {
			return err
		}
	}
	return nil
}

// loopWaitLines are what the loop holds of its GitHub reads and how many
// open pull requests have no owner, "" for each with nothing to say. Unread
// side files say nothing: the status of the loop itself stands.
func (a *app) loopWaitLines() (held, pulls string) {
	if a.store == nil {
		return "", ""
	}
	var spend loopGraphQLSpend
	if found, err := a.store.ReadFile(loopGraphQLFile, &spend); err == nil && found {
		held = a.heldLine(spend)
	}
	var count loopSweepCount
	if found, err := a.store.ReadFile(loopSweepFile, &count); err == nil && found {
		pulls = count.line(a)
	}
	return held, pulls
}

// loopVersion returns the running supervisor loop's binary version.
func (a *app) loopVersion(ctx context.Context, pid int) string {
	if pid <= 0 {
		return ""
	}
	var rec loopVersionRecord
	if found, err := a.store.ReadFile(loopVersionFile, &rec); err == nil && found && rec.PID == pid && rec.Version != "" {
		return normalizeLoopVersion(rec.Version)
	}
	if pid == os.Getpid() {
		return normalizeLoopVersion(loopRunningVersion())
	}
	for _, path := range []string{
		fmt.Sprintf("/proc/%d/exe", pid),
		loopProcessPath(pid),
	} {
		if path == "" {
			continue
		}
		if ver := loopVersionOf(ctx, path); ver != "" {
			return ver
		}
	}
	if path, replaced := processBinary(pid); path != "" && !replaced {
		if installed, err := installedLoopBinary(); err == nil && sameBinaryFile(path, installed) {
			return normalizeLoopVersion(loopRunningVersion())
		}
	}
	return ""
}

func loopProcessPath(pid int) string {
	if b := platform.ProcessBinary(pid); b != nil && !b.Replaced() {
		return b.Path
	}
	return ""
}

func sameBinaryFile(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if resolvedPath(a) == resolvedPath(b) {
		return true
	}
	fa, err := os.Stat(a) // #nosec G703 -- running or installed beekeeper binary path
	if err != nil {
		return false
	}
	fb, err := os.Stat(b) // #nosec G703 -- running or installed beekeeper binary path
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

// staleLoopLine reports when the running supervisor loop runs an older binary
// than the installed one.
func (a *app) staleLoopLine(ctx context.Context) string {
	pid := a.loopPID()
	if pid <= 0 {
		return ""
	}
	installedVer := normalizeLoopVersion(loopRunningVersion())
	if installed, err := installedLoopBinary(); err == nil && installed != "" {
		_, selfReplaced := replacedBinary(os.Getpid())
		if self, selfErr := os.Executable(); selfErr != nil || !sameBinaryFile(installed, self) || selfReplaced {
			if v := loopVersionOf(ctx, installed); v != "" {
				installedVer = v
			}
		}
	}
	loopVer := a.loopVersion(ctx, pid)
	if loopVer != "" && installedVer != "" && loopVer != installedVer {
		if semver.IsValid(loopVer) && semver.IsValid(installedVer) && semver.Compare(loopVer, installedVer) < 0 {
			return fmt.Sprintf("stale supervisor loop: pid %d runs %s while %s is installed; it re-executes the installed binary at its next quiet point, or restart %s", pid, loopVer, installedVer, loopUnit)
		}
	}
	if _, replaced := replacedBinary(pid); replaced {
		if loopVer != "" && installedVer != "" {
			return fmt.Sprintf("stale supervisor loop: pid %d runs %s from a binary an install replaced (%s installed); it re-executes the installed binary at its next quiet point, or restart %s", pid, loopVer, installedVer, loopUnit)
		}
		return fmt.Sprintf("stale supervisor loop: pid %d runs a binary an install replaced (%s installed); it re-executes the installed binary at its next quiet point, or restart %s", pid, installedVer, loopUnit)
	}
	if path, _ := processBinary(pid); path != "" {
		if installed, err := installedLoopBinary(); err == nil && installed != "" && !sameBinaryFile(path, installed) {
			if loopVer != "" && installedVer != "" && loopVer != installedVer {
				return fmt.Sprintf("stale supervisor loop: pid %d runs %s while %s is installed; it re-executes the installed binary at its next quiet point, or restart %s", pid, loopVer, installedVer, loopUnit)
			}
			return fmt.Sprintf("stale supervisor loop: pid %d runs an older binary than the installed %s; it re-executes the installed binary at its next quiet point, or restart %s", pid, installedVer, loopUnit)
		}
	}
	return ""
}
