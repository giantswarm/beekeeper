package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
)

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
		return []state.Event{event(by, "agents.continuation-queued", "%s: %s", ag.Name, message)}, nil
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
		for _, r := range st.Records {
			if r.Session.Is(p.By) && r.Ended.IsZero() && r.Issue != "" {
				task = "Continue " + issueURL(r.Issue)
			}
		}
		if task == "" {
			if _, ok := parseRef(p.On); ok {
				task = "Continue " + issueURL(p.On)
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

const freshContinuationBrief = `Continue the existing task for {{.URL}} in {{.Repo}}.

Task: {{.Task}}

{{.Context}}

Read the issue and its acceptance criteria, the PR and current head, and repository rules.
Re-query live state before acting. Continue the existing branch in your own worktree;
never create a competing PR or resume the previous worker's transcript. Preserve
all remaining release, rollout and proof obligations. CI owns tests, lints and builds.
Push fixes once per round, then park from the worktree for desk review. The loop
owns merges. Finish only after acceptance is proven; report unresolved barriers.
`

// continueParks consumes durable events only after a fresh start is accepted.
// A stable replacement name makes a coordinator restart after launch idempotent.
// Failed placement or delivery leaves the task and all its events parked.
func (l *supervisorLoop) continueParks(ctx context.Context, st *state.State) error {
	if st.Stop != nil || l.holdingStarts() {
		return nil
	}
	for _, ag := range st.Agents {
		if !freshPark(st, ag) || len(ag.Park.Continuation) == 0 {
			continue
		}
		ref := agentRef(st, ag.Name)
		if _, ok := parseRef(ag.Park.On); ok {
			ref = ag.Park.On
		}
		r, ok := parseRef(ref)
		if !ok || l.agentUnreadRepo(st, ag, ref) != "" {
			continue
		}
		if l.dryRun {
			l.sayOnce("continue "+ag.Name, "SUPERVISOR WOULD CONTINUE %s with a fresh worker for %s", ag.Name, ref)
			continue
		}
		name := fmt.Sprintf("%s next-%x", truncate(ag.Name, 80), ag.Park.At.UnixNano())
		current, err := l.a.store.Read()
		if err != nil {
			return err
		}
		i := slices.IndexFunc(current.Agents, func(cur state.Agent) bool { return cur.Name == name })
		if i < 0 {
			place, err := l.startPlacement(ctx, name, r.Repo, false, false)
			if err != nil {
				l.sayOnce("continue capacity "+ag.Name, "SUPERVISOR CONTINUATION WAITS %s: %v", ag.Name, err)
				continue
			}
			url := issueURL(ref)
			branch, head := "", ag.Park.ReviewHead
			if ag.Park.RefKind == "PullRequest" || head != "" {
				pull, err := readLoopPull(ctx, ref)
				if err != nil {
					continue
				}
				url, branch, head = pull.URL, pull.Branch, pull.Head
				if url == "" {
					url = fmt.Sprintf("https://github.com/%s/pull/%d", r.Repo, r.N)
				}
				if place.remote() && branch != "" {
					place.Ref = branch
				}
			}

			context := fmt.Sprintf("Previous wait: %s\nReference: %s\nPublished head: %s\nEvents:\n%s", ag.Park.Waits, ref, head, strings.Join(ag.Park.Continuation, "\n"))
			context += "\nExisting branch: " + branch
			if ag.Park.PostMerge != nil {
				context += "\nRemaining post-merge work: " + *ag.Park.PostMerge
			}
			brief, err := l.brief("", freshContinuationBrief, "continuation "+name, briefData{URL: url, Repo: r.Repo, Number: r.N, Task: ag.Task, Context: context})
			if err != nil {
				return err
			}
			if err := l.incidentStart(ref); err != nil {
				continue
			}
			args := []string{agentsName, agentStartName, name, brief, "--harness", harnessAuto}
			args = append(args, place.remoteStartFlags()...)
			args = append(args, place.excludeFlags()...)
			if err := l.a.store.Update(func(st *state.State) ([]state.Event, error) {
				for k := range st.Agents {
					cur := &st.Agents[k]
					if cur.Is(ag.Party) && cur.Park != nil && cur.Park.At.Equal(ag.Park.At) && slices.Equal(cur.Park.Continuation, ag.Park.Continuation) {
						cur.Park.Continuing = true
						return nil, nil
					}
				}
				return nil, refused("continuation changed before startup")
			}); err != nil {
				continue
			}
			if _, err := l.run(ctx, args...); err != nil {
				_ = l.a.store.Update(func(st *state.State) ([]state.Event, error) {
					if !slices.ContainsFunc(st.Agents, func(cur state.Agent) bool { return cur.Name == name }) {
						for k := range st.Agents {
							if st.Agents[k].Is(ag.Party) && st.Agents[k].Park != nil {
								st.Agents[k].Park.Continuing = false
							}
						}
					}
					return nil, nil
				})
				l.sayOnce("continue start "+ag.Name, "SUPERVISOR CONTINUATION WAITS %s: %v", ag.Name, err)
				continue
			}
		}
		if i >= 0 && current.Agents[i].Execution != nil {
			if rec, _ := executionRecord(current, *current.Agents[i].Execution); rec != nil && unactivated(rec) {
				if _, err := l.remoteStartRefused(ctx, name); err != nil {
					continue
				}
			}
		}
		if err := l.finishContinuation(ag, name); err != nil {
			return err
		}
	}
	return nil
}

func (l *supervisorLoop) finishContinuation(old state.Agent, name string) error {
	return l.a.store.Update(func(st *state.State) ([]state.Event, error) {
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
		next.Task, next.AssignedAt = prev.Task, prev.AssignedAt
		for k := range st.Records {
			if st.Records[k].Session.Is(prev.Party) && st.Records[k].Ended.IsZero() {
				st.Records[k].Session = next.Party
				st.Records[k].Waits = ""
			}
		}
		for k := range st.ReviewParks {
			if st.ReviewParks[k].Party.Is(prev.Party) {
				st.ReviewParks[k].Party, st.ReviewParks[k].Agent = next.Party, next.Name
				st.ReviewParks[k].Execution = next.Execution
			}
		}
		if _, err := handClaims(st, lease.Dir(l.a.cfg.LeaseDir), prev.Party, next.Party); err != nil {
			return nil, err
		}
		e := event(loopParty, "agents.continuation", "%s: fresh worker %s; previous transcript stays parked", prev.Name, next.Name)
		removeAgent(st, j)
		return []state.Event{e}, nil
	})
}
