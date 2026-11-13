package cmd

import (
	"context"
	"time"

	"github.com/giantswarm/beekeeper/internal/gate"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/state"
)

// gateApp is the app behind a host; nil for none.
func gateApp(h gate.Host) *app {
	a, _ := h.(*app)
	return a
}

// The gate calls the command through Cmd.
func init() {
	gate.Cmd = gate.Command{
		BinaryVersion:    binaryVersion,
		Clock:            clock,
		DevctlCommandEnv: devctlCommandEnv,
		EndChild:         func(pid int) error { return endChild(pid) },
		Event:            event,
		HandOver:         handOver,
		HoldTarget:       holdTarget,
		LastLine:         lastLine,
		LastOf:           lastOf,
		MergeAuthority: func(ctx context.Context, a gate.Host, repo string, pr int, targetHead string) (string, bool, error) {
			return mergeAuthority(ctx, gateApp(a), repo, pr, targetHead)
		},
		NewExitError:          newExitError,
		OutliveCaller:         outliveCaller,
		OwnedBase:             ownedBase,
		ParseRef:              parseRef,
		Plat:                  func() platform.Platform { return plat },
		ReadBudget:            func(a gate.Host, ctx context.Context) (github.Budget, error) { return readBudget(gateApp(a), ctx) },
		ReadPID:               readPID,
		RecordDeliveryRelease: recordDeliveryRelease,
		RecordRun:             recordRun,
		ReplacedBinary:        replacedBinary,
		RunFailure:            runFailure,
		StoredGraphQL:         storedGraphQL,
		UntilText:             func(a gate.Host, h state.Hold) string { return untilText(gateApp(a), h) },
		UserSystemd:           func() bool { return userSystemd() },
		WakeID:                wakeID,
		WithInfra:             withInfra,
	}
}

func (a *app) As() string { return a.as }

func (a *app) CallCentralRaw(ctx context.Context, me state.Party, tool string, args map[string]any, v any) (string, error) {
	return a.callCentralRaw(ctx, me, tool, args, v)
}

func (a *app) CheckPlaces(ctx context.Context, by state.Party, now time.Time, lane string, repo string, pr int, every time.Duration) error {
	return a.checkPlaces(ctx, by, now, lane, repo, pr, every)
}

func (a *app) CheckRepositoryVisibility(ctx context.Context, repo string) error {
	return a.checkRepositoryVisibility(ctx, repo)
}

func (a *app) CloseToolWindow(ctx context.Context, by state.Party) { a.closeToolWindow(ctx, by) }

func (a *app) ExplicitConfig() string { return a.explicitConfig() }

func (a *app) InstallTool(ctx context.Context, by state.Party, verb string, what string) error {
	return a.installTool(ctx, by, verb, what)
}

func (a *app) LeaveCentral(ctx context.Context, merges []state.Merge, why string) {
	a.leaveCentral(ctx, merges, why)
}

func (a *app) MergeEvents(evs []state.Event) ([]state.Event, error) { return a.mergeEvents(evs) }

func (a *app) PollToolWindow(ctx context.Context, by state.Party) { a.pollToolWindow(ctx, by) }

func (a *app) Present(m state.Merge) bool { return a.present(m) }

func (a *app) RecordGone(ctx context.Context) ([]string, []string, []state.Merge) {
	return a.recordGone(ctx)
}

func (a *app) TeleportCause(ctx context.Context) string { return a.teleportCause(ctx) }
