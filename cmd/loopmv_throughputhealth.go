package cmd

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const ThroughputStartFailed = "start-command-failed"

type eligibleWait struct {
	Head, URL string
	Since     time.Time
}

type throughputObservation struct {
	Candidate string
	At        time.Time
	Unknown   bool
	Eligible  map[string]eligibleWait
}

// Keep retry/backoff decisions out of this ledger. Only a command actually
// attempted contributes; capacity and budget refusals are intentional holds.
func (l *Loop) RunStart(ctx context.Context, act superloop.Action, args ...string) (string, error) {
	begin := l.Host.timeNow()
	out, err := l.Run(ctx, args...)
	if err != nil && (errors.Is(err, context.Canceled) || Code(err) == ExitRefused || isLoopGitHubHeldOutput(out)) {
		return out, err
	}
	// A start cut off after it registered its session (killed while it
	// waited for the state lock: exit -1) did start: its roster entry is
	// the outcome, not the child's exit status. Registration precedes
	// launch, so also require the child's confirmation that startup finished.
	// Otherwise a failed launch with a retained roster entry looks successful.
	if err != nil && len(args) >= 3 && strings.Contains("\n"+out, "\nstarted "+args[2]+":") && l.startRegistered(args[2], begin) {
		err = nil
	}
	reason := ThroughputStartFailed
	if err == nil {
		reason = ""
	}
	if saveErr := l.Host.operationOutcome(agentStartName, reason, act.URL, err == nil); saveErr != nil {
		return out, saveErr
	}
	return out, err
}

// startRegistered says whether the roster holds name, registered since begin.
func (l *Loop) startRegistered(name string, begin time.Time) bool {
	st, err := l.Host.store.Read()
	if err != nil {
		return false
	}
	return slices.ContainsFunc(st.Agents, func(ag state.Agent) bool {
		return strings.EqualFold(ag.Name, name) && !ag.Registered.Before(begin)
	})
}

// Phase progress refreshes liveness without invalidating the last planner
// observation before the next pass's health check reads it. The planner's
// checkpoint below still marks eligibility unknown while gathering inputs.
func (l *Loop) refreshThroughputHeartbeat(now time.Time) error {
	var obs throughputObservation
	found, err := l.Host.store.ReadFile(throughputObservationFile, &obs)
	if err != nil {
		return err
	}
	candidate := NormalizeLoopVersion(LoopRunningVersion())
	if !found || obs.Candidate != candidate {
		obs.Unknown = true
	}
	obs.At, obs.Candidate = now, candidate
	return l.Host.store.WriteFile(throughputObservationFile, obs)
}

// CheckpointThroughput reports bounded pass progress before expensive history
// loading. Keep eligibility ages intact until planning can observe them afresh.
// Use wall time: the pass snapshot can already be minutes old after handover.
// This is not a background keepalive; a stuck pass stops refreshing it.
func (l *Loop) CheckpointThroughput(now time.Time) error {
	var obs throughputObservation
	if _, err := l.Host.store.ReadFile(throughputObservationFile, &obs); err != nil {
		return err
	}
	obs.At, obs.Candidate, obs.Unknown = now, NormalizeLoopVersion(LoopRunningVersion()), true
	return l.Host.store.WriteFile(throughputObservationFile, obs)
}

func (l *Loop) ObserveThroughput(in superloop.Input, actions []superloop.Action, unknown bool) error {
	var old throughputObservation
	_, err := l.Host.store.ReadFile(throughputObservationFile, &old)
	if err != nil {
		old = throughputObservation{}
		unknown = true
	}
	now := l.Host.timeNow()
	next := throughputObservation{At: now, Candidate: NormalizeLoopVersion(LoopRunningVersion()), Unknown: unknown, Eligible: map[string]eligibleWait{}}
	if l.Lifecycle != nil {
		// Liveness uses completion time, not the frozen planning snapshot.
		next.At = clk.Now().UTC()
	}
	if unknown {
		for ref, wait := range old.Eligible {
			if _, observed := in.Pulls[ref]; !observed {
				next.Eligible[ref] = wait
			}
		}
	}
	for _, act := range actions {
		p := in.Pulls[act.Ref]
		if act.Kind != superloop.KindGo || !p.Green || p.Head != act.Head {
			continue
		}
		// Plan applies ownership, review provenance, holds and merge policy. Only
		// exact-head approvals count; a base-only update is deliberately excluded.
		approved := false
		for _, r := range p.Reviews {
			if r.Head == p.Head && r.Verdict == superloop.Approved {
				approved = true
			}
		}
		if !approved || p.Draft || strings.EqualFold(p.Mergeable, MergeableConflicting) || strings.EqualFold(p.MergeState, github.DirtyState) {
			continue
		}
		wait := eligibleWait{Head: p.Head, URL: publicPullURL(p.URL), Since: now}
		if prev, ok := old.Eligible[act.Ref]; ok && prev.Head == wait.Head {
			wait.Since = prev.Since
		}
		next.Eligible[act.Ref] = wait
	}
	return l.Host.store.WriteFile(throughputObservationFile, next)
}
