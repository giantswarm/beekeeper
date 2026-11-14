package watch

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/feed"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
)

const resumableEvent = "agents.resumable"

// SettledPark is a park whose note closed or whose issue or pull request
// merged or closed, and what settled it.
type SettledPark struct {
	Party      state.Party
	On, Answer string
}

// SettledParks reads which parks of st settled: a hold it parked on is
// lifted or ended, a note it parked on is no longer open (the log, read only then, has its answer), an issue or pull
// request is merged or closed (GitHub, read only for parks on one and while
// the budget is over its floor).
func (w *Watcher) SettledParks(ctx context.Context, st *state.State) []SettledPark {
	open := map[int]bool{}
	for _, n := range st.Notes {
		open[n.ID] = true
	}
	var parked, centralParked []state.Agent
	var closed []int
	var refs []github.PR
	out := w.settledReleaseParks(ctx, st)
	for _, ag := range Cmd.ParkedAgents(st) {
		p := ag.Park
		if p == nil || p.Checks || p.On == "" || !p.Resumable.IsZero() {
			continue
		}
		if p.ReviewHead != "" && p.PostMerge != nil && slices.ContainsFunc(st.Agents, func(registered state.Agent) bool { return registered.Is(ag.Party) }) {
			continue
		}
		if res, ok := Cmd.ParkLease(p.On); ok {
			if answer, ok := w.Host.LeaseSettled(st, ag, res); ok {
				out = append(out, SettledPark{ag.Party, p.On, answer})
			}
			continue
		}
		if target, ok := Cmd.ParkHold(p.On); ok {
			if !Cmd.HoldStands(st, target, w.Host.Now()) {
				out = append(out, SettledPark{ag.Party, p.On, fmt.Sprintf("nothing holds %s any more: run the refused command again", target)})
			}
			continue
		}
		if id, ok := Cmd.ParkNote(p.On); ok {
			if !open[id] {
				parked, closed = append(parked, ag), append(closed, id)
			}
		} else if ctx, _, ok := Cmd.ParkCentralNote(p.On); ok {
			if w.Host.CentralNotesOn() && strings.EqualFold(ctx, w.Host.Cfg().Central.Context) {
				centralParked = append(centralParked, ag)
			}
		} else if r, ok := Cmd.ParseRef(p.On); ok {
			parked = append(parked, ag)
			if p.RefKind != github.RefPullRequest && !p.RefChecked.IsZero() && w.Host.Now().Sub(p.RefChecked) < parkRefRetry {
				continue
			}
			if !slices.Contains(refs, r) {
				refs = append(refs, r)
			}
		}
	}
	out = append(out, w.settledCentralParks(ctx, centralParked)...)
	if len(parked) == 0 {
		return out
	}
	var states map[github.PR]github.RefState
	if len(refs) > 0 && !loop.BoardBudgetLow(st.Budget, w.Host.Cfg().GitHub.Floor, w.Host.Now()) {
		// Back off issues and unresolved references even when GitHub fails. A failed batch leaves
		// its agents parked; the others settle.
		var s map[github.PR]github.RefState
		read := func() error {
			attempts := map[github.PR]github.RefState{}
			for _, ag := range parked {
				if ag.Park != nil && ag.Park.RefKind != github.RefPullRequest {
					r, ok := Cmd.ParseRef(ag.Park.On)
					if ok && slices.Contains(refs, r) {
						attempts[r] = github.RefState{Kind: ag.Park.RefKind}
					}
				}
			}
			if len(attempts) > 0 {
				w.Host.CacheParkRefs(parked, attempts)
			}
			var err error
			s, err = Cmd.RefStates(ctx, refs)
			return err
		}
		var err error
		if w.LoopOwned {
			err = w.Host.LoopGitHub(ctx, "park-refs", read)
		} else {
			err = read()
		}
		if err != nil {
			if !errors.Is(err, loop.ErrLoopLater) {
				w.Fail("park-refs", "cannot read the parked agents' issues and pull requests: %v", err)
			}
		} else {
			w.Clear("park-refs")
		}
		if len(s) > 0 {
			states = s
			w.Host.CacheParkRefs(parked, s)
		}
	}
	var closes map[int]string
	if len(closed) > 0 {
		evs, err := w.Host.Store().Events(0, func(e state.Event) bool { return Cmd.NoteClosing(e.Verb) })
		if err != nil {
			return out // the next poll reads the notes again
		}
		closes = NoteCloses(evs, closed, "")
	}
	for _, ag := range parked {
		on := ag.Park.On
		if id, ok := Cmd.ParkNote(on); ok {
			out = append(out, SettledPark{ag.Party, on, cmp.Or(closes[id], fmt.Sprintf("note #%d is closed", id))})
			continue
		}
		r, _ := Cmd.ParseRef(on)
		if s := states[r].State; s != "" && s != github.Open {
			out = append(out, SettledPark{ag.Party, on, Cmd.RefName(r) + " " + strings.ToLower(s)})
		} else if rp := states[r].Reply; rp.At.After(ag.Park.At) {
			out = append(out, SettledPark{ag.Party, on, replyText(Cmd.RefName(r), rp)})
		}
	}
	return out
}

// replyText says what replied on the pull request ref since a park on it:
// its newest review or comment by anyone but its author.
func replyText(ref string, r github.Reply) string {
	return fmt.Sprintf("%s has a new %s by %s at %s: read and answer its reviews and comments since the park",
		ref, strings.ToLower(r.Kind), r.By, r.At.UTC().Format(time.RFC3339))
}

// NoteCloses is what closed each note of ids among the closing events evs,
// oldest first, the last one winning: its answer word for word, or how it
// closed. Each note is named <central>#<n>.
func NoteCloses(evs []state.Event, ids []int, central string) map[int]string {
	closes := map[int]string{}
	for _, e := range evs {
		for _, id := range ids {
			if an, ok := Cmd.ParseAnswered(e); ok {
				if an.ID == id {
					closes[id] = fmt.Sprintf("note %s#%d answered by %s: %s", central, id, an.For, an.Answer)
					if an.Comment != "" {
						closes[id] += " (" + an.Comment + ")"
					}
				}
			} else if strings.HasPrefix(e.Detail, fmt.Sprintf("#%d ", id)) {
				closes[id] = fmt.Sprintf("note %s#%d closed: %s", central, id, Cmd.OneLine(e.Detail))
			}
		}
	}
	return closes
}

// settledCentralParks reads which of parked, each parked on a central
// note, settled: its note is no longer among the central instance's open
// notes (note_list); what closed it is the feed's (read_feed), read only
// then. An unreachable central instance leaves them parked, said once.
func (w *Watcher) settledCentralParks(ctx context.Context, parked []state.Agent) []SettledPark {
	if len(parked) == 0 {
		return nil
	}
	const key = "park-central"
	me := state.Party{Name: w.Host.Cfg().Identity.Person, Host: w.Host.Cfg().Identity.Host}
	notes, err := w.Host.CentralNotes(ctx, me)
	if err != nil {
		w.Fail(key, "cannot read the central instance's notes (%s) the parked agents wait on: %v", w.Host.Cfg().Central.Context, err)
		return nil
	}
	open := map[int]bool{}
	for _, n := range notes {
		open[n.ID] = true
	}
	var closed []int
	for _, ag := range parked {
		if _, id, _ := Cmd.ParkCentralNote(ag.Park.On); !open[id] {
			closed = append(closed, id)
		}
	}
	if len(closed) == 0 {
		w.Clear(key)
		return nil
	}
	var f feed.Feed
	if _, err := w.Host.CallCentralRaw(ctx, me, nameReadFeed, map[string]any{}, &f); err != nil {
		w.Fail(key, "cannot read the central feed (%s) for what closed the notes the parked agents wait on: %v", w.Host.Cfg().Central.Context, err)
		return nil
	}
	w.Clear(key)
	var evs []state.Event
	for _, ev := range f.Events {
		detail, ok := strings.CutPrefix(ev.Line, feed.Line(ev.Kind, ev.Actor, ""))
		if ok && Cmd.NoteClosing(ev.Kind) {
			evs = append(evs, state.Event{At: ev.Time, Verb: ev.Kind, Detail: detail, By: state.Party{Name: ev.Actor.Name, Person: ev.Actor.Person, Host: ev.Actor.Host}})
		}
	}
	closes := NoteCloses(evs, closed, w.Host.Cfg().Central.Context)
	var out []SettledPark
	for _, ag := range parked {
		if _, id, _ := Cmd.ParkCentralNote(ag.Park.On); !open[id] {
			out = append(out, SettledPark{ag.Party, ag.Park.On, cmp.Or(closes[id], fmt.Sprintf("note %s is closed", ag.Park.On))})
		}
	}
	return out
}

// MarkResumable marks the parks of st among settled resumable and returns
// their watch lines and events: one AGENT RESUMABLE line per park, since
// the mark is in the state. A park ended or moved meanwhile is left alone.
func MarkResumable(st *state.State, settled []SettledPark, auto bool, now time.Time) ([]string, []state.Event) {
	var lines []string
	var evs []state.Event
	mark := func(name string, p *state.Park, s SettledPark, routed bool) {
		// The wake promises a full claim window, even if the grant was
		// nearly expired when this poll observed it.
		if res, ok := Cmd.ParkLease(s.On); ok && strings.Contains(s.Answer, "is free and yours:") {
			for i := range st.Grants {
				if g := &st.Grants[i]; g.Resource == res && g.To.Is(s.Party) {
					g.Reserved = now.UTC()
					break
				}
			}
		}
		p.Resumable, p.Answer = now.UTC(), s.Answer
		next := fmt.Sprintf("beekeeper agents resume %q", name)
		switch {
		case !routed:
			next = unroutedResume
		case auto:
			next = "the watch resumes it with these words"
		}
		lines = append(lines, fmt.Sprintf("AGENT RESUMABLE %q: %s; %s", name, Cmd.Truncate(Cmd.ParkedText(p, now), 400), next))
		evs = append(evs, Cmd.Event(WatchParty, resumableEvent, "%s: %s", name, Cmd.ParkedText(p, now)))
	}
	unsettled := func(party state.Party, p *state.Park, s SettledPark) bool {
		return party.Is(s.Party) && p != nil && p.On == s.On && p.Resumable.IsZero()
	}
	for _, s := range settled {
		for k := range st.Agents {
			if ag := &st.Agents[k]; unsettled(ag.Party, ag.Park, s) {
				mark(ag.Name, ag.Park, s, Cmd.WakeRoute(*ag))
			}
		}
		for k := range st.Parks {
			if p := &st.Parks[k]; unsettled(p.By, p, s) {
				mark(cmp.Or(Cmd.WakeID(p.By), p.By.Name), p, s, true)
			}
		}
	}
	return lines, evs
}

// AutoResume runs in the command, which holds what it needs (watchAutoResume).
func (w *Watcher) AutoResume(ctx context.Context, st *state.State) {
	Cmd.WatchAutoResume(w, ctx, st)
}
