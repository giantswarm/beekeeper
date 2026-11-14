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

// DueAgent is a registered agent whose hand-over is due; parked when its
// CLI does not run. held is why it is held instead of due (its task's last
// step, "" when due), until when; was says how long a hand-over due after
// a hold was held.
type DueAgent struct {
	Agent   state.Agent
	Context int64
	Parked  bool
	Held    string
	Until   time.Time
	Was     string
}

// HandoversDue are the registered agents whose context reached relayAt, at a
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
func HandoversDue(st *state.State, sessions []*claude.Session, cfg config.Agents, now time.Time, said func(state.Party) bool,
	contextOf func(state.Agent, *claude.Session) int64, alive func(int) bool,
) []DueAgent {
	var out []DueAgent
	for _, ag := range st.Agents {
		if said(ag.Party) || ag.Done || Cmd.KeepsRole(st, ag.Party) || Cmd.MergeInFlight(st, ag.Party, alive) {
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
		d := DueAgent{Agent: ag, Context: c, Parked: !live}
		if why, since, ok := lastStepOf(st, ag.Party); ok {
			until := since.Add(cfg.LastStepGrace.Duration)
			switch {
			case now.Before(until) && c < int64(cfg.LastStepCeiling):
				d.Held, d.Until = why, until
			case now.Before(until):
				d.Was = fmt.Sprintf("its last step (%s) holds no hand-over past %s", why, Cmd.TokensText(int64(cfg.LastStepCeiling)))
			default:
				d.Was = fmt.Sprintf("held %s for its last step (%s)", Cmd.Dur(cfg.LastStepGrace.Duration), why)
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

// HandoversDue says HANDOVER DUE once for each agent handoversDue finds,
// and HANDOVER HELD once for an agent it holds in its last step; the
// watch's mark keeps both said across the watch's restarts.
func (w *Watcher) HandoversDue(st *state.State, sessions []*claude.Session) {
	key := func(p state.Party) string { return "handover " + p.Session }
	said := func(p state.Party) bool { return w.Reported[key(p)] }
	contextOf := func(ag state.Agent, s *claude.Session) int64 {
		if s == nil {
			s = &claude.Session{Transcript: Cmd.TranscriptOf(w.Host.Cfg(), ag.Session)}
		}
		return Cmd.TranscriptContext(s)
	}
	for _, d := range HandoversDue(st, sessions, w.Host.Cfg().Agents, w.Host.Now(), said, contextOf, proc.Alive) {
		if d.Held != "" {
			if k := "handover-held " + d.Agent.Session; !w.Reported[k] {
				w.Reported[k], w.Dirty = true, true
				w.EmitNow("handover", "HANDOVER HELD %q at %s: last step, report expected (%s); due at %s or at %s",
					d.Agent.Name, Cmd.TokensText(d.Context), d.Held, d.Until.Local().Format("15:04"), Cmd.TokensText(int64(w.Host.Cfg().Agents.LastStepCeiling)))
			}
			continue
		}
		w.Reported[key(d.Agent.Party)], w.Dirty = true, true
		parked, was := "", ""
		if d.Parked {
			parked = " (its CLI does not run: the hand-over resumes it headless for its note)"
		}
		if d.Was != "" {
			was = ", " + d.Was
		}
		w.EmitNow("handover", "HANDOVER DUE %q at %s%s%s: beekeeper agents handover %q", d.Agent.Name, Cmd.TokensText(d.Context), parked, was, d.Agent.Name)
	}
}
