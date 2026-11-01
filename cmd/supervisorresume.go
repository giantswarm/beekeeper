package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/giantswarm/beekeeper/internal/state"
)

var loopResumePark = (*app).resumeParked

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
