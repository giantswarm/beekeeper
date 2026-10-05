package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/platform"
)

// askTimeout bounds a request to the broker: it answers in milliseconds.
const askTimeout = 10 * time.Second

// pingTimeout bounds the broker's ping, which every capped run of a
// sandboxed session pays.
const pingTimeout = 2 * time.Second

// Capper caps the runs of a sandboxed session through the host's broker:
// the sandbox keeps the service manager out of reach, so the broker caps
// the slot's slice and moves the command into its scope. The command
// itself stays in the sandbox.
type Capper struct {
	// Dir is the broker's spool.
	Dir string
	// Exe is this binary, whose "sandbox scope" asks the broker for the
	// scope and then becomes the command.
	Exe string
}

// Available reports whether a broker answers.
func (c Capper) Available() bool { return Ask(c.Dir, Request{Op: OpPing}, pingTimeout) == nil }

// Capped reports whether this process runs in a capped run's scope.
func (Capper) Capped() bool {
	raw, err := os.ReadFile("/proc/self/cgroup")
	return err == nil && strings.Contains(string(raw), "/memcap.slice/")
}

// CapSlot asks the broker to cap the slot's slice.
func (c Capper) CapSlot(cp platform.Cap) error {
	return Ask(c.Dir, Request{Op: OpCapSlot, Slice: cp.Slice, Max: cp.Max, Swap: cp.Swap}, askTimeout)
}

// Command is "<Exe> sandbox scope … -- argv": it asks the broker of Dir
// for the scope name around itself and then execs argv in it.
func (c Capper) Command(name string, cp platform.Cap, argv []string) (*exec.Cmd, error) {
	args := append([]string{"sandbox", "scope", "--spool", c.Dir, "--unit", name, "--slice", cp.Slice, "--max", cp.Max, "--swap", cp.Swap, "--"}, argv...)
	return exec.Command(c.Exe, args...), nil //nolint:gosec // this binary, running the caller's command is the purpose
}

// Adopt is the host's: a sandboxed session asks for its scope instead.
func (Capper) Adopt(int, string, platform.Cap) error {
	return errors.New("a sandboxed session asks the broker for its scope")
}

// EnterScope asks the broker to move this process into the capped scope
// unit, in cp.Slice.
func EnterScope(dir, unit string, cp platform.Cap) error {
	return Ask(dir, Request{Op: OpScope, Unit: unit, Slice: cp.Slice, Max: cp.Max, Swap: cp.Swap}, askTimeout)
}
