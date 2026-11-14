package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const AlertParkPrefix = "alert:"

const loopAlertIntent = "supervisor.alert-replacement"

const LoopAlertKind = "alert"

const LoopAlertVerb = "supervisor." + LoopAlertKind

type alertReplacement struct {
	Key, Name, Brief string
	Original         state.Party
	// Ref is the item the replaced worker served, "" for none or an intent
	// an older loop wrote: a hold on its repository holds the replacement.
	Ref string `json:",omitempty"`
}

func (l *Loop) watchAlerts(ctx context.Context) error {
	a := l.Host
	evs, err := state.FollowSnapshot(a.Store(), "supervisor-alert-history", "v2", l.eventHistoryFilter(func(e state.Event) bool {
		return e.Verb == LoopAlertVerb || e.Verb == loopAlertIntent || e.Verb == LoopClaimVerb || e.Verb == agentWakeEvent || e.Verb == continuationQueuedEvent || e.Verb == "agents.start"
	}))
	if err != nil {
		return err
	}
	done := map[string]bool{}
	claims := map[string]LoopClaim{}
	var pending []alertReplacement
	pendingKeys := map[string]bool{}
	for _, e := range evs {
		key, rest, ok := Keyed(e.Detail)
		if e.Verb == LoopAlertVerb && ok {
			done[key] = true
		}
		if e.Verb == LoopClaimVerb && ok {
			claims[key] = LoopClaim{At: e.At, Target: rest}
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
			l.SayOnce("replace "+r.Key, "SUPERVISOR ALERT REPLACEMENT FAILED %s: %v", r.Name, err)
			continue
		}
		if !l.HoldingStarts() {
			done[r.Key] = true
		}
	}
	firing, err := Cmd.ReadLoopAlerts(ctx, a)
	if err != nil {
		return err
	}
	st, err := a.Store().Read()
	if err != nil {
		return err
	}
	var sessions []*claude.Session
	if slices.ContainsFunc(st.AlertOwners, func(o state.AlertOwner) bool { return !o.Person() && o.Ended.IsZero() }) {
		var err error
		sessions, _, err = a.Sessions()
		if err != nil {
			return fmt.Errorf("reading alert owners: %w", err)
		}
	}
	var unowned []alerts.Firing
	for _, f := range firing {
		// A preceding event may have resumed or replaced a parked worker.
		// Use its current roster entry before routing the next instance.
		st, err = a.Store().Read()
		if err != nil {
			return err
		}
		owned := slices.ContainsFunc(st.AlertOwners, func(o state.AlertOwner) bool {
			if Cmd.AlertOwnerKey(o) != f.ScopedKey() || !o.Ended.IsZero() {
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
			q, ok := strings.CutPrefix(ag.Park.On, AlertParkPrefix)
			if !ok || len(alerts.Match([]alerts.Firing{f}, q)) == 0 {
				continue
			}
			waiting = true
			key := "alert wake " + f.ScopedKey() + " " + ag.Name
			if done[key] || pendingKeys[key] || l.HoldingStarts() {
				continue
			}
			err := l.wakeAlertWorker(ctx, st, ag, f, key, claims, evs)
			if err != nil {
				l.SayOnce(key, "SUPERVISOR ALERT WAKE FAILED %s: %v", ag.Name, err)
				continue
			}
			l.ClearSaid(key)
			done[key] = true
		}
		// Existing firing pages are included: taking over from a watch must
		// not lose an unowned NEW event already written into its baseline.
		key := "alert page " + f.ScopedKey()
		staffed := false
		for k := range done {
			if strings.HasPrefix(k, "alert wake "+f.ScopedKey()+" ") {
				staffed = true
				break
			}
		}
		if owned || waiting || staffed || done[key] || f.Severity != a.Cfg().Alerts.PageSeverity || !teamAlert(a.Cfg().Alerts.Team, f) {
			continue
		}
		refs := blockedAlertRefs(st, f)
		if len(refs) == 0 {
			unowned = append(unowned, f)
			continue
		}
		act := superloop.Action{Kind: LoopAlertKind, Key: key}
		question := "Assign an owner to the firing alert " + f.Name() + "?"
		// Alert identities are events, not GitHub issues. File a local note
		// directly, so no invented issue reference or public data is needed.
		if err := l.alertNote(act, question, f, refs); err != nil {
			return err
		}
		done[key] = true
	}
	if len(unowned) > 0 {
		counts := map[string]int{}
		var oldest time.Time
		for _, f := range unowned {
			counts[f.Installation]++
			if since, err := time.Parse(time.RFC3339Nano, f.Since); err == nil && (oldest.IsZero() || since.Before(oldest)) {
				oldest = since
			}
		}
		var installations []string
		for name, count := range counts {
			installations = append(installations, fmt.Sprintf("%s=%d", name, count))
		}
		slices.Sort(installations)
		since := kindUnknown
		if !oldest.IsZero() {
			since = oldest.UTC().Format(time.RFC3339Nano)
		}
		_, err := fmt.Fprintf(a.Out(), "SUPERVISOR ALERTS unowned: %s; count=%d; oldest firing=%s\n", strings.Join(installations, ", "), len(unowned), since)
		return err
	}
	return nil
}

// teamAlert reports whether a firing alert is relevant to the loop: with
// alerts.team set, only the team's own alerts are, at any hour (the reader
// keeps the team's alerts that only the working hours inhibit); another
// team's or a team-less alert gets no note and no summary line. A worker
// parked on an alert is woken by it whoever's it is: it asked for that event.
// Without alerts.team every alert is the desk's.
func teamAlert(team string, f alerts.Firing) bool { return team == "" || f.Team == team }

// A configured installation, a lease or an unrelated task is not evidence
// of blocked work. An active session record must name its issue and explicitly
// wait on this alert (sessions serve ... --waits alert:<name>). Parked agents
// are routed above; these records cover work the loop cannot route itself.
func blockedAlertRefs(st *state.State, f alerts.Firing) []string {
	var refs []string
	for _, r := range st.Records {
		q, ok := strings.CutPrefix(r.Waits, AlertParkPrefix)
		ref, valid := Cmd.ParseRef(r.Issue)
		if !r.Ended.IsZero() || !valid || !ok || len(alerts.Match([]alerts.Firing{f}, q)) == 0 {
			continue
		}
		name := Cmd.RefName(ref)
		if !slices.Contains(refs, name) {
			refs = append(refs, name)
		}
	}
	return refs
}

func (l *Loop) alertNote(act superloop.Action, question string, f alerts.Firing, refs []string) error {
	if l.Host.Cfg().Guide.Person == "" {
		return fmt.Errorf("guide.person is unset: cannot route unowned alert %s", f.Name())
	}
	question = Cmd.Truncate(question, 150)
	err := l.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
		// Use the same durable note state as note add, without GitHub calls.
		st.NextNote++
		id := st.NextNote
		status := fmt.Sprintf("%s %s since %s blocks %s", f.Instance(), f.Where, f.Since, strings.Join(refs, ", "))
		st.Notes = append(st.Notes, state.Note{
			Refs: refs, ID: id, For: l.Host.Cfg().Guide.Person, By: LoopParty,
			Text: question + "\n" + status, At: l.Host.TimeNow(), Kind: noteDecision,
			Question: question, StatusQuo: status,
			Default: "keep this alert unowned until an owner is assigned",
		})
		return []state.Event{Cmd.Event(LoopParty, "note.add", "#%d %s: %s %s since %s", id, question, f.Instance(), f.Where, f.Since), Cmd.Event(LoopParty, LoopAlertVerb, "%s", superloop.Detail(act, "unowned alert sent to the guide: "+f.Instance()))}, nil
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(l.Host.Out(), "SUPERVISOR ALERT unowned alert sent to the guide:", f.Instance())
	return err
}

func (l *Loop) replaceAlertWorker(ctx context.Context, r alertReplacement, evs []state.Event) error {
	if l.HoldingStarts() {
		return nil
	}
	// The intent precedes remove/start. Reconcile a successful start after
	// a crash before recording completion, without retiring the replacement.
	intents, err := l.Host.Store().Events(0, func(e state.Event) bool { return e.Verb == loopAlertIntent && replacementKey(e.Detail) == r.Key })
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
		// A repository hold stops the replacement as it stops every start
		// there. The intent and the old roster entry wait for its lift.
		if err := l.RepositoryStart(r.Ref); err != nil {
			if errors.Is(err, ErrLoopLater) {
				return nil
			}
			return err
		}
		st, err := l.Host.Store().Read()
		if err != nil {
			return err
		}
		if i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == r.Name }); i >= 0 {
			if !st.Agents[i].Is(r.Original) {
				return fmt.Errorf("replacement %s waits: its roster name now belongs to another session", r.Name)
			}
			if err := l.Remove(ctx, r.Name, "supervisor loop: unresumable parked worker replaced on its original task"); err != nil {
				return err
			}
		}
		args := []string{agentsName, agentStartName, r.Name, r.Brief}
		if len(l.Host.Cfg().Capacity.Choices) > 0 {
			args = append(args, "--harness", harnessAuto)
		}
		if _, err := l.Run(ctx, args...); err != nil {
			return err
		}
	}
	return l.Record(superloop.Action{Kind: LoopAlertKind, Key: r.Key}, "replaced unresumable parked worker "+r.Name+" on its original task")
}

func replacementKey(detail string) string {
	var r alertReplacement
	_ = json.Unmarshal([]byte(detail), &r)
	return r.Key
}

func (l *Loop) wakeAlertWorker(ctx context.Context, st *state.State, ag state.Agent, f alerts.Firing, key string, claims map[string]LoopClaim, evs []state.Event) error {
	a := l.Host
	act := superloop.Action{Kind: LoopAlertKind, Key: key, Agent: ag.Name, Message: "Your parked event fired: " + f.Instance() + " " + f.Where + ". Re-query the live state of your task and continue it."}
	if c, ok := claims[key]; ok && slices.ContainsFunc(evs, func(e state.Event) bool {
		return (e.Verb == agentWakeEvent || e.Verb == continuationQueuedEvent) && !e.At.Before(c.At) && strings.HasPrefix(e.Detail, ag.Name+":")
	}) {
		return l.SettleAlertWake(act, ag)
	}
	if !Cmd.LoopResumable(a, st, ag) {
		// A replacement is a fresh start: none for a repository the desk
		// does no work on (supervisor.ownership.never, outside its orgs).
		item := AgentRef(st, ag.Name)
		if why := l.agentUnreadRepo(st, ag, item); why != "" {
			return fmt.Errorf("no replacement starts: %s", why)
		}
		// Write the complete replacement brief before retiring the old
		// roster entry: a restart can finish staffing exactly this task.
		task := ag.Task
		if item != "" {
			task += "\nWork the original item " + item + "; read its acceptance criteria and finish it in full."
		}
		data := BriefData{Task: task + "\n\n" + act.Message, Worker: ag.Name}
		if ref, ok := Cmd.ParseRef(item); ok {
			data.Repo, data.Number, data.URL = ref.Repo, ref.N, fmt.Sprintf("https://github.com/%s/issues/%d", ref.Repo, ref.N)
		}
		brief, err := l.Brief(a.Cfg().Supervisor.WorkerBrief, "{{.Task}}", key, data)
		if err != nil {
			return err
		}
		r := alertReplacement{Key: key, Name: ag.Name, Brief: brief, Original: ag.Party, Ref: item}
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if err := a.Store().Record(Cmd.Event(LoopParty, loopAlertIntent, "%s", raw)); err != nil {
			return err
		}
		if err := l.replaceAlertWorker(ctx, r, evs); err != nil {
			return err
		}
	} else {
		if err := l.wake(ctx, act); err != nil {
			return err
		}
		return l.SettleAlertWake(act, ag)
	}
	return nil
}

// End exactly the park whose wake was delivered, and record completion in
// the same save. A fast worker may already have parked on a new event.
func (l *Loop) SettleAlertWake(act superloop.Action, ag state.Agent) error {
	err := l.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
		var evs []state.Event
		for i := range st.Agents {
			current := &st.Agents[i]
			if current.Is(ag.Party) && current.Park != nil && current.Park.On == ag.Park.On && current.Park.At.Equal(ag.Park.At) && len(current.Park.Continuation) == 0 {
				current.Park = nil
				evs = append(evs, Cmd.Event(LoopParty, resumedEvent, "%s: alert event fired", ag.Name))
			}
		}
		return append(evs, Cmd.Event(LoopParty, LoopAlertVerb, "%s", superloop.Detail(act, "alert wake delivered to "+ag.Name))), nil
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(l.Host.Out(), "SUPERVISOR ALERT woke", ag.Name)
	return err
}
