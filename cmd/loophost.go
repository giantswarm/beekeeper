package cmd

import (
	"context"
	"io"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// asApp is the app behind a loop's host; nil for none.
func asApp(h loop.Host) *app {
	a, _ := h.(*app)
	return a
}

// The loop calls the command through Cmd and reaches the app as its Host.
func init() {
	loop.Cmd = loop.Command{
		About:                about,
		AgyAgentStartID:      agyAgentStartID,
		AgyQuotaError:        agyQuotaError,
		AlertOwnerKey:        alertOwnerKey,
		ApplyDeliveryVerdict: applyDeliveryVerdict,
		BudgetGateArgs:       func() []string { return budgetGateArgs },
		ClassifyDelivery: func(st *state.State, d state.Delivery, now time.Time, p any) deliveryFlow {
			return classifyDelivery(st, d, now, p.(deliveryPolicy))
		},
		ClearRecords:               clearRecords,
		Clock:                      clock,
		CloseSupersededPulls:       closeDuplicateExecutionPulls,
		Code:                       Code,
		ContinuationName:           continuationName,
		CoordinatorHostUpdateReady: coordinatorHostUpdateReady,
		CoordinatorVersion:         func() string { return coordinatorVersion() },
		DeliveryAgent:              deliveryAgent,
		DeliveryExempt:             deliveryExempt,
		DeliveryURL:                deliveryURL,
		DevctlCommandEnv:           devctlCommandEnv,
		EffortFlags:                effortFlags,
		EndNotes:                   endNotes,
		EndedParkCandidate:         endedParkCandidate,
		EpicChildStatus: func(st *state.State, child board.Item, now time.Time, policy any) string {
			return epicChildStatus(st, child, now, policy.(deliveryPolicy))
		},
		EpicOwner:                  epicOwner,
		ErrGone:                    func() error { return errGone },
		EscapedLine:                escapedLine,
		Event:                      event,
		ExecutionRecord:            executionRecord,
		FreshPark:                  freshPark,
		FreshSessions:              func(p0 loop.Host) ([]*claude.Session, *proc.Table, error) { return freshSessions(asApp(p0)) },
		HeldBy:                     heldBy,
		HostExecutions:             hostExecutions,
		IncidentExempt:             incidentExempt,
		InheritReviewParkOnAgent:   inheritReviewParkOnAgent,
		IsDeskReviewWait:           isDeskReviewWait,
		IssueURL:                   issueURL,
		LoopBroker:                 func(ctx context.Context) (string, error) { return loopBroker(ctx) },
		LoopEpicStart:              loopEpicStart,
		LoopFiledIssues:            loopFiledIssues,
		LoopFinishContinuation:     loopFinishContinuation,
		LoopFireTimers:             loopFireTimers,
		LoopInTurn:                 func(a loop.Host, p state.Party) bool { return loopInTurn(asApp(a), p) },
		LoopRecoverUnfinishedTurns: loopRecoverUnfinishedTurns,
		LoopResumable:              func(a loop.Host, st *state.State, ag state.Agent) bool { return loopResumable(asApp(a), st, ag) },
		LoopResumePark: func(a loop.Host, ctx context.Context, by state.Party, q string) error {
			return loopResumePark(asApp(a), ctx, by, q)
		},
		LoopRoster:                loopRoster,
		LoopSessions:              func(a loop.Host) ([]*claude.Session, *proc.Table, error) { return loopSessions(asApp(a)) },
		LoopSettleOverrides:       loopSettleOverrides,
		LowBudget:                 lowBudget,
		NamedRefs:                 namedRefs,
		NewExitError:              newExitError,
		OneLine:                   oneLine,
		ParkMessage:               parkMessage,
		ParkedBetweenTurns:        parkedBetweenTurns,
		ParseAnswered:             parseAnswered,
		ParseRef:                  parseRef,
		PauseChangeFor:            pauseChangeFor,
		PublicPullURL:             publicPullURL,
		PublishedParkVerification: publishedParkVerification,
		ReadDeliveryWIP: func(st *state.State, limit int, interval time.Duration, now time.Time, policies ...any) *deliveryWIP {
			own := make([]deliveryPolicy, len(policies))
			for i, v := range policies {
				own[i] = v.(deliveryPolicy)
			}
			return readDeliveryWIP(st, limit, interval, now, own...)
		},
		ReadIntakeAnchor: func(ctx context.Context, a loop.Host, ref string, from string) (string, error) {
			return readIntakeAnchor(ctx, asApp(a), ref, from)
		},
		ReadLoopAlerts:        func(p0 context.Context, a loop.Host) ([]alerts.Firing, error) { return readLoopAlerts(p0, asApp(a)) },
		ReadRepoOpenPulls:     func(ctx context.Context, repo string) ([]unmergedPull, error) { return readRepoOpenPulls(ctx, repo) },
		RecentUnmergedPulls:   recentUnmergedPulls,
		RecordAgentReviewPark: recordAgentReviewPark,
		RefName:               refName,
		RefStates: func(ctx context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
			return refStates(ctx, refs)
		},
		RefURL: func() *regexp.Regexp { return refURL },
		RefreshLoopBudget: func(a loop.Host, ctx context.Context) (*state.Budget, error) {
			return refreshLoopBudget(asApp(a), ctx)
		},
		Refused:               refused,
		ReleaseVersion:        releaseVersion,
		RemoveAgent:           removeAgent,
		ReportIdle:            reportIdle,
		RepositoryStartReason: repositoryStartReason,
		ReviewContext:         reviewContext,
		RoleRun:               roleRun,
		RunGit:                runGit,
		ShortDur:              shortDur,
		ShortSHA:              shortSHA,
		StopMerges:            stopMerges,
		Truncate:              truncate,
		Unactivated:           unactivated,
		UnmergedHoldReason:    unmergedHoldReason,
		UnmergedWindowLabel:   unmergedWindowLabel,
	}
}

func (a *app) AllowMonitoredIssue(ctx context.Context, ref string) error {
	return a.allowMonitoredIssue(ctx, ref)
}

func (a *app) AssignExecutionReview(ctx context.Context, act superloop.Action) error {
	return a.assignExecutionReview(ctx, act)
}

func (a *app) BoardClient() *board.Client { return a.boardClient() }

func (a *app) Cfg() *config.Config { return a.cfg }

func (a *app) CfgPath() string { return a.cfgPath }

func (a *app) CheckDeliveries(now time.Time) (string, error) { return a.checkDeliveries(now) }

func (a *app) CheckThroughput(now time.Time, held bool, progress ...func()) (string, error) {
	return a.checkThroughput(now, held, progress...)
}

func (a *app) CleanRemovedWorktree(ctx context.Context, dir string, publishedHead string) {
	a.cleanRemovedWorktree(ctx, dir, publishedHead)
}

func (a *app) ClearLoopVersion() { a.clearLoopVersion() }

func (a *app) ConsumeReports(ctx context.Context) error { return a.consumeReports(ctx) }

func (a *app) DeliveryExemptIntake(st *state.State) bool { return a.deliveryExemptIntake(st) }

func (a *app) DeliveryPolicy() any { return a.deliveryPolicy() }

func (a *app) EndAgy(ctx context.Context, id string) string { return a.endAgy(ctx, id) }

func (a *app) ExecutionHost(selector string) (execution.Host, error) {
	return a.executionHost(selector)
}

func (a *app) ExecutionMergeDecisionLive(ctx context.Context, st *state.State, use execution.MergeUse, loopDecision bool) error {
	return a.executionMergeDecisionLive(ctx, st, use, loopDecision)
}

func (a *app) FatalAdmission(name string, review bool) error { return a.fatalAdmission(name, review) }

func (a *app) FatalPublicationBarrier(ctx context.Context, st *state.State, repo string, pr int) error {
	return a.fatalPublicationBarrier(ctx, st, repo, pr)
}

func (a *app) FinishReviewRequest(act superloop.Action, started bool) {
	a.finishReviewRequest(act, started)
}

func (a *app) HoldOn(ctx context.Context, notes []state.Note, ref string) (*state.ItemHold, error) {
	return a.holdOn(ctx, notes, ref)
}

func (a *app) HostPinToolsCurrent(pin state.HostPin, h execution.Host) bool {
	return a.hostPinToolsCurrent(pin, h)
}

func (a *app) HostPlacementViews(st *state.State) []hostPlacementView {
	return a.hostPlacementViews(st)
}

func (a *app) LastNoteContext(ctx context.Context, p state.Party, since time.Time, progress ...func()) (string, bool, error) {
	return a.lastNoteContext(ctx, p, since, progress...)
}

func (a *app) LoadedCfg() string { return a.loadedCfg }

func (a *app) LoopGitHub(ctx context.Context, step string, run func() error) error {
	return a.loopGitHub(ctx, step, run)
}

func (a *app) LoopNow() *atomic.Pointer[time.Time] { return a.loopNow }

func (a *app) MonitoredIssueStarted(ctx context.Context, ref string) error {
	return a.monitoredIssueStarted(ctx, ref)
}

func (a *app) NextMonitoredIssue(ctx context.Context, skip ...string) (*loop.LoopBoardItem, error) {
	return a.nextMonitoredIssue(ctx, skip...)
}

func (a *app) Now() time.Time { return a.now }

func (a *app) OperationOutcome(operation string, reason string, rawURL string, success bool) error {
	return a.operationOutcome(operation, reason, rawURL, success)
}

func (a *app) OtherDeskClaim(ctx context.Context, issue string) (string, error) {
	return a.otherDeskClaim(ctx, issue)
}

func (a *app) Out() io.Writer { return a.out }

func (a *app) ParkIsPull(ctx context.Context, st *state.State, ref string) bool {
	return a.parkIsPull(ctx, st, ref)
}

func (a *app) PrepareEventSegmentation() error { return a.prepareEventSegmentation() }

func (a *app) ReadWatchLevel(kind string, value any) (time.Time, error) {
	return a.readWatchLevel(kind, value)
}

func (a *app) RecordAgyQuotaExhaustedForID(id string, model string, qerr *agy.QuotaError, now time.Time) {
	a.recordAgyQuotaExhaustedForID(id, model, qerr, now)
}

func (a *app) RecordFatalRepair(owner string, ref string) error {
	return a.recordFatalRepair(owner, ref)
}

func (a *app) RecoveryHeld() string { return a.recoveryHeld() }

func (a *app) ReserveReview(act superloop.Action) error { return a.reserveReview(act) }

func (a *app) ResetFirst(st *state.State) bool { return a.resetFirst(st) }

func (a *app) RuleModel(name string, kind string) string { return a.ruleModel(name, kind) }

func (a *app) Sessions() ([]*claude.Session, *proc.Table, error) { return a.sessions() }

func (a *app) SetCfg(v *config.Config) { a.cfg = v }

func (a *app) SetLoopNow(v *atomic.Pointer[time.Time]) { a.loopNow = v }

func (a *app) SetNow(v time.Time) { a.now = v }

func (a *app) SetOut(v io.Writer) { a.out = v }

func (a *app) SnapshotAtNow() loop.Host { return a.snapshotAtNow() }

func (a *app) StartLoopWatch(ctx context.Context) func() { return a.startLoopWatch(ctx) }

func (a *app) Store() state.Store { return a.store }

func (a *app) SyncClaims(ctx context.Context) error { return a.syncClaims(ctx) }

func (a *app) TimeNow() time.Time { return a.timeNow() }

func (a *app) UnmergedPolicy() (int, time.Duration) { return a.unmergedPolicy() }

func (a *app) WriteLoopVersion() error { return a.writeLoopVersion() }
