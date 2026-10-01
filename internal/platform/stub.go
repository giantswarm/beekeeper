package platform

import (
	"context"
	"os/exec"
	"time"

	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/notify"
	"github.com/giantswarm/beekeeper/internal/proc"
)

// Stub is the platform with every part not available: the build's own on
// other systems and with the nosystemd tag, and in every build the one the
// degrade paths are tested against.
func Stub() Platform {
	return Platform{
		Machine:     stubMachine{},
		Launcher:    stubLauncher{},
		Capper:      stubCapper{},
		Opener:      stubOpener{},
		NewNotifier: func() Notifier { return stubNotifier{} },
	}
}

func missing(part string) error { return &NotAvailableError{Part: part} }

type stubMachine struct{}

func (stubMachine) Mem() (machine.Mem, error)        { return machine.Mem{}, missing("Machine.Mem") }
func (stubMachine) Load() ([3]float64, error)        { return [3]float64{}, missing("Machine.Load") }
func (stubMachine) Forks() (uint64, error)           { return 0, missing("Machine.Forks") }
func (stubMachine) MemoryPressure() (float64, error) { return 0, missing("Machine.MemoryPressure") }
func (stubMachine) CPUPressure() (float64, error)    { return 0, missing("Machine.CPUPressure") }
func (stubMachine) Processes() (*proc.Table, error)  { return nil, missing("Machine.Processes") }
func (stubMachine) Started(int) (time.Time, error)   { return time.Time{}, missing("Machine.Started") }
func (stubMachine) DesktopScope() (*machine.Scope, error) {
	return nil, missing("Machine.DesktopScope")
}
func (stubMachine) MemcapScope(string) *machine.Scope {
	return nil
}
func (stubMachine) ScopePIDs(string) []int  { return nil }
func (stubMachine) CgroupPIDs(string) []int { return nil }
func (stubMachine) OOMPolicy(string) string { return "?" }
func (stubMachine) OOMDSwapLimit() int      { return machine.DefaultOOMDSwapLimit }
func (stubMachine) SwapoffRuns() bool       { return false }
func (stubMachine) OOMKills(context.Context, time.Time) ([]machine.OOMKill, error) {
	return nil, missing("Machine.OOMKills")
}
func (stubMachine) OomdKills(context.Context, time.Time) ([]string, error) {
	return nil, missing("Machine.OomdKills")
}
func (stubMachine) ServiceLog(context.Context, string, string) ([]byte, error) {
	return nil, missing("Machine.ServiceLog")
}

type stubLauncher struct{}

func (stubLauncher) Available() bool                                   { return false }
func (stubLauncher) Start(Unit) error                                  { return missing("Launcher.Start") }
func (stubLauncher) Freeze(context.Context, string) error              { return missing("Launcher.Freeze") }
func (stubLauncher) Thaw(context.Context, string) error                { return missing("Launcher.Thaw") }
func (stubLauncher) State(context.Context, string) string              { return "" }
func (stubLauncher) Running(context.Context, bool, ...string) []string { return nil }

type stubCapper struct{}

func (stubCapper) Available() bool { return false }
func (stubCapper) Capped() bool    { return false }
func (stubCapper) Command(string, Cap, []string) (*exec.Cmd, error) {
	return nil, missing("Capper.Command")
}

type stubOpener struct{}

func (stubOpener) Running(*proc.Table) time.Time { return time.Time{} }
func (stubOpener) Open(context.Context, string, bool) error {
	return missing("Opener.Open")
}

type stubNotifier struct{}

func (stubNotifier) Send(context.Context, notify.Message) (uint32, error) {
	return 0, missing("Notifier.Send")
}
func (stubNotifier) Close() error { return nil }
