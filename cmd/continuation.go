package cmd

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
)

const continuationQueuedEvent = "agents.continuation-queued"

// Role holders have a separate relay lifecycle. Workers hand their task to
// the loop rather than turning the transcript that happened to park it.
func freshPark(st *state.State, ag state.Agent) bool {
	return ag.Park != nil && ag.Task != "" && !ag.Done && !roleRun(st, ag.Party)
}

func (a *app) queueContinuation(by state.Party, ag state.Agent, message string) error {
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		i := slices.IndexFunc(st.Agents, func(cur state.Agent) bool { return cur.Is(ag.Party) })
		if i < 0 || st.Agents[i].Park == nil || !st.Agents[i].Park.At.Equal(ag.Park.At) {
			return nil, refused("%s changed its park before continuation delivery", ag.Name)
		}
		p := st.Agents[i].Park
		if slices.Contains(p.Continuation, message) {
			return nil, nil
		}
		if p.Continuing {
			return nil, refused("%s: fresh continuation is starting; retry event delivery", ag.Name)
		}
		if len(strings.Join(p.Continuation, "\n"))+len(message) > maxBrief/2 {
			return nil, refused("%s: continuation brief is full; event remains undelivered", ag.Name)
		}
		p.Continuation = append(p.Continuation, message)
		if st.Agents[i].Execution != nil {
			if r, _ := executionRecord(st, *st.Agents[i].Execution); r != nil {
				r.Completed = true
			}
		}
		return []state.Event{event(by, continuationQueuedEvent, "%s: %s", ag.Name, message)}, nil
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.out, "continuation: %s queued for a fresh worker; its old session stays parked\n", ag.Name)
	return err
}

// adoptContinuation gives an off-roster worker's durable task back to the
// loop. A role holder follows its own relay, and an anonymous park without
// task context is refused rather than loading an old transcript to find it.
func (a *app) adoptContinuation(by state.Party, p state.Park, message string) error {
	var ag state.Agent
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		if roleRun(st, p.By) {
			return nil, refused("role holders use their relay lifecycle")
		}
		if onRoster(st, p.By) {
			return nil, refused("worker rejoined the roster; retry delivery")
		}
		task := ""
		for _, start := range slices.Concat(st.Starts, st.EndedWorkers) {
			if start.Is(p.By) && start.Task != "" {
				task = start.Task
			}
		}
		if task == "" {
			for _, r := range st.Records {
				if r.Session.Is(p.By) && r.Ended.IsZero() && r.Issue != "" {
					task = "Continue " + r.Issue
					if ref, ok := parseRef(r.Issue); ok && !strings.HasPrefix(r.Issue, "https://") {
						task = "Continue " + issueURL(refName(ref))
					}
				}
			}
		}
		if task == "" {
			if ref, ok := parseRef(p.On); ok {
				task = "Continue " + issueURL(refName(ref))
			}
		}
		if task == "" {
			return nil, refused("%s has no recorded task reference for a fresh continuation", p.By.Name)
		}
		ag = state.Agent{Party: p.By, Task: task, Park: &p, Registered: a.now.UTC(), AssignedAt: p.At}
		st.Agents = append(st.Agents, ag)
		st.Parks = slices.DeleteFunc(st.Parks, func(cur state.Park) bool { return cur.By.Is(p.By) })
		return nil, nil
	})
	if err != nil {
		return err
	}
	return a.queueContinuation(by, ag, message)
}

func continuationName(ag state.Agent) string {
	id := sha256.Sum256([]byte(fmt.Sprintf("%s/%s/%s/%d", ag.Name, ag.Session, ag.HostSession, ag.Park.At.UnixNano())))
	return fmt.Sprintf("%s next-%x", truncate(ag.Name, 60), id[:8])
}

// continuedPark is the park of the worker whose fresh continuation the start
// named name is: the loop marks that park Continuing before it starts the
// session continuationName names. Nil for any other start.
func continuedPark(st *state.State, name string) *state.Park {
	for _, ag := range st.Agents {
		if ag.Park != nil && ag.Park.Continuing && continuationName(ag) == name {
			return ag.Park
		}
	}
	return nil
}

// continuesPull says whether the start named name continues a task whose
// delivery it keeps, and the pull request that joins that delivery: a parked
// worker's fresh continuation (continuedPark, with the pull it parked on) or
// the session the loop starts for a review park whose pull request ended
// (state.ReviewPark.Continuing, with that pull request).
func continuesPull(st *state.State, name string) (string, bool) {
	if p := continuedPark(st, name); p != nil {
		return parkedPull(p), true
	}
	for _, rp := range st.ReviewParks {
		if rp.Continuing != "" && strings.EqualFold(rp.Continuing, name) {
			if r, ok := parseRef(rp.On); ok {
				return refName(r), true
			}
			return "", true
		}
	}
	return "", false
}

// parkedPull is the pull request p waits on, owner/repo#n: "" when p waits
// on an issue, a note or nothing GitHub names.
func parkedPull(p *state.Park) string {
	if p == nil || p.RefKind != github.RefPullRequest && p.ReviewHead == "" {
		return ""
	}
	r, ok := parseRef(p.On)
	if !ok {
		return ""
	}
	return refName(r)
}

func loopFinishContinuation(l *loop.Loop, old state.Agent, name string) error {
	finished := false
	err := asApp(l.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		i := slices.IndexFunc(st.Agents, func(cur state.Agent) bool { return cur.Name == name })
		j := slices.IndexFunc(st.Agents, func(cur state.Agent) bool { return cur.Is(old.Party) })
		if i < 0 || j < 0 || st.Agents[i].Undelivered != "" {
			return nil, nil
		}
		next, prev := &st.Agents[i], st.Agents[j]
		if prev.Park == nil || !prev.Park.At.Equal(old.Park.At) {
			return nil, refused("%s changed its park during continuation startup", old.Name)
		}
		if next.Execution != nil {
			r, _ := executionRecord(st, *next.Execution)
			if r == nil || unactivated(r) {
				return nil, nil
			}
		}
		if !slices.Equal(prev.Park.Continuation, old.Park.Continuation) {
			return nil, refused("continuation changed during startup")
		}
		if !next.Done {
			next.Task, next.AssignedAt = prev.Task, prev.AssignedAt
		}
		for k := range st.Records {
			if st.Records[k].Session.Is(prev.Party) && st.Records[k].Ended.IsZero() {
				st.Records[k].Session = next.Party
				st.Records[k].Waits = ""
				if next.Done {
					st.Records[k].Ended = asApp(l.Host).now.UTC()
				}
			}
		}
		for k := range st.ReviewParks {
			if st.ReviewParks[k].Party.Is(prev.Party) {
				st.ReviewParks[k].Party, st.ReviewParks[k].Agent = next.Party, next.Name
				st.ReviewParks[k].Execution = next.Execution
			}
		}
		for k := range st.Timers {
			t := &st.Timers[k]
			if t.Wake == prev.Name || t.Wake == prev.Session && prev.Session != "" || t.Wake == prev.HostSession && prev.HostSession != "" {
				t.Wake = next.Name
			}
		}
		for k := range st.Owed {
			if st.Owed[k].Owner.Is(prev.Party) {
				st.Owed[k].Owner = next.Party
			}
		}
		for k := range st.Notes {
			if st.Notes[k].By.Is(prev.Party) {
				st.Notes[k].By = next.Party
			}
		}
		for k := range st.AlertOwners {
			if st.AlertOwners[k].By.Is(prev.Party) && st.AlertOwners[k].Ended.IsZero() {
				st.AlertOwners[k].By = next.Party
			}
		}
		for k := range st.Merges {
			if st.Merges[k].By.Is(prev.Party) {
				st.Merges[k].By = next.Party
			}
		}
		if _, err := handClaims(st, lease.Dir(asApp(l.Host).cfg.LeaseDir), prev.Party, next.Party); err != nil {
			return nil, err
		}
		e := event(loop.LoopParty, "agents.continuation", "%s: fresh worker %s; previous transcript stays parked", prev.Name, next.Name)
		successor := next.Party
		removeAgent(st, j)
		for k := range st.Starts {
			if st.Starts[k].Is(prev.Party) {
				st.Starts[k].Successor = &successor
			}
		}
		for k := range st.EndedWorkers {
			if st.EndedWorkers[k].Is(prev.Party) {
				st.EndedWorkers[k].Successor = &successor
			}
		}
		finished = true
		return []state.Event{e}, nil
	})
	if err == nil && finished {
		if h, id, ok := harnessOf(old.Party); ok {
			_ = plat.Launcher.Stop(context.Background(), h.Unit(id))
		}
		_, _ = fmt.Fprintf(asApp(l.Host).out, "SUPERVISOR CONTINUATION %s: fresh worker %s\n", old.Name, name)
	}
	return err
}
