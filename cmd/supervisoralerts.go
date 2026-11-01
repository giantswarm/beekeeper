package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const alertParkPrefix = "alert:"
const loopAlertIntent = "supervisor.alert-replacement"
const loopAlertKind = "alert"
const loopAlertVerb = "supervisor." + loopAlertKind

// readLoopAlerts consumes the watch's durable snapshot. Only watch reads
// installations; a loop restart can still route a page it has not handled.
var readLoopAlerts = func(_ context.Context, a *app) ([]alerts.Firing, error) {
	evs, err := a.store.Events(1, func(e state.Event) bool { return e.Verb == "watch.alert-state" })
	if err != nil || len(evs) == 0 {
		return nil, err
	}
	var firing []alerts.Firing
	err = json.Unmarshal([]byte(evs[len(evs)-1].Detail), &firing)
	return firing, err
}

// Only a proven unresumable session is replaced. Capacity refusals and
// transient wake failures leave the worker and its task on the roster.
var loopResumable = func(a *app, st *state.State, ag state.Agent) bool {
	if ag.Execution != nil {
		return true
	}
	if _, _, ok := harnessForHost(ag.HostSession); ok {
		sessions, _, err := a.sessions()
		if err != nil {
			return true
		} // an unreadable source proves no session ended
		return slices.ContainsFunc(sessions, func(s *claude.Session) bool { return ag.Is(s.Party()) })
	}
	_, err := resolveWake(a.cfg, st, ag)
	return err == nil
}

type alertReplacement struct {
	Key, Name, Brief string
	Original         state.Party
}

func (l *supervisorLoop) watchAlerts(ctx context.Context) error {
	a := l.a
	evs, err := a.store.Events(0, func(e state.Event) bool {
		return e.Verb == loopAlertVerb || e.Verb == loopAlertIntent || e.Verb == loopClaimVerb || e.Verb == "agents.wake" || e.Verb == "agents.start"
	})
	if err != nil {
		return err
	}
	done := map[string]bool{}
	claims := map[string]loopClaim{}
	var pending []alertReplacement
	pendingKeys := map[string]bool{}
	for _, e := range evs {
		key, rest, ok := keyed(e.Detail)
		if e.Verb == loopAlertVerb && ok {
			done[key] = true
		}
		if e.Verb == loopClaimVerb && ok {
			claims[key] = loopClaim{at: e.At, target: rest}
		}
		if e.Verb == loopAlertIntent {
			var r alertReplacement
			if err := json.Unmarshal([]byte(e.Detail), &r); err != nil {
				return err
			}
			pending = append(pending, r)
			pendingKeys[r.Key] = true
		}
	}
	for _, r := range pending {
		if done[r.Key] {
			continue
		}
		if err := l.replaceAlertWorker(ctx, r, evs); err != nil {
			l.sayOnce("replace "+r.Key, "SUPERVISOR ALERT REPLACEMENT FAILED %s: %v", r.Name, err)
			continue
		}
		if !l.startsHeld {
			done[r.Key] = true
		}
	}
	firing, err := readLoopAlerts(ctx, a)
	if err != nil {
		return err
	}
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	var sessions []*claude.Session
	if slices.ContainsFunc(st.AlertOwners, func(o state.AlertOwner) bool { return !o.Person() && o.Ended.IsZero() }) {
		var err error
		sessions, _, err = a.sessions()
		if err != nil {
			return fmt.Errorf("reading alert owners: %w", err)
		}
	}
	for _, f := range firing {
		// A preceding event may have resumed or replaced a parked worker.
		// Use its current roster entry before routing the next instance.
		st, err = a.store.Read()
		if err != nil {
			return err
		}
		owned := slices.ContainsFunc(st.AlertOwners, func(o state.AlertOwner) bool {
			if o.Alert != f.Key || !o.Ended.IsZero() {
				return false
			}
			_, live := claude.Live(sessions, o.By)
			return o.Person() || live
		})
		waiting := false
		for _, ag := range st.Agents {
			if ag.Park == nil {
				continue
			}
			q, ok := strings.CutPrefix(ag.Park.On, alertParkPrefix)
			if !ok || len(alerts.Match([]alerts.Firing{f}, q)) == 0 {
				continue
			}
			waiting = true
			key := "alert wake " + f.Installation + "/" + f.Key + " " + ag.Name + " " + ag.Park.At.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
			if done[key] || pendingKeys[key] || l.startsHeld {
				continue
			}
			err := l.wakeAlertWorker(ctx, st, ag, f, key, claims, evs)
			if err != nil {
				l.sayOnce(key, "SUPERVISOR ALERT WAKE FAILED %s: %v", ag.Name, err)
				continue
			}
			delete(l.said, key)
			done[key] = true
		}
		// Existing firing pages are included: taking over from a watch must
		// not lose an unowned NEW event already written into its baseline.
		key := "alert page " + f.Installation + "/" + f.Key
		staffed := false
		for k := range done {
			if strings.HasPrefix(k, "alert wake "+f.Installation+"/"+f.Key+" ") {
				staffed = true
				break
			}
		}
		if owned || waiting || staffed || done[key] || f.Severity != a.cfg.Alerts.PageSeverity || (a.cfg.Alerts.Team != "" && f.Team != a.cfg.Alerts.Team) {
			continue
		}
		act := superloop.Action{Kind: loopAlertKind, Key: key}
		question := "Assign an owner to the firing alert " + f.Name() + "?"
		// Alert identities are events, not GitHub issues. File a local note
		// directly, so no invented issue reference or public data is needed.
		if err := l.alertNote(act, question, f); err != nil {
			return err
		}
	}
	return nil
}

func (l *supervisorLoop) alertNote(act superloop.Action, question string, f alerts.Firing) error {
	if l.a.cfg.Guide.Person == "" {
		return fmt.Errorf("guide.person is unset: cannot route unowned alert %s", f.Name())
	}
	question = truncate(question, 150)
	err := l.a.store.Update(func(st *state.State) ([]state.Event, error) {
		// Use the same durable note state as note add, without GitHub calls.
		st.NextNote++
		id := st.NextNote
		status := fmt.Sprintf("%s %s since %s", f.Instance(), f.Where, f.Since)
		st.Notes = append(st.Notes, state.Note{
			ID: id, For: l.a.cfg.Guide.Person, By: loopParty,
			Text: question + "\n" + status, At: l.a.now, Kind: noteDecision,
			Question: question, StatusQuo: status,
			Default: "keep this alert unowned until an owner is assigned",
		})
		return []state.Event{event(loopParty, "note.add", "#%d %s: %s %s since %s", id, question, f.Instance(), f.Where, f.Since), event(loopParty, loopAlertVerb, "%s", superloop.Detail(act, "unowned alert sent to the guide: "+f.Instance()))}, nil
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(l.a.out, "SUPERVISOR ALERT unowned alert sent to the guide:", f.Instance())
	return err
}

func (l *supervisorLoop) replaceAlertWorker(ctx context.Context, r alertReplacement, evs []state.Event) error {
	if l.startsHeld {
		return nil
	}
	// The intent precedes remove/start. Reconcile a successful start after
	// a crash before recording completion, without retiring the replacement.
	intents, err := l.a.store.Events(0, func(e state.Event) bool { return e.Verb == loopAlertIntent && replacementKey(e.Detail) == r.Key })
	if err != nil {
		return err
	}
	started := false
	if len(intents) > 0 {
		at := intents[0].At
		started = slices.ContainsFunc(evs, func(e state.Event) bool {
			return e.Verb == "agents.start" && !e.At.Before(at) && strings.HasPrefix(e.Detail, r.Name+":")
		})
	}
	if !started {
		st, err := l.a.store.Read()
		if err != nil {
			return err
		}
		if i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == r.Name }); i >= 0 {
			if !st.Agents[i].Is(r.Original) {
				return fmt.Errorf("replacement %s waits: its roster name now belongs to another session", r.Name)
			}
			if _, err := l.run(ctx, agentsName, loopRemove, r.Name); err != nil {
				return err
			}
		}
		args := []string{agentsName, agentStartName, r.Name, r.Brief}
		if len(l.a.cfg.Capacity.Choices) > 0 {
			args = append(args, "--harness", harnessAuto)
		}
		if _, err := l.run(ctx, args...); err != nil {
			return err
		}
	}
	return l.record(superloop.Action{Kind: loopAlertKind, Key: r.Key}, "replaced unresumable parked worker "+r.Name+" on its original task")
}

func replacementKey(detail string) string {
	var r alertReplacement
	_ = json.Unmarshal([]byte(detail), &r)
	return r.Key
}

func (l *supervisorLoop) wakeAlertWorker(ctx context.Context, st *state.State, ag state.Agent, f alerts.Firing, key string, claims map[string]loopClaim, evs []state.Event) error {
	a := l.a
	act := superloop.Action{Kind: loopAlertKind, Key: key, Agent: ag.Name, Message: "Your parked event fired: " + f.Instance() + " " + f.Where + ". Re-query the live state of your task and continue it."}
	if c, ok := claims[key]; ok && slices.ContainsFunc(evs, func(e state.Event) bool {
		return e.Verb == "agents.wake" && !e.At.Before(c.at) && strings.HasPrefix(e.Detail, ag.Name+":")
	}) {
		return l.settleAlertWake(act, ag)
	}
	if !loopResumable(a, st, ag) {
		// Write the complete replacement brief before retiring the old
		// roster entry: a restart can finish staffing exactly this task.
		task := ag.Task
		if ref := agentRef(st, ag.Name); ref != "" {
			task += "\nWork the original item " + ref + "; read its acceptance criteria and finish it in full."
		}
		data := briefData{Task: task + "\n\n" + act.Message, Worker: ag.Name}
		if ref, ok := parseRef(agentRef(st, ag.Name)); ok {
			data.Repo, data.Number, data.URL = ref.Repo, ref.N, fmt.Sprintf("https://github.com/%s/issues/%d", ref.Repo, ref.N)
		}
		brief, err := l.brief(a.cfg.Supervisor.WorkerBrief, "{{.Task}}", key, data)
		if err != nil {
			return err
		}
		r := alertReplacement{Key: key, Name: ag.Name, Brief: brief, Original: ag.Party}
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if err := a.store.Record(event(loopParty, loopAlertIntent, "%s", raw)); err != nil {
			return err
		}
		if err := l.replaceAlertWorker(ctx, r, evs); err != nil {
			return err
		}
	} else {
		if err := l.wake(ctx, act); err != nil {
			return err
		}
		return l.settleAlertWake(act, ag)
	}
	return nil
}

// End exactly the park whose wake was delivered, and record completion in
// the same save. A fast worker may already have parked on a new event.
func (l *supervisorLoop) settleAlertWake(act superloop.Action, ag state.Agent) error {
	err := l.a.store.Update(func(st *state.State) ([]state.Event, error) {
		var evs []state.Event
		for i := range st.Agents {
			current := &st.Agents[i]
			if current.Is(ag.Party) && current.Park != nil && current.Park.On == ag.Park.On && current.Park.At.Equal(ag.Park.At) {
				current.Park = nil
				evs = append(evs, event(loopParty, resumedEvent, "%s: alert event fired", ag.Name))
			}
		}
		return append(evs, event(loopParty, loopAlertVerb, "%s", superloop.Detail(act, "alert wake delivered to "+ag.Name))), nil
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(l.a.out, "SUPERVISOR ALERT woke", ag.Name)
	return err
}
