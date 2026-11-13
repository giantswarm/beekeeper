package cmd

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
)

const throughputOutcomeVerb = "supervisor.operation-outcome"
const throughputHealthVerb = "supervisor.throughput-health"
const throughputObservationFile = "throughput-observation.json"
const throughputHealthFile = "throughput-health.json"
const throughputCadence = time.Minute
const throughputHealthy = "healthy"
const throughputStalledMerge = "stalled-merge"
const watchThroughputKey = "throughput"

type throughputOutcome struct {
	At                                time.Time
	Operation, Reason, Candidate, URL string
	Success                           bool
	Unknown                           bool
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
	if len(parts) != 4 || parts[2] != loop.LoopPullStep || parts[3] == "" {
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
	o := throughputOutcome{At: a.timeNow(), Operation: operation, Reason: reason, Candidate: loop.NormalizeLoopVersion(loop.LoopRunningVersion()), URL: publicPullURL(rawURL), Success: success}
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	return a.store.Record(event(loop.LoopParty, throughputOutcomeVerb, "%s", b))
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
		candidate = UnknownState
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
		status = UnknownState
	}
	slices.Sort(urls)
	return status, fmt.Sprintf("release throughput %s; candidate %s; oldest eligible wait %s; public PRs: %s", status, candidate, oldest.Round(time.Second), strings.Join(urls, ", "))
}

// Both watches share this durable incident boundary. The state lock serializes
// evaluation and guide filing so concurrent/restarted watches cannot duplicate it.
func (a *app) checkThroughput(now time.Time, held bool, progress ...func()) (string, error) {
	if a.store == nil {
		return "", fmt.Errorf("durable health unavailable")
	}
	var prev throughputHealth
	if _, err := a.store.ReadFile(throughputHealthFile, &prev); err != nil {
		return "", err
	}
	if !prev.At.IsZero() && !now.Before(prev.At) && now.Sub(prev.At) < throughputCadence {
		return "", nil
	}
	keepHistory := func(e state.Event) bool { return e.Verb == throughputOutcomeVerb || e.Verb == throughputHealthVerb }
	if len(progress) > 0 {
		keepHistory = loop.EventProgressFilter(keepHistory, progress[0], clk.Now)
	}
	// Warm a cold reader before taking the state lock. Refresh it inside
	// the transaction as well to include concurrent health transitions.
	if _, err := state.FollowSnapshot(a.store, "throughput-health-history", "v1", keepHistory); err != nil {
		return "", err
	}
	st, err := a.store.Read()
	if err != nil {
		return "", err
	}
	var line string
	step := func(st *state.State, commit bool) ([]state.Event, error) {
		line = ""
		var prev throughputHealth
		if _, err := a.store.ReadFile(throughputHealthFile, &prev); err != nil {
			return nil, err
		}
		if !prev.At.IsZero() && !now.Before(prev.At) && now.Sub(prev.At) < throughputCadence {
			return nil, nil
		}
		var obs throughputObservation
		found, err := a.store.ReadFile(throughputObservationFile, &obs)
		// A phase checkpoint can precede the first planning observation. It
		// establishes liveness, not a throughput incident on an otherwise idle
		// desk. Keep stale observations and retained eligibility diagnostic.
		freshCheckpoint := obs.Unknown && len(obs.Eligible) == 0 && !obs.At.IsZero() &&
			now.Sub(obs.At) >= -throughputCadence && now.Sub(obs.At) <= 3*throughputCadence
		initializing := err == nil && prev.At.IsZero() && (!found || freshCheckpoint)
		if err != nil || !found {
			obs.Unknown = true
		}
		evs, err := state.FollowSnapshot(a.store, "throughput-health-history", "v1", keepHistory)
		if err != nil {
			return nil, err
		}
		if initializing && len(evs) == 0 {
			if commit {
				return nil, a.store.WriteFile(throughputHealthFile, throughputHealth{At: now, Status: UnknownState})
			}
			return nil, nil
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
		status, detail := evaluateThroughput(now, after, obs, outcomes, held || st.Stop != nil || a.cfg.GitHub.Floor > 0 && loop.LoopBudgetUnknownOrLow(st.Budget, a.cfg.GitHub.Floor, now))
		if status == prev.Status || prev.Status == "" && status == throughputHealthy {
			if commit {
				return nil, a.store.WriteFile(throughputHealthFile, throughputHealth{At: now, Status: status})
			}
			return nil, nil
		}
		if commit {
			if err := a.store.WriteFile(throughputHealthFile, throughputHealth{At: now, Status: status}); err != nil {
				return nil, err
			}
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
			report := fmt.Sprintf("%s reported at %s%s%s", loop.LoopParty.Name, now.UTC().Format(time.RFC3339), loop.FileReportSeparator, detail)
			events = append(events, event(loop.LoopParty, loop.LoopFileVerb, "[%s] %s", key, report))
		}
		return events, nil
	}
	events, err := step(st, false)
	if err != nil {
		return "", err
	}
	if len(events) == 0 {
		var needUpdate bool
		if err := a.store.Exclusive(func() error {
			evs, err := step(st, false)
			if err != nil {
				return err
			}
			if len(evs) > 0 {
				needUpdate = true
				return nil
			}
			_, err = step(st, true)
			return err
		}); err != nil || !needUpdate {
			return line, err
		}
	}
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		return step(st, true)
	})
	return line, err
}
