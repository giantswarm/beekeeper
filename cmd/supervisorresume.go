package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

var loopResumePark = (*app).resumeParked

// recoverQuota re-queues the task of any local agy worker or reviewer whose
// run ended with a quota error on another choice. Its bucket is marked full
// until reset, which goneAgent already frees the slot for. The entry and its
// quota marker stay until a replacement started: a held, refused or failed
// start leaves them for the next pass, so no task is lost.
func (l *supervisorLoop) recoverQuota(ctx context.Context, st *state.State) error {
	if st.Stop != nil {
		return nil
	}
	for _, ag := range st.Agents {
		if ag.Done || ag.Task == "" || ag.Execution != nil || roleRun(st, ag.Party) || ag.Park != nil && len(ag.Park.Continuation) > 0 {
			continue
		}
		qerr := agyQuotaError(l.a.cfg, ag)
		if qerr == nil {
			continue
		}
		ref := cmp.Or(loopServes(st, ag), agentRef(st, ag.Name))
		if l.agentUnreadRepo(st, ag, ref) != "" {
			continue
		}
		target := cmp.Or(ref, ag.Task)
		if stop, err := l.stopPassForReexec(ctx); stop || err != nil {
			return err
		}
		l.lifecyclePhase(loopPhaseRecover)
		if l.dryRun {
			if _, err := fmt.Fprintf(l.a.out, "SUPERVISOR WOULD QUOTA %s: %s; free slot and re-queue %s on another choice\n", ag.Name, qerr.Error(), target); err != nil {
				return err
			}
			continue
		}
		startID := agyAgentStartID(ag)
		_, model := agentHarnessModel(st, nil, ag)
		l.a.recordAgyQuotaExhaustedForID(startID, model, qerr, l.a.now)
		if l.startsHeld {
			l.sayOnce("quota held "+startID, "SUPERVISOR QUOTA %s: %s; slot freed, starts are held: %s waits for its re-queue", ag.Name, qerr.Error(), target)
			continue
		}
		review, err := l.quotaReview(st, ag)
		if err != nil {
			return err
		}
		if err := l.restartQuota(ctx, st, ag, ref, review); err != nil {
			if errors.Is(err, state.ErrRecoveryWriterFenced) {
				return err
			}
			if errors.Is(err, errLoopLater) {
				l.sayOnce("quota later "+startID, "SUPERVISOR QUOTA %s: %s; slot freed, no room for its replacement yet: %s waits for its re-queue", ag.Name, qerr.Error(), target)
				continue
			}
			l.sayOnce("quota failed "+startID, "SUPERVISOR QUOTA FAILED %s: %s; %s stays on the roster: %v", ag.Name, qerr.Error(), target, err)
			continue
		}
		note, _, _ := l.a.lastNote(ag.Party, time.Time{}, func() { l.lifecyclePhase(loopPhaseRecover) })
		l.lifecyclePhase(loopPhaseRecover)
		err = l.a.store.Update(func(st *state.State) ([]state.Event, error) {
			// The replacement took the name, and with it the task; it
			// serves what the old session served.
			var next *state.Agent
			for i := range st.Agents {
				if st.Agents[i].Name == ag.Name && !st.Agents[i].Is(ag.Party) {
					next = &st.Agents[i]
				}
			}
			for j := range st.Records {
				if !st.Records[j].Session.Is(ag.Party) {
					continue
				}
				if next != nil && st.Records[j].Ended.IsZero() {
					st.Records[j].Session = next.Party
					continue
				}
				st.Records[j].Ended, st.Records[j].Waits = l.a.now.UTC(), ""
			}
			if i := slices.IndexFunc(st.Agents, func(x state.Agent) bool { return x.Is(ag.Party) }); i >= 0 {
				removeAgent(st, i)
			}
			return []state.Event{
				event(loopParty, "agents.gone", "%s: %s; task: %s; hand-over: %s", ag.Name, qerr.Error(), ag.Task, note),
				event(loopParty, "supervisor.quota", "%s: %s; slot freed, task %s re-queued", ag.Name, qerr.Error(), target),
			}, nil
		})
		if err != nil {
			return err
		}
		if startID != "" {
			_ = l.a.endAgy(ctx, startID)
		}
		l.fillPause = fillPause{}
		l.fillEmpty = 0
		l.boardNone = nil
		if _, err := fmt.Fprintf(l.a.out, "SUPERVISOR QUOTA %s: %s; slot freed, re-queuing %s on another choice\n", ag.Name, qerr.Error(), target); err != nil {
			return err
		}
	}
	return nil
}

// quotaRound is the review round the loop started a reviewer for: the key
// of its last claim under the reviewer's name ("review <ref>@<head>"), its
// ref and round, and whether the work it reviews is a self-hosted model's.
type quotaRound struct {
	key, ref   string
	round      int
	selfHosted bool
}

// quotaReview reads the round of ag when it is a reviewer the loop
// started, from the last claim the loop logged under ag's name; nil for
// any other session.
func (l *supervisorLoop) quotaReview(st *state.State, ag state.Agent) (*quotaRound, error) {
	evs, err := state.FollowSnapshot(l.a.store, "supervisor-review-quota", "v1", l.eventHistoryFilter(func(e state.Event) bool { return e.By.Name == loopParty.Name && e.Verb == loopClaimVerb }))
	if err != nil {
		return nil, err
	}
	for i := len(evs) - 1; i >= 0; i-- {
		key, name, ok := keyed(evs[i].Detail)
		if !ok || name != ag.Name {
			continue
		}
		rest, ok := strings.CutPrefix(key, "review ")
		if !ok {
			return nil, nil
		}
		ref, _, _ := strings.Cut(rest, "@")
		// A pull request has no cap on its review rounds: the name says which.
		digits, ok := strings.CutPrefix(ag.Name, strings.TrimSuffix(superloop.ReviewerName(ref, 0), "0"))
		if !ok {
			return nil, nil
		}
		rd, err := strconv.Atoi(digits)
		if err != nil || rd < 1 || superloop.ReviewerName(ref, rd) != ag.Name {
			return nil, nil
		}
		return &quotaRound{key: key, ref: ref, round: rd, selfHosted: reviewedSelfHosted(l.a.cfg.Capacity, st, ref)}, nil
	}
	return nil, nil
}

// reviewedSelfHosted says whether the work under review at ref is a
// self-hosted model's, as the loop's plan decides it; a worker no longer
// found counts as one, so the replacement review stays on a frontier
// model.
func reviewedSelfHosted(k config.Capacity, st *state.State, ref string) bool {
	for _, ag := range st.Agents {
		if ag.Park != nil && strings.EqualFold(loopParkOn(ag.Park), ref) {
			return selfHostedModel(k, ag.Model)
		}
	}
	for _, rp := range st.ReviewParks {
		if strings.EqualFold(rp.On, ref) {
			return selfHostedModel(k, rp.Model)
		}
	}
	return true
}

// restartQuota starts the replacement of ag under its name, placed as the
// loop places its starts: a reviewer with the brief of its round and the
// review flags, a worker with supervisor.workerBrief and its old task.
// errLoopLater says there is no room for it yet.
func (l *supervisorLoop) restartQuota(ctx context.Context, st *state.State, ag state.Agent, ref string, review *quotaRound) error {
	if review != nil {
		ref = review.ref
	}
	r, ok := parseRef(ref)
	if !ok {
		return fmt.Errorf("no task reference")
	}
	if l.agentUnreadRepo(st, ag, ref) != "" {
		return errLoopLater
	}
	if (review != nil || superloop.IsPull(ref) && ag.Park != nil && ag.Park.RefKind == github.RefPullRequest || strings.Contains(strings.ToLower(ref), "/pull/")) && !l.liveOpen(ctx, ref) {
		return errLoopLater
	}
	var brief string
	if review != nil {
		brief = filepath.Join(l.a.store.Dir(), "supervisor-loop", briefName.ReplaceAllString(review.key, "-")+".md")
		if _, err := os.Stat(brief); err != nil {
			return fmt.Errorf("the brief of review round %d: %w", review.round, err)
		}
	} else {
		if l.a.cfg.Supervisor.WorkerBrief == "" {
			return fmt.Errorf("no supervisor.workerBrief")
		}
		var err error
		brief, err = l.brief(l.a.cfg.Supervisor.WorkerBrief, "", "quota "+ag.Name, briefData{URL: fmt.Sprintf("https://github.com/%s/issues/%d", r.Repo, r.N), Repo: r.Repo, Number: r.N, Worker: ag.Name})
		if err != nil {
			return err
		}
		if err := appendBrief(brief, fmt.Sprintf("\nContinue your existing task: %s\nIts previous session stopped on a quota error. Re-query its live state before continuing.\n", ag.Task)); err != nil {
			return err
		}
	}
	place, err := l.startPlacement(ctx, ag.Name, r.Repo, review != nil, review != nil && review.selfHosted)
	if err != nil {
		return err
	}
	args := []string{agentsName, agentStartName, ag.Name, brief}
	if review != nil {
		args = append(args, "--"+reviewFlag)
	}
	if len(l.a.cfg.Capacity.Choices) > 0 {
		args = append(args, "--harness", harnessAuto)
		if review != nil && review.selfHosted {
			args = append(args, "--"+reviewOfSelfHostedFlag)
		}
	}
	args = append(args, place.remoteStartFlags()...)
	args = append(args, place.excludeFlags()...)
	if _, err := l.run(ctx, args...); err != nil {
		if Code(err) == ExitRefused {
			if place.remote() {
				if _, rerr := l.remoteStartRefused(ctx, ag.Name); rerr == nil {
					return nil
				}
			}
			return errLoopLater
		}
		return err
	}
	return nil
}

// appendBrief adds text to the brief file at path.
func appendBrief(path, text string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // the brief just created under the state directory
	if err != nil {
		return err
	}
	_, err = f.WriteString(text)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// recoverParks repairs settled waits whose local worker no longer exists.
// Discovery failure is not evidence that a worker ended. Managed executions
// have their own lifecycle and must never be replaced by a local start.
func (l *supervisorLoop) recoverParks(ctx context.Context, st *state.State) error {
	if st.Stop != nil || !slices.ContainsFunc(st.Agents, endedParkCandidate) {
		return nil
	}
	sessions, _, err := freshSessions(l.a)
	if err != nil {
		return err
	}
	for _, ag := range st.Agents {
		if !endedParkCandidate(ag) {
			continue
		}
		alive := false
		for _, s := range sessions {
			if s.Party().Is(ag.Party) {
				alive = true
				break
			}
		}
		if alive {
			continue
		}
		ref := agentRef(st, ag.Name)
		pullPark := ag.Park != nil && ag.Park.RefKind == github.RefPullRequest
		if pullPark && ag.Park.On != "" {
			ref = ag.Park.On
		}
		if l.agentUnreadRepo(st, ag, ref) != "" {
			continue
		}
		if stop, err := l.stopPassForReexec(ctx); stop || err != nil {
			return err
		}
		l.lifecyclePhase(loopPhaseRecover)
		if (pullPark || strings.Contains(strings.ToLower(ref), "/pull/")) && !l.liveOpen(ctx, ref) {
			continue
		}
		if l.dryRun {
			if _, err := fmt.Fprintf(l.a.out, "SUPERVISOR WOULD RESUME %s: check its harness before restarting a gone session\n", ag.Name); err != nil {
				return err
			}
			continue
		}
		// A missing process alone is not a gone verdict. Ask the harness
		// to resume its inbox first; only errGone permits replacement.
		if err := loopResumePark(l.a, ctx, loopParty, ag.Name); !errors.Is(err, errGone) {
			continue
		}
		if err := l.restartPark(ctx, ag, ref); err != nil {
			return err
		}
	}
	return nil
}

func endedParkCandidate(ag state.Agent) bool {
	_, _, nonClaude := harnessForHost(ag.HostSession)
	return nonClaude && !ag.Done && ag.Task != "" && ag.Execution == nil && ag.Park != nil && !ag.Park.Resumable.IsZero() && len(ag.Park.Continuation) == 0 && !ag.Park.Continuing
}

func (l *supervisorLoop) restartPark(ctx context.Context, ag state.Agent, ref string) error {
	pullPark := ag.Park != nil && ag.Park.RefKind == github.RefPullRequest
	if pullPark {
		ref = ag.Park.On
	}
	r, ok := parseRef(ref)
	var startErr error
	if !ok || (!pullPark && l.a.cfg.Supervisor.WorkerBrief == "") {
		startErr = fmt.Errorf("no task reference or supervisor.workerBrief")
	} else if l.agentUnreadRepo(nil, ag, ref) != "" {
		startErr = fmt.Errorf("repository of %s is not owned by supervisor", ref)
	} else if (pullPark || strings.Contains(strings.ToLower(ref), "/pull/")) && !l.liveOpen(ctx, ref) {
		startErr = fmt.Errorf("pull request %s is not open", ref)
	} else {
		file, def := l.a.cfg.Supervisor.WorkerBrief, ""
		url := fmt.Sprintf("https://github.com/%s/issues/%d", r.Repo, r.N)
		if pullPark {
			// A recovery continues the parked task; the board template would
			// turn the PR number into an unrelated issue assignment.
			file, def = "", "Continue your existing task after the pull request {{.URL}} settled."
			url = fmt.Sprintf("https://github.com/%s/pull/%d", r.Repo, r.N)
		}
		brief, err := l.brief(file, def, "resume "+ag.Name, briefData{URL: url, Repo: r.Repo, Number: r.N, Worker: ag.Name})
		if err == nil {
			text := fmt.Sprintf("\nContinue your existing task: %s\n%s\nRe-query its live state before continuing.\n", ag.Task, parkMessage(ag.Park, loopParty.Name))
			if ag.Park.PostMerge != nil && *ag.Park.PostMerge != "" {
				text += fmt.Sprintf("\nRemaining post-merge work: %s\n", *ag.Park.PostMerge)
			}
			err = appendBrief(brief, text)
		}
		if err == nil {
			_, err = l.run(ctx, agentsName, agentStartName, ag.Name, brief, "--harness", harnessAuto)
		}
		startErr = err
	}
	if errors.Is(startErr, state.ErrRecoveryWriterFenced) {
		return startErr
	}
	// Release only a holder whose harness confirmed it gone. A partial
	// start may already have installed a resumable replacement.
	gone := ag.Party
	if startErr != nil {
		current, err := l.a.store.Read()
		if err != nil {
			return err
		}
		for _, cur := range current.Agents {
			if cur.Name == ag.Name && cur.Task == ag.Task && !cur.Is(ag.Party) && endedParkCandidate(cur) && cur.Park.At.Equal(ag.Park.At) {
				if err := loopResumePark(l.a, ctx, loopParty, cur.Name); errors.Is(err, errGone) {
					gone = cur.Party
				}
			}
		}
	}
	// A successful start inherits the old task and park through registerAgent.
	return l.a.store.Update(func(st *state.State) ([]state.Event, error) {
		for i := range st.Agents {
			cur := &st.Agents[i]
			if cur.Name != ag.Name || cur.Task != ag.Task || cur.Park == nil || !cur.Park.At.Equal(ag.Park.At) {
				continue
			}
			if startErr == nil {
				cur.Park = nil
			} else if cur.Is(gone) {
				removeAgent(st, i)
			}
			return []state.Event{event(loopParty, "supervisor.resume", "%s: restart result: %v", ag.Name, startErr)}, nil
		}
		return nil, nil
	})
}

// Persistent inbox harnesses do not run the Claude stop-post hook. Resume
// only a positively observed ended turn, once per observed turn timestamp.
func (l *supervisorLoop) recoverUnfinishedTurns(ctx context.Context, st *state.State) error {
	if l.dryRun || st.Stop != nil || l.holdingStarts() {
		return nil
	}
	sessions, _, err := freshSessions(l.a)
	if err != nil {
		return err
	}
	for _, ag := range st.Agents {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		h, _, ok := harnessForHost(ag.HostSession)
		if !ok && ag.Execution == nil || ag.Done || ag.Task == "" || ag.Park != nil || roleRun(st, ag.Party) || !ag.ContinuationExhausted.IsZero() {
			continue
		}
		if keptBy(st, ag, l.a.now) != "" || agyQuotaError(l.a.cfg, ag) != nil {
			continue
		}
		if ag.Execution != nil {
			r, _ := executionRecord(st, *ag.Execution)
			if r == nil || !r.Request.Messaging || r.Completed || r.StopRequest != nil || r.Observation.StopAcknowledged || !r.Observation.Ready || r.Observation.Reachability != execution.Reachable || executionInTurn(st, ag.Party, l.a.now) {
				continue
			}
			if h, ok := l.a.cfg.Agents.Hosts[r.Selector]; !ok || h.Identity != r.Handle.Ref.Host {
				continue
			}
			o := r.Observation
			if !o.BetweenTurns() || o.LastActive.IsZero() || !ag.ResumedWait.IsZero() && !o.LastActive.After(ag.ResumedWait) {
				continue
			}
			if _, err := l.a.resumeOnWait(ctx, ag.Name, ""); err != nil && !isUnreachable(err) && !errors.Is(err, execution.ErrUnsupported) {
				return err
			}
			continue
		}
		endedAt := ag.AssignedAt
		ended := false
		for _, s := range sessions {
			if s.Party().Is(ag.Party) && s.State == omp.StateIdle && len(s.Commands) == 0 {
				ended, endedAt = true, s.LastActive
			}
		}
		if !ended {
			if _, live := liveSession(sessions, ag.Party); live {
				continue
			}
			for _, s := range h.Ended(l.a.cfg, sessions, ag.AssignedAt) {
				if s.Party().Is(ag.Party) {
					ended, endedAt = true, s.LastActive
				}
			}
		}
		if !ended || endedAt.IsZero() || !ag.ResumedWait.IsZero() && !endedAt.After(ag.ResumedWait) {
			continue
		}
		if _, err := l.a.resumeOnWait(ctx, ag.HostSession, ""); err != nil {
			return err
		}
	}
	return nil
}
