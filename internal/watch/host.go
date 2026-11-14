package watch

import (
	"context"
	"io"
	"regexp"
	"time"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/central"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/free"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/upgrade"
)

// Host is the command the watch runs in: its configuration, state, clock and
// output, and the command's own steps. The command's app is
// the one implementation.
type Host interface {
	AlertCycle(ctx context.Context, store *alerts.Store, r *alerts.Reader) ([]string, []string)
	AlertDiscovery(ctx context.Context) ([]string, []string, error)
	AlertReader() *alerts.Reader
	AlertRules() alerts.Rules
	AppDrifts(ctx context.Context, records []state.App) map[string]string
	BackgroundAgent(ctx context.Context, st *state.State, p state.Party) bool
	CacheParkRefs(agents []state.Agent, refs map[github.PR]github.RefState)
	CachedProvidersCapacityAt(k config.Capacity, now time.Time, providerErrs map[string]string) (config.Capacity, *capacity.UsageView)
	CachedUsageReading(file string, u config.Usage, now time.Time, cause error) (capacity.UsageReading, error)
	CallCentralRaw(ctx context.Context, me state.Party, tool string, args map[string]any, v any) (string, error)
	CentralNotes(ctx context.Context, me state.Party) ([]state.Note, error)
	CentralNotesOn() bool
	Cfg() *config.Config
	CfgPath() string
	CheckDeliveries(now time.Time) (string, error)
	CheckPlaces(ctx context.Context, by state.Party, now time.Time, lane string, repo string, pr int, every time.Duration) error
	CheckThroughput(now time.Time, held bool, progress ...func()) (string, error)
	Context() context.Context
	DesktopCap() int
	DesktopMode() bool
	DiskPathRoots() []string
	ExecutionClient(h execution.Host) execution.Transport
	ExecutionHost(selector string) (execution.Host, error)
	ExecutionHostRetirable(st *state.State, r *state.ExecutionRecord) bool
	ExplicitConfig() string
	HasRow(id string) bool
	HasUnrecoveredPublication(r *state.ExecutionRecord) bool
	Hub() *central.Client
	InPlay(now time.Time) func(string) bool
	LeaseSettled(st *state.State, ag state.Agent, res string) (string, bool)
	LeaveCentral(ctx context.Context, merges []state.Merge, why string)
	LiveCapacity() config.Capacity
	LogQuiet(lines ...string)
	LoopGitHub(ctx context.Context, step string, run func() error) error
	LoopTimerChecks(ctx context.Context, timers []state.Timer, now time.Time, held bool) map[int]CheckResult
	Now() time.Time
	Out() io.Writer
	PagerDutyCycle(ctx context.Context, store *alerts.Store, run central.Runner) []string
	Paid(o state.Owed, why ...string)
	PayOwed(ctx context.Context, o state.Owed)
	PollToolWindow(ctx context.Context, by state.Party)
	ReadHeadroom(ctx context.Context, k config.Capacity, swap *capacity.SwapReading) *capacity.Headroom
	ReadSnapshot() (*state.State, error)
	RearmRecovery()
	RecheckLeaseParks(st *state.State, parks []SettledPark) []SettledPark
	ReconcileServeModes(ctx context.Context) error
	RecordGone(ctx context.Context) ([]string, []string, []state.Merge)
	RecordStoppedGates(ctx context.Context)
	Release(ctx context.Context, by state.Party, n *state.Note, how string) []string
	RequireClaudeLogin() error
	ResetFirst(st *state.State) bool
	ResolveReading(ctx context.Context, file string, u config.Usage) (capacity.UsageReading, error)
	ResolveUsage(ctx context.Context, k config.Capacity) (config.Capacity, *capacity.UsageView)
	RetainUsageStop(file string, err error)
	RetireExecution(ctx context.Context, ref execution.Ref, trigger string, now time.Time, skipHost bool) error
	Rowless(ctx context.Context, st *state.State, turning func(context.Context, string) bool) []state.Agent
	ServeModeReadings(ctx context.Context, name string, p config.Provider) capacity.ProviderReadingResult
	SetCtx(v context.Context)
	SetNow(v time.Time)
	SettleOverrides(ctx context.Context, by state.Party, check func([]state.Timer) map[int]CheckResult) ([]string, error)
	SlotView() (string, int)
	StopText(s *state.Stop) string
	Store() state.Store
	SyncAgyQuotaFromState(st *state.State, now time.Time)
	SyncClaims(ctx context.Context) error
	SyncExecutionHost(ctx context.Context, selector string) error
	UpgradeStatuses(ctx context.Context, st *state.State, now time.Time, rs upgrade.Readings, fresh func(installation string) time.Duration) []upgrade.Status
	UseCodexRolloutUsage(u config.Usage, cached capacity.UsageReading) bool
	WakeAgent(ctx context.Context, by state.Party, q string, msg string, mode string) error
}

// Command is the command package's functions the watch calls. They stay
// there until their own packages exist; a field goes when its function has
// moved to a package the watch can import.
type Command struct {
	AgentArgv                     func(bin string, id string, name string, model string, brief string, flags ...string) []string
	AgentOfSession                func(st *state.State, id string) int
	AgyQuotaError                 func(cfg *config.Config, ag state.Agent) *agy.QuotaError
	AlertOwnerKey                 func(o state.AlertOwner) string
	AnsweredLine                  func(id int, by string, answer string) string
	BinaryVersion                 func(ctx context.Context, file string) string
	BudgetGateArgs                func() []string
	CapacityStopped               func(v *capacity.View, a Host, st *state.State) *capacity.View
	CheckTimers                   func(ctx context.Context, timers []state.Timer, now time.Time, lowBudget bool) map[int]CheckResult
	Clock                         func(now time.Time, t time.Time) string
	CloseDefaulted                func(st *state.State, person string, by state.Party, now time.Time) ([]string, []state.Event, []state.Note)
	CountAgents                   func(st *state.State, sessions []*claude.Session, now time.Time, cfg *config.Config) capacity.AgentCount
	CountProviderAgents           func(st *state.State, sessions []*claude.Session, k config.Capacity, now time.Time, cfg *config.Config) map[string]capacity.ProviderAgents
	DesktopCLIs                   func(t *proc.Table) int
	DevctlCommandEnv              func(configPath string) []string
	Discover                      func(cfg *config.Config, t *proc.Table, now time.Time) []*claude.Session
	DiskPathGrowth                func(cur []DiskPath, prev []DiskPath)
	DiskPathText                  func(p DiskPath) string
	Display                       func(args []string) string
	Dur                           func(d time.Duration) string
	EndRevived                    func(ctx context.Context, s *claude.Session) (int, error)
	EndSession                    func(ctx context.Context, s *claude.Session) (int, error)
	EndedTimerTarget              func(st *state.State, target string) bool
	ErrRestart                    func() error
	EscapedLine                   func(s string) string
	Event                         func(by state.Party, verb string, format string, a ...any) state.Event
	ExecutionNeedsSync            func(st *state.State, r *state.ExecutionRecord) bool
	ExecutionSyncBackoff          func(failures int) time.Duration
	ExecutionTranscriptPending    func(r *state.ExecutionRecord) bool
	ExecutionUnfinished           func(st *state.State, r *state.ExecutionRecord) bool
	FirstLine                     func(s string) string
	GibStr                        func(b int64) string
	GoBuilds                      func() []int
	GoneAgent                     func(cfg *config.Config, st *state.State, sessions []*claude.Session, ag state.Agent, staleAfter time.Duration, now time.Time) (time.Time, bool)
	GoneUnrecorded                func(r state.Role, sessions []*claude.Session) bool
	GttLine                       func(gpus []machine.GPU, models []machine.HostModel) string
	GuideReportEvents             func(st *state.State, events []state.Event, now time.Time) []state.Event
	GuideRole                     func() Role
	GuideWorkBlocks               func(work state.GuideWork, now time.Time) bool
	GuideWorkText                 func(st *state.State) string
	Guides                        func(person string, n *state.Note) bool
	HeadlessTurn                  func(t *proc.Table, id string) string
	Heard                         func(o state.Owed) bool
	HoldStands                    func(st *state.State, target string, now time.Time) bool
	HoldTarget                    func(h state.Hold) string
	HoldsRole                     func(st *state.State, p state.Party) bool
	HostModels                    func(ctx context.Context, cfg *config.Config) ([]machine.HostModel, error)
	ImportStatus                  func(ag state.Agent, now time.Time) string
	IsUnreachable                 func(err error) bool
	IsWatch                       func(p *proc.Process) bool
	KeepsRole                     func(st *state.State, p state.Party) bool
	KeptBy                        func(st *state.State, ag state.Agent, now time.Time) string
	Launch                        func(unit string, dir string, config string, stopPost []string, argv []string, env ...string) error
	ListOrNone                    func(items []string) string
	LoopServiceHint               func() string
	LowBudget                     func(b *state.Budget, floor int, now time.Time) bool
	MergeCheckGroup               func(c *proc.Cmd)
	MergeInFlight                 func(st *state.State, p state.Party, alive func(int) bool) bool
	NamesPerson                   func(waits string, person string) bool
	NoteClosing                   func(verb string) bool
	OneLine                       func(s string) string
	Overtakable                   func(n state.Note, asking int) bool
	OvertakenEvent                func(by state.Party, n state.Note, reason string) state.Event
	OweArchive                    func(st *state.State, p state.Party, why string, now time.Time) string
	Owner                         func(t *proc.Table, pid int, owners map[int]string) string
	ParkCentralNote               func(on string) (string, int, bool)
	ParkHold                      func(on string) (string, bool)
	ParkLease                     func(on string) (string, bool)
	ParkNote                      func(on string) (int, bool)
	ParkedAgents                  func(st *state.State) []state.Agent
	ParkedOn                      func(waits string, person string) string
	ParkedText                    func(p *state.Park, now time.Time) string
	ParseAnswered                 func(e state.Event) (loop.Answered, bool)
	ParseRef                      func(s string) (github.PR, bool)
	ParseReleaseTarget            func(on string) (string, string, bool)
	PastRun                       func(st *state.State, p state.Party, now time.Time) bool
	Plat                          func() platform.Platform
	PrintsTurn                    func(p *proc.Process) bool
	ProcessBinary                 func(pid int) (string, bool)
	ReadDiskPaths                 func(ctx context.Context, extra []string) []DiskPath
	ReadHolder                    func(r state.Role, sessions []*claude.Session, now time.Time, grace time.Duration) Supervision
	Rearm                         func(args []string) string
	RebindSessionState            func(st *state.State, oldID string, newID string, hostID string) bool
	RecordText                    func(r state.Record) string
	RefName                       func(r github.PR) string
	RefStates                     func(ctx context.Context, refs []github.PR) (map[github.PR]github.RefState, error)
	Refused                       func(format string, a ...any) error
	RelayPending                  func(st *state.State, r state.Role, now time.Time) bool
	RenameHolder                  func(sup *state.Supervisor, sessions []*claude.Session) bool
	ReportDue                     func(r *state.Report, pause *state.ReportPause, rc config.Reporter, now time.Time, posted bool, ended bool) ReportAction
	ReportSlot                    func(every time.Duration, now time.Time) time.Time
	ReporterParty                 func() state.Party
	ResumeHint                    func(ag state.Agent) string
	Resumes                       func(args []string, id string) bool
	RetainGuideReport             func(st *state.State, key string, text string, at time.Time, by state.Party) bool
	RoleSlash                     func(cfg config.Role) string
	Roles                         func() []Role
	RosterKept                    func(st *state.State, p state.Party, now time.Time) string
	RowlessCandidate              func(st *state.State, ag state.Agent, now time.Time) (state.Start, bool)
	SessionContext                func(sessions []*claude.Session, p state.Party) int64
	SettleTimersMatching          func(st *state.State, found map[int]CheckResult, now time.Time, tick time.Duration, accept func(state.Timer) bool) ([]string, []state.Event, []TimerFire, bool)
	SizeStr                       func(b int64) string
	Started                       func(st *state.State, p state.Party) bool
	StartsHold                    func(st *state.State, now time.Time) *state.Hold
	StartsSession                 func(p *proc.Process, id string) bool
	StopUnits                     func(ctx context.Context, id string) []string
	Superseded                    func(st *state.State, p state.Party, now time.Time) (string, time.Time)
	TakesRelay                    func(to state.Party, me state.Party) bool
	TokensText                    func(n int64) string
	TopDiskPaths                  func(paths []DiskPath, growth bool, n int) []DiskPath
	TranscriptContext             func(s *claude.Session) int64
	TranscriptOf                  func(cfg *config.Config, id string) string
	Truncate                      func(s string, n int) string
	UnitEnded                     func(ctx context.Context, unit string) bool
	UnreadableReason              func(err error) string
	UnsafeKey                     func() *regexp.Regexp
	UsageErr                      func(format string, a ...any) error
	UsageHighBucketFile           func(name string, bucket string) string
	UsageHighProviderFile         func(name string) string
	UsageProviderFile             func(name string) string
	WaitsOf                       func(records []state.Record, ag state.Agent) string
	WakeID                        func(p state.Party) string
	WakeRoute                     func(ag state.Agent) bool
	WakeTargetPark                func(st *state.State, q string) *state.Park
	WatchAutoFree                 func(w *Watcher, now time.Time)
	WatchAutoResume               func(w *Watcher, ctx context.Context, st *state.State)
	WatchBudget                   func(w *Watcher, ctx context.Context, now time.Time)
	WatchCallersText              func(w *Watcher) string
	WatchClaudeLogin              func(w *Watcher)
	WatchDeliverWakeTimer         func(w *Watcher, ctx context.Context, st *state.State, original TimerFire, f TimerFire) TimerWakeOutcome
	WatchDiskPrune                func(w *Watcher, now time.Time, pressure bool)
	WatchDocker                   func(w *Watcher, now time.Time)
	WatchDoctor                   func(w *Watcher, ctx context.Context)
	WatchDrainMailbox             func(w *Watcher, ctx context.Context) bool
	WatchExecUsageNow             func(w *Watcher, ctx context.Context, file string, u config.Usage) (capacity.UsageReading, error)
	WatchFreeDue                  func(w *Watcher, read func() (free.Machine, error), now time.Time, every time.Duration)
	WatchGoCacheLook              func(w *Watcher, now time.Time, builds func() []int)
	WatchHandCITimerToReviewRead  func(w *Watcher, ctx context.Context, f TimerFire, ag state.Agent, ref string) (bool, error)
	WatchHungMerges               func(w *Watcher, ctx context.Context, now time.Time)
	WatchKills                    func(w *Watcher, ctx context.Context, since time.Time, sessions []*claude.Session, t *proc.Table)
	WatchKindSpending             func(w *Watcher, now time.Time)
	WatchNewGuideRecovery         func(w *Watcher) *Watcher
	WatchNoteFailedSuccessor      func(w *Watcher, rl Role, holder state.Party) int
	WatchRecoverGuide             func(w *Watcher, ctx context.Context)
	WatchRunaways                 func(w *Watcher, sessions []*claude.Session, t *proc.Table)
	WatchSameTitles               func(w *Watcher, sessions []*claude.Session)
	WatchStaleLeases              func(w *Watcher, ctx context.Context, sessions []*claude.Session)
	WatchStalls                   func(w *Watcher)
	WatchStartReport              func(w *Watcher, ctx context.Context, sessions []*claude.Session, action ReportAction) error
	WatchStateHolders             func(w *Watcher, now time.Time)
	WatchStopParked               func(w *Watcher, ctx context.Context, st *state.State, sessions []*claude.Session)
	WatchSuspends                 func(w *Watcher, ctx context.Context, since time.Time, sessions []*claude.Session)
	WatchTeleport                 func(w *Watcher, ctx context.Context)
	WatchTellBusyWorkersUsageHigh func(w *Watcher, ctx context.Context, st *state.State, sessions []*claude.Session, u *capacity.UsageView, ledgerFile string, match func(state.Agent) bool)
	WatchTellDefaulted            func(w *Watcher, ctx context.Context, notes []state.Note)
	WatchTendFeedback             func(w *Watcher, ctx context.Context)
	WatchUngated                  func(w *Watcher, sessions []*claude.Session, t *proc.Table)
	WatchWorktrees                func(w *Watcher, now time.Time)
	WithPaceOffs                  func(readings map[string]capacity.ProviderReadingResult, st *state.State, now time.Time)
}

// Cmd is the command's functions, set by the command at its start.
var Cmd Command
