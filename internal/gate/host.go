package gate

import (
	"context"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/state"
)

// Host is the command the gate runs in: its configuration, state, clock and
// output, and the command's own steps. The command's app is
// the one implementation.
type Host interface {
	As() string
	CallCentralRaw(ctx context.Context, me state.Party, tool string, args map[string]any, v any) (string, error)
	Cfg() *config.Config
	CfgPath() string
	CheckPlaces(ctx context.Context, by state.Party, now time.Time, lane string, repo string, pr int, every time.Duration) error
	CheckRepositoryVisibility(ctx context.Context, repo string) error
	CloseToolWindow(ctx context.Context, by state.Party)
	ExplicitConfig() string
	FatalPublicationBarrier(ctx context.Context, st *state.State, repo string, pr int) error
	InstallTool(ctx context.Context, by state.Party, verb string, what string) error
	LeaveCentral(ctx context.Context, merges []state.Merge, why string)
	LoadedCfg() string
	MergeEvents(evs []state.Event) ([]state.Event, error)
	Now() time.Time
	PollToolWindow(ctx context.Context, by state.Party)
	Present(m state.Merge) bool
	RecordGone(ctx context.Context) ([]string, []string, []state.Merge)
	SetCfg(v *config.Config)
	SetNow(v time.Time)
	Store() state.Store
	TeleportCause(ctx context.Context) string
}

// Command is the command package's functions the gate calls. They stay
// there until their own packages exist; a field goes when its function has
// moved to a package the gate can import.
type Command struct {
	AwaitsRelease         func(base string) bool
	BinaryVersion         func(ctx context.Context, file string) string
	Clock                 func(now time.Time, t time.Time) string
	DevctlCommandEnv      func(configPath string) []string
	EndChild              func(pid int) error
	Event                 func(by state.Party, verb string, format string, a ...any) state.Event
	HandOver              func(base string, r ChildRun, cli int)
	HoldTarget            func(h state.Hold) string
	LastLine              func(path string) string
	LastOf                func(s string) string
	MergeAuthority        func(ctx context.Context, a Host, repo string, pr int, targetHead string) (string, bool, error)
	NewExitError          func(code int, msg string, err error) error
	OutliveCaller         func() func()
	OwnedBase             func(stateDir string, argv []string, pid int) string
	ParseRef              func(s string) (github.PR, bool)
	Plat                  func() platform.Platform
	ReadBudget            func(a Host, ctx context.Context) (github.Budget, error)
	ReadPID               func(base string) int
	RecordDeliveryRelease func(st *state.State, repo string, pr int, out merge.Outcome, handCut string, now time.Time) []state.Event
	RecordRun             func(st *state.State, i int, lane config.Lane, by state.Party, r RunOutcome, now time.Time, note string) []state.Event
	ReplacedBinary        func(pid int) (platform.Install, bool)
	RunFailure            func(doc []byte, last string, handCut string) string
	StoredGraphQL         func(g *github.GraphQL, at time.Time) *state.GraphQL
	UntilText             func(a Host, h state.Hold) string
	UserSystemd           func() bool
	WakeID                func(p state.Party) string
	WithInfra             func(reason string, infra []merge.Infra) string
}

// Cmd is the command's functions, set by the command at its start.
var Cmd Command
