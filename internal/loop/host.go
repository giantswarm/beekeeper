package loop

import (
	"context"
	"io"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
	"github.com/giantswarm/beekeeper/internal/worktrees"
)

// Host is the command the loop runs in: its configuration, state, clock and
// output, and the command's own steps the loop takes. The command's app is
// the one implementation; the loop holds no other reference to it.
type Host interface {
	AllowMonitoredIssue(ctx context.Context, ref string) error
	AssignExecutionReview(ctx context.Context, act superloop.Action) error
	BoardClient() *board.Client
	Cfg() *config.Config
	CfgPath() string
	CheckDeliveries(now time.Time) (string, error)
	CheckThroughput(now time.Time, held bool, progress ...func()) (string, error)
	CleanRemovedWorktree(ctx context.Context, dir string, publishedHead string)
	ClearLoopVersion()
	ConsumeReports(ctx context.Context) error
	DeliveryExemptIntake(st *state.State) bool
	DeliveryPolicy() any
	EndAgy(ctx context.Context, id string) string
	ExecutionHost(selector string) (execution.Host, error)
	ExecutionMergeDecisionLive(ctx context.Context, st *state.State, use execution.MergeUse, loopDecision bool) error
	FatalAdmission(name string, review bool) error
	FatalPublicationBarrier(ctx context.Context, st *state.State, repo string, pr int) error
	FinishReviewRequest(act superloop.Action, started bool)
	HoldOn(ctx context.Context, notes []state.Note, ref string) (*state.ItemHold, error)
	HostPinToolsCurrent(pin state.HostPin, h execution.Host) bool
	HostPlacementViews(st *state.State) []capacity.HostPlacementView
	LastNoteContext(ctx context.Context, p state.Party, since time.Time, progress ...func()) (string, bool, error)
	LoadedCfg() string
	LoopGitHub(ctx context.Context, step string, run func() error) error
	LoopNow() *atomic.Pointer[time.Time]
	MonitoredIssueStarted(ctx context.Context, ref string) error
	NextMonitoredIssue(ctx context.Context, skip ...string) (*LoopBoardItem, error)
	Now() time.Time
	OperationOutcome(operation string, reason string, rawURL string, success bool) error
	Out() io.Writer
	ParkIsPull(ctx context.Context, st *state.State, ref string) bool
	PrepareEventSegmentation() error
	ReadWatchLevel(kind string, value any) (time.Time, error)
	RecordAgyQuotaExhaustedForID(id string, model string, qerr *agy.QuotaError, now time.Time)
	RecordFatalRepair(owner string, ref string) error
	RecoveryHeld() string
	ReserveReview(act superloop.Action) error
	ResetFirst(st *state.State) bool
	RuleModel(name string, kind string) string
	Sessions() ([]*claude.Session, *proc.Table, error)
	SetCfg(v *config.Config)
	SetLoopNow(v *atomic.Pointer[time.Time])
	SetNow(v time.Time)
	SetOut(v io.Writer)
	SnapshotAtNow() Host
	StartLoopWatch(ctx context.Context) func()
	Store() state.Store
	SyncClaims(ctx context.Context) error
	TimeNow() time.Time
	UnmergedPolicy() (int, time.Duration)
	WriteLoopVersion() error
}

// Command is the command package's functions the loop calls. They stay
// there until their own packages exist; a field goes when its function has
// moved to a package the loop can import.
type Command struct {
	About                      func(e state.Event, ref string) state.Event
	AgyAgentStartID            func(ag state.Agent) string
	AgyQuotaError              func(cfg *config.Config, ag state.Agent) *agy.QuotaError
	AlertOwnerKey              func(o state.AlertOwner) string
	ApplyDeliveryVerdict       func(d *state.Delivery, verdict string, tag string, now time.Time)
	BudgetGateArgs             func() []string
	ClassifyDelivery           func(st *state.State, d state.Delivery, now time.Time, p any) DeliveryFlow
	ClearRecords               func(st *state.State, p state.Party) []state.Record
	Clock                      func(now time.Time, t time.Time) string
	Code                       func(err error) int
	ContinuationName           func(ag state.Agent) string
	CoordinatorHostUpdateReady func(version string) error
	CoordinatorVersion         func() string
	DeliveryAgent              func(st *state.State, d state.Delivery, ag state.Agent) bool
	DeliveryExempt             func(st *state.State, now time.Time, ref string, exempt bool) bool
	DeliveryURL                func(ref string, pull bool) string
	DevctlCommandEnv           func(configPath string) []string
	EffortFlags                func(level string) []string
	EndNotes                   func(st *state.State, worker state.Party, now time.Time)
	EndedParkCandidate         func(ag state.Agent) bool
	EpicChildStatus            func(st *state.State, child board.Item, now time.Time, policy any) string
	EpicOwner                  func(st *state.State, ref string) *state.Agent
	ErrGone                    func() error
	EscapedLine                func(s string) string
	Event                      func(by state.Party, verb string, format string, a ...any) state.Event
	ExecutionRecord            func(st *state.State, ref execution.Ref) (*state.ExecutionRecord, int)
	FreshPark                  func(st *state.State, ag state.Agent) bool
	FreshSessions              func(p0 Host) ([]*claude.Session, *proc.Table, error)
	HeldBy                     func(notes []state.Note, ref string, except int) (*state.Note, *state.ItemHold)
	HostExecutions             func(st *state.State, selector string) int
	IncidentExempt             func(st *state.State, now time.Time, refs ...string) bool
	InheritReviewParkOnAgent   func(st *state.State, ag *state.Agent, rp state.ReviewPark, now time.Time)
	IsDeskReviewWait           func(waits string) bool
	IssueURL                   func(ref string) string
	LoopBroker                 func(ctx context.Context) (string, error)
	LoopEpicStart              func(l *Loop, next LoopBoardItem) error
	LoopFiledIssues            func(l *Loop, st *state.State) []superloop.FiledIssue
	LoopFinishContinuation     func(l *Loop, old state.Agent, name string) error
	LoopFireTimers             func(l *Loop, ctx context.Context, st *state.State) error
	LoopInTurn                 func(a Host, p state.Party) bool
	LoopRecoverUnfinishedTurns func(l *Loop, ctx context.Context, st *state.State) error
	LoopResumable              func(a Host, st *state.State, ag state.Agent) bool
	LoopResumePark             func(a Host, ctx context.Context, by state.Party, q string) error
	LoopRoster                 func(l *Loop, ctx context.Context, planned map[string]bool) error
	LoopSessions               func(a Host) ([]*claude.Session, *proc.Table, error)
	LoopSettleOverrides        func(l *Loop, ctx context.Context, st *state.State)
	LowBudget                  func(b *state.Budget, floor int, now time.Time) bool
	NamedRefs                  func(text string, owner string) []string
	NewExitError               func(code int, msg string, err error) error
	OneLine                    func(s string) string
	ParkMessage                func(p *state.Park, by string) string
	ParkedBetweenTurns         func(st *state.State, x *state.ExecutionRecord) bool
	ParseAnswered              func(e state.Event) (Answered, bool)
	ParseRef                   func(s string) (github.PR, bool)
	PauseChangeFor             func(ctx context.Context, d time.Duration) bool
	PublicPullURL              func(raw string) string
	PublishedParkVerification  func(ctx context.Context, on string, dir string, git worktrees.Git, gh github.GH) (string, string)
	ReadDeliveryWIP            func(st *state.State, limit int, interval time.Duration, now time.Time, policies ...any) *DeliveryWIP
	ReadIntakeAnchor           func(ctx context.Context, a Host, ref string, from string) (string, error)
	ReadLoopAlerts             func(p0 context.Context, a Host) ([]alerts.Firing, error)
	ReadRepoOpenPulls          func(ctx context.Context, repo string) ([]UnmergedPull, error)
	RecentUnmergedPulls        func(repo string, pulls []UnmergedPull, now time.Time, window time.Duration) []UnmergedPull
	RecordAgentReviewPark      func(st *state.State, ag state.Agent)
	RefName                    func(r github.PR) string
	RefStates                  func(ctx context.Context, refs []github.PR) (map[github.PR]github.RefState, error)
	RefURL                     func() *regexp.Regexp
	RefreshLoopBudget          func(a Host, ctx context.Context) (*state.Budget, error)
	Refused                    func(format string, a ...any) error
	ReleaseVersion             func(v string) bool
	RemoveAgent                func(st *state.State, i int)
	ReportIdle                 func(ag *state.Agent, now time.Time)
	RepositoryStartReason      func(st *state.State, now time.Time, ref string) string
	ReviewContext              func(ctx context.Context, p superloop.Pull, previous string, threshold int) (string, error)
	RoleRun                    func(st *state.State, p state.Party) bool
	RunGit                     func(ctx context.Context, dir string, args ...string) ([]byte, error)
	ShortDur                   func(d time.Duration) string
	StopMerges                 func(store state.Store, by state.Party, pick func(state.Merge) bool, keepSeeded bool, how func(state.Merge) string) ([]string, error)
	Truncate                   func(s string, n int) string
	Unactivated                func(r *state.ExecutionRecord) bool
	UnmergedHoldReason         func(repo string, recent []UnmergedPull, limit int, window time.Duration) string
	UnmergedWindowLabel        func(d time.Duration) string
}

// Cmd is the command's functions, set by the command before any loop runs.
var Cmd Command
