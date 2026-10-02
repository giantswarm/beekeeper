package cmd

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// resumedWaitEvent is the event of a headless resume after a turn that ended
// on a background wait.
const resumedWaitEvent = "agent.resumed-wait"

// waitResumeMessage is the turn a worker whose headless turn ended on a
// background wait is resumed with; %s names the wait.
const waitResumeMessage = "Your headless turn ended while %s ran in the background. In a headless turn the end of " +
	"the turn is the end of the process: no completion notice wakes it. Re-query the live state of your task " +
	"and continue it, waiting in the foreground (devctl pr wait, devctl pr merge, a foreground Bash with a " +
	"bounded timeout); end a turn only when the task is done or parked on a person."

// parkedWords are what a serve record's --waits names when the agent waits
// on a person: their answer reaches it by message or wake, not by a resume.
var parkedWords = regexp.MustCompile(`(?i)\b(?:note|person|supervisor|guide|answer|review|approval|decision)s?\b`)

// parkedOn is what a serve record says the agent waits on when that is a
// person (guide.person, a note, the supervisor's answer); "" otherwise.
func parkedOn(waits, person string) string {
	if parkedWords.MatchString(waits) || person != "" && regexp.MustCompile(`(?i)\b`+regexp.QuoteMeta(person)+`\b`).MatchString(waits) {
		return waits
	}
	return ""
}

// waitsOf is what the serve record of ag says it waits on; "" for none.
func waitsOf(records []state.Record, ag state.Agent) string {
	for _, r := range slices.Backward(records) {
		if r.Session.Is(ag.Party) {
			return r.Waits
		}
	}
	return ""
}

// resumedForTask reports whether ag was resumed on a wait for its current
// task already.
func resumedForTask(ag state.Agent) bool {
	return !ag.ResumedWait.IsZero() && !ag.ResumedWait.Before(ag.AssignedAt)
}

// unitLeftovers are the argument lists of the processes a headless turn's
// unit still runs once its CLI ended (KillMode=process keeps them), apart
// from the reopen calling it; nil outside a start's or wake's unit of
// session id. Tests replace it.
var unitLeftovers = func(id string) [][]string {
	self := os.Getpid()
	cg := proc.Cgroup(self)
	if !turnUnit(path.Base(cg), id) {
		return nil
	}
	t, err := plat.Machine.Processes()
	if err != nil {
		return nil
	}
	var out [][]string
	for _, pid := range plat.Machine.CgroupPIDs(cg) {
		if p := t.ByPID[pid]; pid != self && p != nil && len(p.Args) > 0 {
			out = append(out, p.Args)
		}
	}
	return out
}

// turnUnit reports whether unit is the start's or a wake's unit of session
// id.
func turnUnit(unit, id string) bool {
	return unit == "beekeeper-agent-"+id[:min(8, len(id))]+".service" ||
		strings.HasPrefix(unit, wakePrefix(id)+"-") && strings.HasSuffix(unit, ".service")
}

// endedOnWait names the background wait session id's headless turn ended on:
// a background Bash its transcript launched with no completion notice after
// it, else a process its unit still runs, named by its masked command line
// (a process's arguments may carry a secret, its own commands do not); ""
// for none. A devctl wait or
// merge the gate runs is none: its outcome wakes its owner (devctl.unheard).
func (a *app) endedOnWait(id string) string {
	var waits []string
	if m, _ := filepath.Glob(filepath.Join(a.cfg.Claude.ProjectsDir, "*", id+".jsonl")); len(m) > 0 {
		open, _ := claude.OpenBackground(m[0])
		for _, b := range slices.Backward(open) {
			if !guard.Owned(b.Command) {
				waits = append(waits, cmp.Or(strings.TrimSpace(b.Description), b.Command))
			}
		}
	}
	for _, args := range unitLeftovers(id) {
		if c := strings.Join(args, " "); !guard.Owned(c) && !strings.Contains(c, "beekeeper gate") {
			waits = append(waits, display(args))
		}
	}
	if len(waits) == 0 {
		return ""
	}
	return fmt.Sprintf("%q", truncate(waits[0], 120))
}

// resumeOnWait resumes the agent of session id headless once its headless
// turn ended on a background wait with its task open: the end of a headless
// turn is the end of its process, and the wait's completion notice never
// wakes it. Once per task, logged as agent.resumed-wait; an agent done,
// kept, or parked on a person (its serve record waits on one) is left alone.
// It reports whether it resumed the agent, whose wake turn's reopen then
// shows it in the desktop.
func (a *app) resumeOnWait(ctx context.Context, id string) (bool, error) {
	st, err := a.store.Read()
	if err != nil {
		return false, err
	}
	i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool {
		return ag.Session == id || ag.HostSession == "local_"+id
	})
	if i < 0 {
		return false, nil
	}
	ag := st.Agents[i]
	if ag.Task == "" || ag.Done || keptBy(st, ag, a.now) != "" || resumedForTask(ag) ||
		parkedOn(waitsOf(st.Records, ag), a.cfg.Guide.Person) != "" {
		return false, nil
	}
	w, err := resolveWake(a.cfg, st, ag)
	if err != nil {
		return false, nil
	}
	on := a.endedOnWait(w.id)
	if on == "" {
		return false, nil
	}
	if err := resumeHeadless(ctx, a, ag, fmt.Sprintf(waitResumeMessage, on)); err != nil {
		return false, fmt.Errorf("resuming %s on its wait: %w", ag.Name, err)
	}
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		for k := range st.Agents {
			if st.Agents[k].Is(ag.Party) {
				st.Agents[k].ResumedWait, st.Agents[k].ResumedOn = a.now.UTC(), on
			}
		}
		return []state.Event{event(ag.Party, resumedWaitEvent, "%s: its headless turn ended on %s with its task open: resumed it headless once to wait in the foreground", ag.Name, on)}, nil
	})
	if err != nil {
		return true, err
	}
	_, err = fmt.Fprintf(a.out, "reopen: %s ended its headless turn on %s: resumed it headless to wait in the foreground, whose reopen follows its turn\n", ag.Name, on)
	return true, err
}

// resumeHeadless resumes ag with msg as agents wake does; tests replace it.
var resumeHeadless = func(ctx context.Context, a *app, ag state.Agent, msg string) error {
	return a.wakeAgent(ctx, ag.Party, ag.Name, msg, "")
}
