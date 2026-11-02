package cmd

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/proc"
)

// supervisorGroup is the command group of the supervisor's commands.
const supervisorGroup = "supervising"

func (a *app) supervisorCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "supervisor",
		Short: "Show the supervisor loop, or run it",
		Long: `The supervisor is the supervisor loop (` + "`beekeeper supervisor loop`" + `), which
the beekeeper-supervisor-loop user service runs: beekeeper install always
installs it. No session holds the supervisor's role and nothing starts or
relays one: while the loop is down the standby watch restarts its systemd
service (Linux; on macOS launchd restarts a failed loop itself), and says
SUPERVISOR LOOP DOWN and notifies until it runs again.

Without a subcommand, shows whether the loop runs (exit 3 when it does not).`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return a.supervisorStatus() },
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Show whether the supervisor loop runs (exit 3 when it does not)",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return a.supervisorStatus() },
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
	Loop bool `json:"loop"`
	PID  int  `json:"pid,omitempty"`
}

// supervisorStatus says whether the supervisor loop runs: exit 3 when not.
func (a *app) supervisorStatus() error {
	pid := a.loopPID()
	if a.json {
		_ = a.printJSON(supervisorView{Loop: pid > 0, PID: pid})
	}
	if pid == 0 {
		return refused("the supervisor loop does not run (%s)", loopServiceHint())
	}
	if a.json {
		return nil
	}
	_, err := fmt.Fprintf(a.out, "the supervisor loop runs (PID %d)\n", pid)
	return err
}
