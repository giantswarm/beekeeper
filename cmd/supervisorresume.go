package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

var loopResumePark = (*app).resumeParked

// recoverQuota reports any local agy worker whose run ended with a quota
// error, marks its bucket full until reset, frees its slot and re-queues its
// task on another choice.
func (l *supervisorLoop) recoverQuota(ctx context.Context, st *state.State) error {
	if st.Stop != nil {
		return nil
	}
	for _, ag := range st.Agents {
		if ag.Done || ag.Task == "" || ag.Execution != nil || roleRun(st, ag.Party) {
			continue
		}
		qerr := agyQuotaError(l.a.cfg, ag)
		if qerr == nil {
			continue
		}
		ref := cmp.Or(loopServes(st, ag), agentRef(st, ag.Name))
		target := cmp.Or(ref, ag.Task)
		if l.dryRun {
			if _, err := fmt.Fprintf(l.a.out, "SUPERVISOR WOULD QUOTA %s: %s; free slot and re-queue %s on another choice\n", ag.Name, qerr.Error(), target); err != nil {
				return err
			}
			continue
		}
		startID := agyAgentStartID(ag)
		_, model := agentHarnessModel(st, nil, ag)
		l.a.recordAgyQuotaExhaustedForID(startID, model, qerr, l.a.now)
		if startID != "" {
			_ = l.a.endAgy(ctx, startID)
		}
		note, _, _ := l.a.lastNote(ag.Party, time.Time{})
		err := l.a.store.Update(func(st *state.State) ([]state.Event, error) {
			i := slices.IndexFunc(st.Agents, func(x state.Agent) bool { return x.Is(ag.Party) })
			if i < 0 {
				return nil, nil
			}
			for j := range st.Records {
				if st.Records[j].Session.Is(ag.Party) {
					st.Records[j].Ended, st.Records[j].Waits = l.a.now.UTC(), ""
				}
			}
			removeAgent(st, i)
			return []state.Event{
				event(loopParty, "agents.gone", "%s: %s; task: %s; hand-over: %s", ag.Name, qerr.Error(), ag.Task, note),
				event(loopParty, "supervisor.quota", "%s: %s; slot freed, task %s re-queued", ag.Name, qerr.Error(), target),
			}, nil
		})
		if err != nil {
			return err
		}
		l.fillPause = fillPause{}
		l.fillEmpty = 0
		l.boardNone = nil
		if _, err := fmt.Fprintf(l.a.out, "SUPERVISOR QUOTA %s: %s; slot freed, re-queuing %s on another choice\n", ag.Name, qerr.Error(), target); err != nil {
			return err
		}
		if !l.startsHeld && l.a.cfg.Supervisor.WorkerBrief != "" {
			if r, ok := parseRef(ref); ok {
				brief, berr := l.brief(l.a.cfg.Supervisor.WorkerBrief, "", "quota "+ag.Name, briefData{URL: fmt.Sprintf("https://github.com/%s/issues/%d", r.Repo, r.N), Repo: r.Repo, Number: r.N, Worker: ag.Name})
				if berr == nil {
					_, _ = l.run(ctx, agentsName, agentStartName, ag.Name, brief, "--harness", harnessAuto)
				}
			}
		}
	}
	return nil
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
		if err := l.restartPark(ctx, ag, agentRef(st, ag.Name)); err != nil {
			return err
		}
	}
	return nil
}

func endedParkCandidate(ag state.Agent) bool {
	_, _, nonClaude := harnessForHost(ag.HostSession)
	return nonClaude && !ag.Done && ag.Task != "" && ag.Execution == nil && ag.Park != nil && !ag.Park.Resumable.IsZero()
}

func (l *supervisorLoop) restartPark(ctx context.Context, ag state.Agent, ref string) error {
	r, ok := parseRef(ref)
	var startErr error
	if !ok || l.a.cfg.Supervisor.WorkerBrief == "" {
		startErr = fmt.Errorf("no task reference or supervisor.workerBrief")
	} else {
		brief, err := l.brief(l.a.cfg.Supervisor.WorkerBrief, "", "resume "+ag.Name, briefData{URL: fmt.Sprintf("https://github.com/%s/issues/%d", r.Repo, r.N), Repo: r.Repo, Number: r.N, Worker: ag.Name})
		if err == nil {
			var f *os.File
			f, err = os.OpenFile(brief, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // the brief just created under the state directory
			if err == nil {
				_, err = fmt.Fprintf(f, "\nContinue your existing task: %s\n%s\nRe-query its live state before continuing.\n", ag.Task, parkMessage(ag.Park, loopParty.Name))
				closeErr := f.Close()
				if err == nil {
					err = closeErr
				}
			}
		}
		if err == nil {
			_, err = l.run(ctx, agentsName, agentStartName, ag.Name, brief, "--harness", harnessAuto)
		}
		startErr = err
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
