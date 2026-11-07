package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const throughputOutcomeVerb = "supervisor.operation-outcome"
const throughputHealthVerb = "supervisor.throughput-health"
const throughputObservationFile = "throughput-observation.json"
const throughputHealthFile = "throughput-health.json"
const throughputCadence = time.Minute
const throughputHealthy = "healthy"
const throughputStalledMerge = "stalled-merge"
const throughputStartFailed = "start-command-failed"
const watchThroughputKey = "throughput"

type throughputOutcome struct {
	At                                time.Time
	Operation, Reason, Candidate, URL string
	Success                           bool
	Unknown                           bool
}
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
type throughputHealth struct {
	At       time.Time
	Status   string
	Incident string
}

// Public diagnostics contain only public GitHub PR URLs, never command output.
func publicPullURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != loopPullStep || parts[3] == "" {
		return ""
	}
	for _, c := range parts[3] {
		if c < '0' || c > '9' {
			return ""
		}
	}
	return "https://github.com/" + strings.Join(parts, "/")
}

func (a *app) operationOutcome(operation, reason, rawURL string, success bool) error {
	o := throughputOutcome{At: a.timeNow(), Operation: operation, Reason: reason, Candidate: normalizeLoopVersion(loopRunningVersion()), URL: publicPullURL(rawURL), Success: success}
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	return a.store.Record(event(loopParty, throughputOutcomeVerb, "%s", b))
}

// Keep retry/backoff decisions out of this ledger. Only a command actually
// attempted contributes; capacity and budget refusals are intentional holds.
func (l *supervisorLoop) runStart(ctx context.Context, act superloop.Action, args ...string) (string, error) {
	begin := l.a.timeNow()
	out, err := l.run(ctx, args...)
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
	reason := throughputStartFailed
	if err == nil {
		reason = ""
	}
	if saveErr := l.a.operationOutcome(agentStartName, reason, act.URL, err == nil); saveErr != nil {
		return out, saveErr
	}
	return out, err
}

// startRegistered says whether the roster holds name, registered since begin.
func (l *supervisorLoop) startRegistered(name string, begin time.Time) bool {
	st, err := l.a.store.Read()
	if err != nil {
		return false
	}
	return slices.ContainsFunc(st.Agents, func(ag state.Agent) bool {
		return strings.EqualFold(ag.Name, name) && !ag.Registered.Before(begin)
	})
}

func (l *supervisorLoop) observeThroughput(in superloop.Input, actions []superloop.Action, unknown bool) error {
	var old throughputObservation
	_, err := l.a.store.ReadFile(throughputObservationFile, &old)
	if err != nil {
		old = throughputObservation{}
		unknown = true
	}
	now := l.a.timeNow()
	next := throughputObservation{At: now, Candidate: normalizeLoopVersion(loopRunningVersion()), Unknown: unknown, Eligible: map[string]eligibleWait{}}
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
		if !approved || p.Draft || strings.EqualFold(p.Mergeable, mergeableConflicting) || strings.EqualFold(p.MergeState, github.DirtyState) {
			continue
		}
		wait := eligibleWait{Head: p.Head, URL: publicPullURL(p.URL), Since: now}
		if prev, ok := old.Eligible[act.Ref]; ok && prev.Head == wait.Head {
			wait.Since = prev.Since
		}
		next.Eligible[act.Ref] = wait
	}
	return l.a.store.WriteFile(throughputObservationFile, next)
}

// evaluateThroughput is deterministic and reconstructs operation streaks from
// durable events, including successes between failures on different PRs.
func evaluateThroughput(now time.Time, after time.Duration, obs throughputObservation, outcomes []throughputOutcome, held bool) (string, string) {
	type streak struct {
		reason string
		count  int
		urls   []string
	}
	streaks := map[string]streak{}
	unknown := map[string]bool{}
	candidate := obs.Candidate
	if candidate == "" {
		candidate = unknownState
	}
	urls := []string{}
	for _, o := range outcomes {
		unknown[o.Operation] = o.Unknown
		if o.Unknown {
			continue
		}
		if o.Candidate != "" {
			candidate = o.Candidate
		}
		s := streaks[o.Operation]
		if o.Success {
			s = streak{}
		} else if s.reason == o.Reason {
			s.count++
		} else {
			s = streak{reason: o.Reason, count: 1}
		}
		if !o.Success {
			if u := publicPullURL(o.URL); u != "" && !slices.Contains(s.urls, u) {
				s.urls = append(s.urls, u)
			}
		}
		streaks[o.Operation] = s
	}
	status := throughputHealthy
	oldest := time.Duration(0)
	if !held {
		for _, op := range []string{runMerge, agentStartName} {
			if s := streaks[op]; s.count >= 3 {
				if status == throughputHealthy {
					status = "failed-" + op + "-" + s.reason
				}
				urls = append(urls, s.urls...)
			}
		}
		for _, wait := range obs.Eligible {
			if wait.Head == "" || wait.Since.IsZero() || wait.Since.After(now) {
				obs.Unknown = true
				continue
			}
			oldest = max(oldest, now.Sub(wait.Since))
			if u := publicPullURL(wait.URL); u != "" && !slices.Contains(urls, u) {
				urls = append(urls, u)
			}
		}
		if status == throughputHealthy && oldest >= after {
			status = throughputStalledMerge
		}
	}
	if unknown[runMerge] || unknown[agentStartName] || obs.Unknown || obs.At.IsZero() || now.Sub(obs.At) > 3*throughputCadence || obs.At.After(now) {
		status = unknownState
	}
	slices.Sort(urls)
	return status, fmt.Sprintf("release throughput %s; candidate %s; oldest eligible wait %s; public PRs: %s", status, candidate, oldest.Round(time.Second), strings.Join(urls, ", "))
}

// Both watches share this durable incident boundary. The state lock serializes
// evaluation and guide filing so concurrent/restarted watches cannot duplicate it.
func (a *app) checkThroughput(now time.Time, held bool) (string, error) {
	if a.store == nil {
		return "", fmt.Errorf("durable health unavailable")
	}
	var line string
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		var prev throughputHealth
		if _, err := a.store.ReadFile(throughputHealthFile, &prev); err != nil {
			return nil, err
		}
		if !prev.At.IsZero() && !now.Before(prev.At) && now.Sub(prev.At) < throughputCadence {
			return nil, nil
		}
		var obs throughputObservation
		found, err := a.store.ReadFile(throughputObservationFile, &obs)
		initializing := !found && err == nil && prev.At.IsZero()
		if err != nil || !found {
			obs.Unknown = true
		}
		evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == throughputOutcomeVerb || e.Verb == throughputHealthVerb })
		if err != nil {
			return nil, err
		}
		if initializing && len(evs) == 0 {
			return nil, a.store.WriteFile(throughputHealthFile, throughputHealth{At: now, Status: unknownState})
		}
		var outcomes []throughputOutcome
		prev.Status = ""
		prev.Incident = ""
		for _, e := range evs {
			if e.Verb == throughputHealthVerb {
				var saved throughputHealth
				if json.Unmarshal([]byte(e.Detail), &saved) == nil {
					prev.Status = saved.Status
					prev.Incident = saved.Incident
				}
				continue
			}
			var o throughputOutcome
			if json.Unmarshal([]byte(e.Detail), &o) != nil || o.At.IsZero() || (o.Operation != runMerge && o.Operation != agentStartName) || (!o.Success && !o.Unknown && o.Reason == "") {
				obs.Unknown = true
				continue
			}
			outcomes = append(outcomes, o)
		}
		after := a.cfg.Watch.ThroughputAfter.Duration
		if after <= 0 {
			after = 2 * time.Hour
		}
		status, detail := evaluateThroughput(now, after, obs, outcomes, held || st.Stop != nil || a.cfg.GitHub.Floor > 0 && loopBudgetUnknownOrLow(st.Budget, a.cfg.GitHub.Floor, now))
		if err := a.store.WriteFile(throughputHealthFile, throughputHealth{At: now, Status: status}); err != nil {
			return nil, err
		}
		if status == prev.Status || prev.Status == "" && status == throughputHealthy {
			return nil, nil
		}
		incident := prev.Incident
		alert := strings.HasPrefix(status, "failed-") || status == throughputStalledMerge
		report := alert && incident != status || status == throughputHealthy && incident != ""
		if alert {
			incident = status
		}
		if status == throughputHealthy {
			incident = ""
			detail = "RECOVERED: " + detail
		}
		line = detail
		saved, _ := json.Marshal(throughputHealth{At: now, Status: status, Incident: incident})
		events := []state.Event{event(watchParty, throughputHealthVerb, "%s", saved)}
		if report {
			key := "throughput " + now.UTC().Format(time.RFC3339Nano)
			report := fmt.Sprintf("%s reported at %s%s%s", loopParty.Name, now.UTC().Format(time.RFC3339), fileReportSeparator, detail)
			events = append(events, event(loopParty, loopFileVerb, "[%s] %s", key, report))
		}
		return events, nil
	})
	return line, err
}
