package cmd

import (
	"context"
	"regexp"
	"time"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/central"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/upgrade"
	"github.com/giantswarm/beekeeper/internal/watch"
)

// watchApp is the app behind a host; nil for none.
func watchApp(h watch.Host) *app {
	a, _ := h.(*app)
	return a
}

// The watch calls the command through Cmd.
func init() {
	watch.Cmd = watch.Command{
		AgentArgv:      agentArgv,
		AgentOfSession: agentOfSession,
		AgyQuotaError:  agyQuotaError,
		AlertOwnerKey:  alertOwnerKey,
		AnsweredLine:   answeredLine,
		BinaryVersion:  binaryVersion,
		BudgetGateArgs: func() []string { return budgetGateArgs },
		CapacityStopped: func(v *capacity.View, a watch.Host, st *state.State) *capacity.View {
			return capacityStopped(v, watchApp(a), st)
		},
		CheckTimers:                checkTimers,
		Clock:                      clock,
		CloseDefaulted:             closeDefaulted,
		CountAgents:                countAgents,
		CountProviderAgents:        countProviderAgents,
		DesktopCLIs:                desktopCLIs,
		DevctlCommandEnv:           devctlCommandEnv,
		Discover:                   discover,
		DiskPathGrowth:             diskPathGrowth,
		DiskPathText:               diskPathText,
		Display:                    display,
		Dur:                        dur,
		EndRevived:                 func(ctx context.Context, s *claude.Session) (int, error) { return endRevived(ctx, s) },
		EndSession:                 endSession,
		EndedTimerTarget:           endedTimerTarget,
		ErrRestart:                 func() error { return errRestart },
		EscapedLine:                escapedLine,
		Event:                      event,
		ExecutionNeedsSync:         executionNeedsSync,
		ExecutionSyncBackoff:       executionSyncBackoff,
		ExecutionTranscriptPending: executionTranscriptPending,
		ExecutionUnfinished:        executionUnfinished,
		FirstLine:                  firstLine,
		GibStr:                     gibStr,
		GoBuilds:                   goBuilds,
		GoneAgent:                  goneAgent,
		GoneUnrecorded:             goneUnrecorded,
		GttLine:                    gttLine,
		GuideReportEvents:          guideReportEvents,
		GuideRole:                  func() role { return guideRole },
		GuideWorkBlocks:            guideWorkBlocks,
		GuideWorkText:              guideWorkText,
		Guides:                     guides,
		HeadlessTurn:               headlessTurn,
		Heard:                      heard,
		HoldStands:                 holdStands,
		HoldTarget:                 holdTarget,
		HoldsRole:                  holdsRole,
		HostModels:                 hostModels,
		ImportStatus:               importStatus,
		IsUnreachable:              isUnreachable,
		IsWatch:                    isWatch,
		KeepsRole:                  keepsRole,
		KeptBy:                     keptBy,
		Launch:                     launch,
		ListOrNone:                 listOrNone,
		LoopServiceHint:            loopServiceHint,
		LowBudget:                  lowBudget,
		MergeCheckGroup:            mergeCheckGroup,
		MergeInFlight:              mergeInFlight,
		NamesPerson:                namesPerson,
		NoteClosing:                noteClosing,
		OneLine:                    oneLine,
		Overtakable:                overtakable,
		OvertakenEvent:             overtakenEvent,
		OweArchive:                 oweArchive,
		Owner:                      owner,
		ParkCentralNote:            parkCentralNote,
		ParkHold:                   parkHold,
		ParkLease:                  parkLease,
		ParkNote:                   parkNote,
		ParkedAgents:               parkedAgents,
		ParkedOn:                   parkedOn,
		ParkedText:                 parkedText,
		ParseAnswered:              parseAnswered,
		ParseRef:                   parseRef,
		ParseReleaseTarget:         parseReleaseTarget,
		PastRun:                    pastRun,
		Plat:                       func() platform.Platform { return plat },
		PrintsTurn:                 printsTurn,
		ProcessBinary:              processBinary,
		ReadDiskPaths:              readDiskPaths,
		ReadHolder:                 readHolder,
		Rearm:                      rearm,
		RebindSessionState:         rebindSessionState,
		RecordText:                 recordText,
		RefName:                    refName,
		RefStates: func(ctx context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
			return refStates(ctx, refs)
		},
		Refused:                       refused,
		RelayPending:                  relayPending,
		RenameHolder:                  renameHolder,
		ReportDue:                     reportDue,
		ReportSlot:                    reportSlot,
		ReporterParty:                 func() state.Party { return reporterParty },
		ResumeHint:                    resumeHint,
		Resumes:                       resumes,
		RetainGuideReport:             retainGuideReport,
		RoleSlash:                     roleSlash,
		Roles:                         func() []role { return roles },
		RosterKept:                    rosterKept,
		RowlessCandidate:              rowlessCandidate,
		SessionContext:                sessionContext,
		SettleTimersMatching:          settleTimersMatching,
		SizeStr:                       sizeStr,
		Started:                       started,
		StartsHold:                    startsHold,
		StartsSession:                 startsSession,
		StopUnits:                     stopUnits,
		Superseded:                    superseded,
		TakesRelay:                    takesRelay,
		TokensText:                    tokensText,
		TopDiskPaths:                  topDiskPaths,
		TranscriptContext:             transcriptContext,
		TranscriptOf:                  transcriptOf,
		Truncate:                      truncate,
		UnitEnded:                     unitEnded,
		UnreadableReason:              unreadableReason,
		UnsafeKey:                     func() *regexp.Regexp { return unsafeKey },
		UsageErr:                      usageErr,
		UsageHighBucketFile:           usageHighBucketFile,
		UsageHighProviderFile:         usageHighProviderFile,
		UsageProviderFile:             usageProviderFile,
		WaitsOf:                       waitsOf,
		WakeID:                        wakeID,
		WakeRoute:                     wakeRoute,
		WakeTargetPark:                wakeTargetPark,
		WatchAutoFree:                 watchAutoFree,
		WatchAutoResume:               watchAutoResume,
		WatchBudget:                   watchBudget,
		WatchCallersText:              watchCallersText,
		WatchClaudeLogin:              watchClaudeLogin,
		WatchDeliverWakeTimer:         watchDeliverWakeTimer,
		WatchDiskPrune:                watchDiskPrune,
		WatchDocker:                   watchDocker,
		WatchDoctor:                   watchDoctor,
		WatchDrainMailbox:             watchDrainMailbox,
		WatchExecUsageNow:             watchExecUsageNow,
		WatchFreeDue:                  watchFreeDue,
		WatchGoCacheLook:              watchGoCacheLook,
		WatchHandCITimerToReviewRead:  watchHandCITimerToReviewRead,
		WatchHungMerges:               watchHungMerges,
		WatchKills:                    watchKills,
		WatchKindSpending:             watchKindSpending,
		WatchNewGuideRecovery:         watchNewGuideRecovery,
		WatchNoteFailedSuccessor:      watchNoteFailedSuccessor,
		WatchRecoverGuide:             watchRecoverGuide,
		WatchRunaways:                 watchRunaways,
		WatchSameTitles:               watchSameTitles,
		WatchStaleLeases:              watchStaleLeases,
		WatchStalls:                   watchStalls,
		WatchStartReport:              watchStartReport,
		WatchStateHolders:             watchStateHolders,
		WatchStopParked:               watchStopParked,
		WatchSuspends:                 watchSuspends,
		WatchTeleport:                 watchTeleport,
		WatchTellBusyWorkersUsageHigh: watchTellBusyWorkersUsageHigh,
		WatchTellDefaulted:            watchTellDefaulted,
		WatchTendFeedback:             watchTendFeedback,
		WatchUngated:                  watchUngated,
		WatchWorktrees:                watchWorktrees,
		WithPaceOffs:                  withPaceOffs,
	}
}

func (a *app) AlertCycle(ctx context.Context, store *alerts.Store, r *alerts.Reader) ([]string, []string) {
	return a.alertCycle(ctx, store, r)
}

func (a *app) AlertDiscovery(ctx context.Context) ([]string, []string, error) {
	return a.alertDiscovery(ctx)
}

func (a *app) AlertReader() *alerts.Reader { return a.alertReader() }

func (a *app) AlertRules() alerts.Rules { return a.alertRules() }

func (a *app) AppDrifts(ctx context.Context, records []state.App) map[string]string {
	return a.appDrifts(ctx, records)
}

func (a *app) BackgroundAgent(ctx context.Context, st *state.State, p state.Party) bool {
	return a.backgroundAgent(ctx, st, p)
}

func (a *app) CacheParkRefs(agents []state.Agent, refs map[github.PR]github.RefState) {
	a.cacheParkRefs(agents, refs)
}

func (a *app) CachedProvidersCapacityAt(k config.Capacity, now time.Time, providerErrs map[string]string) (config.Capacity, *capacity.UsageView) {
	return a.cachedProvidersCapacityAt(k, now, providerErrs)
}

func (a *app) CachedUsageReading(file string, u config.Usage, now time.Time, cause error) (capacity.UsageReading, error) {
	return a.cachedUsageReading(file, u, now, cause)
}

func (a *app) CentralNotes(ctx context.Context, me state.Party) ([]state.Note, error) {
	return a.centralNotes(ctx, me)
}

func (a *app) CentralNotesOn() bool { return a.centralNotesOn() }

func (a *app) Context() context.Context { return a.context() }

func (a *app) DesktopCap() int { return a.desktopCap() }

func (a *app) DesktopMode() bool { return a.desktopMode() }

func (a *app) DiskPathRoots() []string { return a.diskPathRoots() }

func (a *app) ExecutionClient(h execution.Host) execution.Transport { return a.executionClient(h) }

func (a *app) ExecutionHostRetirable(st *state.State, r *state.ExecutionRecord) bool {
	return a.executionHostRetirable(st, r)
}

func (a *app) HasRow(id string) bool { return a.hasRow(id) }

func (a *app) HasUnrecoveredPublication(r *state.ExecutionRecord) bool {
	return a.hasUnrecoveredPublication(r)
}

func (a *app) Hub() *central.Client { return a.hub() }

func (a *app) InPlay(now time.Time) func(string) bool { return a.inPlay(now) }

func (a *app) LeaseSettled(st *state.State, ag state.Agent, res string) (string, bool) {
	return a.leaseSettled(st, ag, res)
}

func (a *app) LiveCapacity() config.Capacity { return a.liveCapacity() }

func (a *app) LogQuiet(lines ...string) { a.logQuiet(lines...) }

func (a *app) LoopTimerChecks(ctx context.Context, timers []state.Timer, now time.Time, held bool) map[int]checkResult {
	return a.loopTimerChecks(ctx, timers, now, held)
}

func (a *app) PagerDutyCycle(ctx context.Context, store *alerts.Store, run central.Runner) []string {
	return a.pagerDutyCycle(ctx, store, run)
}

func (a *app) Paid(o state.Owed, why ...string) { a.paid(o, why...) }

func (a *app) PayOwed(ctx context.Context, o state.Owed) { a.payOwed(ctx, o) }

func (a *app) ReadHeadroom(ctx context.Context, k config.Capacity, swap *capacity.SwapReading) *capacity.Headroom {
	return a.readHeadroom(ctx, k, swap)
}

func (a *app) ReadSnapshot() (*state.State, error) { return a.readSnapshot() }

func (a *app) RearmRecovery() { a.rearmRecovery() }

func (a *app) RecheckLeaseParks(st *state.State, parks []settledPark) []settledPark {
	return a.recheckLeaseParks(st, parks)
}

func (a *app) ReconcileServeModes(ctx context.Context) error { return a.reconcileServeModes(ctx) }

func (a *app) RecordStoppedGates(ctx context.Context) { a.recordStoppedGates(ctx) }

func (a *app) Release(ctx context.Context, by state.Party, n *state.Note, how string) []string {
	return a.release(ctx, by, n, how)
}

func (a *app) RequireClaudeLogin() error { return a.requireClaudeLogin() }

func (a *app) ResolveReading(ctx context.Context, file string, u config.Usage) (capacity.UsageReading, error) {
	return a.resolveReading(ctx, file, u)
}

func (a *app) ResolveUsage(ctx context.Context, k config.Capacity) (config.Capacity, *capacity.UsageView) {
	return a.resolveUsage(ctx, k)
}

func (a *app) RetainUsageStop(file string, err error) { a.retainUsageStop(file, err) }

func (a *app) RetireExecution(ctx context.Context, ref execution.Ref, trigger string, now time.Time, skipHost bool) error {
	return a.retireExecution(ctx, ref, trigger, now, skipHost)
}

func (a *app) Rowless(ctx context.Context, st *state.State, turning func(context.Context, string) bool) []state.Agent {
	return a.rowless(ctx, st, turning)
}

func (a *app) ServeModeReadings(ctx context.Context, name string, p config.Provider) capacity.ProviderReadingResult {
	return a.serveModeReadings(ctx, name, p)
}

func (a *app) SetCtx(v context.Context) { a.ctx = v }

func (a *app) SettleOverrides(ctx context.Context, by state.Party, check func([]state.Timer) map[int]checkResult) ([]string, error) {
	return a.settleOverrides(ctx, by, check)
}

func (a *app) SlotView() (string, int) { return a.slotView() }

func (a *app) StopText(s *state.Stop) string { return a.stopText(s) }

func (a *app) SyncAgyQuotaFromState(st *state.State, now time.Time) { a.syncAgyQuotaFromState(st, now) }

func (a *app) SyncExecutionHost(ctx context.Context, selector string) error {
	return a.syncExecutionHost(ctx, selector)
}

func (a *app) UpgradeStatuses(ctx context.Context, st *state.State, now time.Time, rs upgrade.Readings, fresh func(installation string) time.Duration) []upgrade.Status {
	return a.upgradeStatuses(ctx, st, now, rs, fresh)
}

func (a *app) UseCodexRolloutUsage(u config.Usage, cached capacity.UsageReading) bool {
	return a.useCodexRolloutUsage(u, cached)
}

func (a *app) WakeAgent(ctx context.Context, by state.Party, q string, msg string, mode string) error {
	return a.wakeAgent(ctx, by, q, msg, mode)
}
