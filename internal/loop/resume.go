package loop

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

	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// RecoverQuota re-queues the task of any local agy worker or reviewer whose
// run ended with a quota error on another choice. Its bucket is marked full
// until reset, which goneAgent already frees the slot for. The entry and its
// quota marker stay until a replacement started: a held, refused or failed
// start leaves them for the next pass, so no task is lost.
func (l *Loop) RecoverQuota(ctx context.Context, st *state.State) error {
	if st.Stop != nil {
		return nil
	}
	for _, ag := range st.Agents {
		if err := ctx.Err(); err != nil {
			return err
		}
		if ag.Done || ag.Task == "" || ag.Execution != nil || Cmd.RoleRun(st, ag.Party) || ag.Park != nil && len(ag.Park.Continuation) > 0 {
			continue
		}
		qerr := Cmd.AgyQuotaError(l.Host.Cfg(), ag)
		if qerr == nil {
			continue
		}
		ref := cmp.Or(LoopServes(st, ag), AgentRef(st, ag.Name))
		if l.agentUnreadRepo(st, ag, ref) != "" {
			continue
		}
		target := cmp.Or(ref, ag.Task)
		if stop, err := l.stopPassForReexec(ctx); stop || err != nil {
			return err
		}
		l.LifecyclePhase(LoopPhaseRecover)
		if l.DryRun {
			if _, err := fmt.Fprintf(l.Host.Out(), "SUPERVISOR WOULD QUOTA %s: %s; free slot and re-queue %s on another choice\n", ag.Name, qerr.Error(), target); err != nil {
				return err
			}
			continue
		}
		startID := Cmd.AgyAgentStartID(ag)
		_, model := capacity.AgentHarnessModel(st, nil, ag)
		l.Host.RecordAgyQuotaExhaustedForID(startID, model, qerr, l.Host.Now())
		if l.StartsHeld {
			l.SayOnce("quota held "+startID, "SUPERVISOR QUOTA %s: %s; slot freed, starts are held: %s waits for its re-queue", ag.Name, qerr.Error(), target)
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
			if errors.Is(err, ErrLoopLater) {
				l.SayOnce("quota later "+startID, "SUPERVISOR QUOTA %s: %s; slot freed, no room for its replacement yet: %s waits for its re-queue", ag.Name, qerr.Error(), target)
				continue
			}
			l.SayOnce("quota failed "+startID, "SUPERVISOR QUOTA FAILED %s: %s; %s stays on the roster: %v", ag.Name, qerr.Error(), target, err)
			continue
		}
		note, _, _ := l.Host.LastNoteContext(ctx, ag.Party, time.Time{}, func() { l.LifecyclePhase(LoopPhaseRecover) })
		l.LifecyclePhase(LoopPhaseRecover)
		err = l.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
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
				st.Records[j].Ended, st.Records[j].Waits = l.Host.Now().UTC(), ""
			}
			if i := slices.IndexFunc(st.Agents, func(x state.Agent) bool { return x.Is(ag.Party) }); i >= 0 {
				Cmd.RemoveAgent(st, i)
			}
			return []state.Event{
				Cmd.Event(LoopParty, "agents.gone", "%s: %s; task: %s; hand-over: %s", ag.Name, qerr.Error(), ag.Task, note),
				Cmd.Event(LoopParty, "supervisor.quota", "%s: %s; slot freed, task %s re-queued", ag.Name, qerr.Error(), target),
			}, nil
		})
		if err != nil {
			return err
		}
		if startID != "" {
			_ = l.Host.EndAgy(ctx, startID)
		}
		l.Mu.Lock()
		l.FillPause = FillPause{}
		l.FillEmpty = 0
		l.BoardNone = nil
		l.Mu.Unlock()
		if _, err := fmt.Fprintf(l.Host.Out(), "SUPERVISOR QUOTA %s: %s; slot freed, re-queuing %s on another choice\n", ag.Name, qerr.Error(), target); err != nil {
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
func (l *Loop) quotaReview(st *state.State, ag state.Agent) (*quotaRound, error) {
	evs, err := state.FollowSnapshot(l.Host.Store(), "supervisor-review-quota", "v1", l.eventHistoryFilter(func(e state.Event) bool { return e.By.Name == LoopParty.Name && e.Verb == LoopClaimVerb }))
	if err != nil {
		return nil, err
	}
	for i := len(evs) - 1; i >= 0; i-- {
		key, name, ok := Keyed(evs[i].Detail)
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
		return &quotaRound{key: key, ref: ref, round: rd, selfHosted: reviewedSelfHosted(l.Host.Cfg().Capacity, st, ref)}, nil
	}
	return nil, nil
}

// reviewedSelfHosted says whether the work under review at ref is a
// self-hosted model's, as the loop's plan decides it; a worker no longer
// found counts as one, so the replacement review stays on a frontier
// model.
func reviewedSelfHosted(k config.Capacity, st *state.State, ref string) bool {
	for _, ag := range st.Agents {
		if ag.Park != nil && strings.EqualFold(LoopParkOn(ag.Park), ref) {
			return SelfHostedModel(k, ag.Model)
		}
	}
	for _, rp := range st.ReviewParks {
		if strings.EqualFold(rp.On, ref) {
			return SelfHostedModel(k, rp.Model)
		}
	}
	return true
}

// restartQuota starts the replacement of ag under its name, placed as the
// loop places its starts: a reviewer with the brief of its round and the
// review flags, a worker with supervisor.workerBrief and its old task.
// ErrLoopLater says there is no room for it yet.
func (l *Loop) restartQuota(ctx context.Context, st *state.State, ag state.Agent, ref string, review *quotaRound) error {
	if review != nil {
		ref = review.ref
	}
	r, ok := Cmd.ParseRef(ref)
	if !ok {
		return fmt.Errorf("no task reference")
	}
	if l.agentUnreadRepo(st, ag, ref) != "" {
		return ErrLoopLater
	}
	if (review != nil || superloop.IsPull(ref) && ag.Park != nil && ag.Park.RefKind == github.RefPullRequest || strings.Contains(strings.ToLower(ref), "/pull/")) && !l.liveOpen(ctx, ref) {
		return ErrLoopLater
	}
	var brief string
	if review != nil {
		brief = filepath.Join(l.Host.Store().Dir(), "supervisor-loop", briefName.ReplaceAllString(review.key, "-")+".md")
		if _, err := os.Stat(brief); err != nil {
			return fmt.Errorf("the brief of review round %d: %w", review.round, err)
		}
	} else {
		if l.Host.Cfg().Supervisor.WorkerBrief == "" {
			return fmt.Errorf("no supervisor.workerBrief")
		}
		var err error
		brief, err = l.Brief(l.Host.Cfg().Supervisor.WorkerBrief, "", "quota "+ag.Name, BriefData{URL: fmt.Sprintf("https://github.com/%s/issues/%d", r.Repo, r.N), Repo: r.Repo, Number: r.N, Worker: ag.Name})
		if err != nil {
			return err
		}
		if err := appendBrief(brief, fmt.Sprintf("\nContinue your existing task: %s\nIts previous session stopped on a quota error. Re-query its live state before continuing.\n", ag.Task)); err != nil {
			return err
		}
	}
	var place startHost
	var err error
	if review != nil {
		place, err = l.startPlacement(ctx, ag.Name, r.Repo, true, review.selfHosted)
	} else {
		var here string
		place, here, err = l.taskPlacement(ctx, st, ag.Name, r.Repo, ref, LoopServes(st, ag), LoopTaskServes(ag, ref))
		if err == nil && here != "" {
			l.SayOnce("quota here "+ag.Name, "SUPERVISOR QUOTA %s STAYS HERE: %s", ag.Name, here)
		}
	}
	if err != nil {
		return err
	}
	args := []string{agentsName, agentStartName, ag.Name, brief}
	if review != nil {
		args = append(args, "--"+reviewFlag)
	}
	if len(l.Host.Cfg().Capacity.Choices) > 0 {
		args = append(args, "--harness", harnessAuto)
		if review != nil && review.selfHosted {
			args = append(args, "--"+reviewOfSelfHostedFlag)
		}
	}
	args = append(args, place.remoteStartFlags()...)
	args = append(args, place.excludeFlags()...)
	if _, err := l.Run(ctx, args...); err != nil {
		if Cmd.Code(err) == ExitRefused {
			if place.remote() {
				if _, rerr := l.remoteStartRefused(ctx, ag.Name); rerr == nil {
					return nil
				}
			}
			return ErrLoopLater
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

// RecoverParks repairs settled waits whose local worker no longer exists.
// Discovery failure is not evidence that a worker ended. Managed executions
// have their own lifecycle and must never be replaced by a local start.
func (l *Loop) RecoverParks(ctx context.Context, st *state.State) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if st.Stop != nil || !slices.ContainsFunc(st.Agents, Cmd.EndedParkCandidate) {
		return nil
	}
	sessions, _, err := Cmd.FreshSessions(l.Host)
	if err != nil {
		return err
	}
	for _, ag := range st.Agents {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !Cmd.EndedParkCandidate(ag) {
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
		ref := AgentRef(st, ag.Name)
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
		l.LifecyclePhase(loopPhaseRecoverParks)
		if (pullPark || strings.Contains(strings.ToLower(ref), "/pull/")) && !l.liveOpen(ctx, ref) {
			continue
		}
		if l.DryRun {
			if _, err := fmt.Fprintf(l.Host.Out(), "SUPERVISOR WOULD RESUME %s: check its harness before restarting a gone session\n", ag.Name); err != nil {
				return err
			}
			continue
		}
		// A missing process alone is not a gone verdict. Ask the harness
		// to resume its inbox first; only errGone permits replacement.
		if err := Cmd.LoopResumePark(l.Host, ctx, LoopParty, ag.Name); !errors.Is(err, Cmd.ErrGone()) {
			continue
		}
		if err := l.RestartPark(ctx, ag, ref); err != nil {
			return err
		}
	}
	return nil
}

func (l *Loop) RestartPark(ctx context.Context, ag state.Agent, ref string) error {
	pullPark := ag.Park != nil && ag.Park.RefKind == github.RefPullRequest
	if pullPark {
		ref = ag.Park.On
	}
	r, ok := Cmd.ParseRef(ref)
	var startErr error
	if !ok || (!pullPark && l.Host.Cfg().Supervisor.WorkerBrief == "") {
		startErr = fmt.Errorf("no task reference or supervisor.workerBrief")
	} else if l.agentUnreadRepo(nil, ag, ref) != "" {
		startErr = fmt.Errorf("repository of %s is not owned by supervisor", ref)
	} else if (pullPark || strings.Contains(strings.ToLower(ref), "/pull/")) && !l.liveOpen(ctx, ref) {
		startErr = fmt.Errorf("pull request %s is not open", ref)
	} else {
		file, def := l.Host.Cfg().Supervisor.WorkerBrief, ""
		url := fmt.Sprintf("https://github.com/%s/issues/%d", r.Repo, r.N)
		if pullPark {
			// A recovery continues the parked task; the board template would
			// turn the PR number into an unrelated issue assignment.
			file, def = "", "Continue your existing task after the pull request {{.URL}} settled."
			url = fmt.Sprintf("https://github.com/%s/pull/%d", r.Repo, r.N)
		}
		brief, err := l.Brief(file, def, "resume "+ag.Name, BriefData{URL: url, Repo: r.Repo, Number: r.N, Worker: ag.Name})
		if err == nil {
			text := fmt.Sprintf("\nContinue your existing task: %s\n%s\nRe-query its live state before continuing.\n", ag.Task, Cmd.ParkMessage(ag.Park, LoopParty.Name))
			if ag.Park.PostMerge != nil && *ag.Park.PostMerge != "" {
				text += fmt.Sprintf("\nRemaining post-merge work: %s\n", *ag.Park.PostMerge)
			}
			err = appendBrief(brief, text)
		}
		if err == nil {
			_, err = l.Run(ctx, agentsName, agentStartName, ag.Name, brief, "--harness", harnessAuto)
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
		current, err := l.Host.Store().Read()
		if err != nil {
			return err
		}
		for _, cur := range current.Agents {
			if cur.Name == ag.Name && cur.Task == ag.Task && !cur.Is(ag.Party) && Cmd.EndedParkCandidate(cur) && cur.Park.At.Equal(ag.Park.At) {
				if err := Cmd.LoopResumePark(l.Host, ctx, LoopParty, cur.Name); errors.Is(err, Cmd.ErrGone()) {
					gone = cur.Party
				}
			}
		}
	}
	// A successful start inherits the old task and park through registerAgent.
	return l.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
		for i := range st.Agents {
			cur := &st.Agents[i]
			if cur.Name != ag.Name || cur.Task != ag.Task || cur.Park == nil || !cur.Park.At.Equal(ag.Park.At) {
				continue
			}
			if startErr == nil {
				cur.Park = nil
			} else if cur.Is(gone) {
				Cmd.RemoveAgent(st, i)
			}
			return []state.Event{Cmd.Event(LoopParty, "supervisor.resume", "%s: restart result: %v", ag.Name, startErr)}, nil
		}
		return nil, nil
	})
}

// recoverUnfinishedTurns runs in the command, which holds what it needs (loopRecoverUnfinishedTurns).
func (l *Loop) recoverUnfinishedTurns(ctx context.Context, st *state.State) error {
	return Cmd.LoopRecoverUnfinishedTurns(l, ctx, st)
}
