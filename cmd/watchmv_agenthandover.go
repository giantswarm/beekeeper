package cmd

import (
	"fmt"
	"regexp"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// dueAgent is a registered agent whose hand-over is due; parked when its
// CLI does not run. held is why it is held instead of due (its task's last
// step, "" when due), until when; was says how long a hand-over due after
// a hold was held.
type dueAgent struct {
	agent   state.Agent
	context int64
	parked  bool
	held    string
	until   time.Time
	was     string
}

// handoversDue are the registered agents whose context reached relayAt, at a
// quiet moment: no tool command of their own running and no gated merge of
// their own in flight. An agent with a task whose CLI no longer runs (its
// headless turn ended, waiting on a grant or a person) is due too, with its
// transcript's context: nothing else relieves it. An agent with no task at
// all (none open, none it reported idle on) is not: its follow-up would
// have nothing to carry on. It skips the agents said
// already, those that reported their work done (the doctor archives them)
// and those holding or relieved of a relayed role, which relay instead.
// An agent in its task's last step (lastStepOf) is held rather than due
// for cfg.LastStepGrace from the step's evidence, unless its context reached
// cfg.LastStepCeiling: its report is expected before a hand-over would
// pay. contextOf gets a nil session for an agent whose CLI does not run.
func handoversDue(st *state.State, sessions []*claude.Session, cfg config.Agents, now time.Time, said func(state.Party) bool,
	contextOf func(state.Agent, *claude.Session) int64, alive func(int) bool,
) []dueAgent {
	var out []dueAgent
	for _, ag := range st.Agents {
		if said(ag.Party) || ag.Done || keepsRole(st, ag.Party) || mergeInFlight(st, ag.Party, alive) {
			continue
		}
		s, live := claude.Live(sessions, ag.Party)
		switch {
		case live && len(s.Commands) > 0, !live && ag.Task == "", ag.Task == "" && ag.LastTask == "":
			continue
		case !live:
			s = nil
		}
		c := contextOf(ag, s)
		if c < int64(cfg.RelayAt) {
			continue
		}
		d := dueAgent{agent: ag, context: c, parked: !live}
		if why, since, ok := lastStepOf(st, ag.Party); ok {
			until := since.Add(cfg.LastStepGrace.Duration)
			switch {
			case now.Before(until) && c < int64(cfg.LastStepCeiling):
				d.held, d.until = why, until
			case now.Before(until):
				d.was = fmt.Sprintf("its last step (%s) holds no hand-over past %s", why, tokensText(int64(cfg.LastStepCeiling)))
			default:
				d.was = fmt.Sprintf("held %s for its last step (%s)", dur(cfg.LastStepGrace.Duration), why)
			}
		}
		out = append(out, d)
	}
	return out
}

// lastStepWords are the words of a sessions serve --waits that say the
// task is in its last step: the report is being written, or the merge
// landed and the proof, the release or the rollout is what remains.
var lastStepWords = regexp.MustCompile(`(?i)\b(report\w*|merged|releas\w*|roll(ed|ing|s)?[ -]?out|proof|proven)\b`)

// lastStepOf finds the evidence that p's task is in its last step and when
// it was given: its sessions serve record waits on the report or says the
// merge landed, or the gate saw a merge of its own land (settling, its
// release and rollout pending). The latest evidence counts; false when
// there is none.
func lastStepOf(st *state.State, p state.Party) (why string, since time.Time, ok bool) {
	for _, r := range st.Records {
		if r.Session.Is(p) && r.Ended.IsZero() && lastStepWords.MatchString(r.Waits) && !r.At.Before(since) {
			why, since, ok = fmt.Sprintf("it waits on %q", r.Waits), r.At, true
		}
	}
	for _, m := range st.Merges {
		if m.By.Is(p) && m.Phase == state.Settling && m.Exit == 0 && !m.Finished.Before(since) {
			why, since, ok = m.Key()+" merged", m.Finished, true
		}
	}
	return why, since, ok
}

// handoversDue says HANDOVER DUE once for each agent handoversDue finds,
// and HANDOVER HELD once for an agent it holds in its last step; the
// watch's mark keeps both said across the watch's restarts.
func (w *watcher) handoversDue(st *state.State, sessions []*claude.Session) {
	key := func(p state.Party) string { return "handover " + p.Session }
	said := func(p state.Party) bool { return w.reported[key(p)] }
	contextOf := func(ag state.Agent, s *claude.Session) int64 {
		if s == nil {
			s = &claude.Session{Transcript: transcriptOf(w.a.cfg, ag.Session)}
		}
		return transcriptContext(s)
	}
	for _, d := range handoversDue(st, sessions, w.a.cfg.Agents, w.a.now, said, contextOf, proc.Alive) {
		if d.held != "" {
			if k := "handover-held " + d.agent.Session; !w.reported[k] {
				w.reported[k], w.dirty = true, true
				w.emitNow("handover", "HANDOVER HELD %q at %s: last step, report expected (%s); due at %s or at %s",
					d.agent.Name, tokensText(d.context), d.held, d.until.Local().Format("15:04"), tokensText(int64(w.a.cfg.Agents.LastStepCeiling)))
			}
			continue
		}
		w.reported[key(d.agent.Party)], w.dirty = true, true
		parked, was := "", ""
		if d.parked {
			parked = " (its CLI does not run: the hand-over resumes it headless for its note)"
		}
		if d.was != "" {
			was = ", " + d.was
		}
		w.emitNow("handover", "HANDOVER DUE %q at %s%s%s: beekeeper agents handover %q", d.agent.Name, tokensText(d.context), parked, was, d.agent.Name)
	}
}
