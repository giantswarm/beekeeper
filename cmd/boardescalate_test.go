package cmd

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/post"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The names the escalation tests share.
const (
	blockedName    = "Blocked / Waiting ⛔️"
	statusHeldRef  = "o/r#15"
	escalatedTitle = "a blocked task"
	issueOneURL    = "https://github.com/o/r/issues/1"
	teammateLogin  = "teammate"
	askedState     = "abc123"
	blockerRef     = "o/r#20"
)

func blockedItem(ref, status string) board.Item {
	return board.Item{Ref: ref, URL: "https://github.com/" + strings.Replace(ref, "#", "/issues/", 1), Title: escalatedTitle, OnBoard: true, Status: status}
}

// A blocked item is escalated once: while its re-check timer is open it is
// not escalated again, and when the timer has fired (it closes as it
// fires) an item still blocked is escalated once more; an item that left
// Blocked, or whose recorded blockers all closed before its Status was
// set (the blocker-cleared step offers it), is not.
func TestEscalationsTakeEachBlockedItemOnce(t *testing.T) {
	held := blockedItem(idleRef, blockedName)
	held.Blockers, held.OpenBlockers, held.OpenBlockedBy = 1, 1, []string{"o/r#19"}
	// Its blockers closed after its Status was set: the blocker-cleared
	// step offers it as work.
	cleared := blockedItem("o/r#3", blockedName)
	cleared.Blockers, cleared.StatusSet, cleared.BlockersClosed = 1, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
	// Its Status set after its blockers closed: it holds the item, which no
	// step offers, so it is escalated with its Status as the reason.
	statusHeld := blockedItem(statusHeldRef, blockedName)
	statusHeld.Blockers, statusHeld.BlockersClosed, statusHeld.StatusSet = 1, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
	items := []board.Item{blockedItem(refOne, blockedName), held, cleared, blockedItem("o/r#4", "In Progress ⛏️"), statusHeld}
	refs := func(es []escalation) []string {
		var out []string
		for _, e := range es {
			out = append(out, e.Ref)
		}
		return out
	}
	es := escalations(items, blockedName, nil, nil)
	if got := refs(es); !slices.Equal(got, []string{refOne, idleRef, statusHeldRef}) {
		t.Fatalf("escalated %v, want the three blocked items no step offers", got)
	}
	if es[0].Why != "Status "+blockedName || es[1].Why != "held: blocked by o/r#19; 1 of 1 blockers open" || es[2].Why != "Status "+blockedName {
		t.Errorf("why: %q, %q, %q", es[0].Why, es[1].Why, es[2].Why)
	}
	// Their timers open: nothing again.
	timers := []state.Timer{{ID: 1, What: escalateWhat + refOne}, {ID: 2, What: escalateWhat + idleRef}, {ID: 3, What: escalateWhat + statusHeldRef}}
	if got := escalations(items, blockedName, timers, nil); len(got) != 0 {
		t.Errorf("escalated again while the timers are open: %v", refs(got))
	}
	// #1's timer fired: board escalate o/r#1 escalates it again while it
	// stays blocked, and nothing once it left Blocked.
	fired := timers[1:]
	if got := refs(escalations(items, blockedName, fired, []string{strings.ToUpper(refOne)})); !slices.Equal(got, []string{refOne}) {
		t.Errorf("the fired timer's run escalated %v, want o/r#1 again", got)
	}
	items[0].Status = "Up Next ➡️"
	if got := escalations(items, blockedName, fired, []string{refOne}); len(got) != 0 {
		t.Errorf("escalated an item no longer blocked: %v", refs(got))
	}
}

// An escalation is one note for the person on the item and one timer that
// runs board escalate on it again after board.escalateEvery.
func TestEscalateFilesOneNoteAndARecheckTimer(t *testing.T) {
	r := newLoopRig(t)
	e := escalation{Ref: refOne, URL: issueOneURL, Title: escalatedTitle, Why: "Status " + blockedName}
	if err := r.a.escalate(context.Background(), loopParty, e, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 2 {
		t.Fatalf("calls %v, want a timer and a note", r.calls)
	}
	// The timer first: a failed note is retried when it fires.
	timer, note := r.calls[0], r.calls[1]
	if !slices.Equal(note[:6], []string{loopNote, loopAdd, flagFor, loopPerson, loopRefFlag, refOne}) ||
		note[len(note)-1] != "What unblocks "+issueOneURL+"?" {
		t.Errorf("note %q", note)
	}
	if !slices.Equal(timer[:4], []string{loopTimer, loopAdd, "24h0m0s", escalateWhat + refOne}) || timer[4] != "--run" ||
		!strings.HasSuffix(timer[5], " --as '"+loopParty.Name+"' board escalate 'o/r#1'") {
		t.Errorf("timer %q", timer)
	}
	// Without a person nothing is filed.
	r.a.cfg.Guide.Person = ""
	if err := r.a.escalate(context.Background(), loopParty, e, time.Hour); err == nil || len(r.calls) != 2 {
		t.Errorf("escalated without guide.person: %v, calls %v", err, r.calls)
	}
}

// A note that fails after its timer was added leaves the timer open: the
// item is asked about again when it fires, not on every tidy.
func TestEscalateAddsTheTimerBeforeTheNote(t *testing.T) {
	r := newLoopRig(t)
	r.answer = func(args []string) (string, int) {
		if args[0] == loopNote {
			return "refused", 1
		}
		return "", 0
	}
	e := escalation{Ref: refOne, URL: issueOneURL, Title: escalatedTitle, Why: "Status " + blockedName}
	if err := r.a.escalate(context.Background(), loopParty, e, time.Hour); err == nil {
		t.Fatal("a failed note add is no error")
	}
	if len(r.calls) != 2 || r.calls[0][0] != loopTimer || r.calls[1][0] != loopNote {
		t.Errorf("calls %v, want the timer, then the note", r.calls)
	}
}

// board escalate reads the blocked Status even when board.order names no
// step of it, and leaves the configured order as it is.
func TestEscalateBoardReadsTheBlockedStatus(t *testing.T) {
	b := config.Board{Order: []config.BoardStep{{Name: stepInProgress, Status: []string{"in progress"}}}}
	got := escalateBoard(b)
	if len(b.Order) != 1 || len(got.Order) != 2 || !slices.Equal(got.Order[1].Status, []string{blockedStatus}) {
		t.Errorf("order %+v from %+v", got.Order, b.Order)
	}
}

// The note asks one whole question, whatever the title's length, and names
// every issue by its full URL: the guard refuses a note that asks nothing
// or names owner/repo#n bare, and a refusal escalated nothing.
func TestEscalateNoteAsksAndLinksEveryIssue(t *testing.T) {
	r := newLoopRig(t)
	e := escalation{Ref: "o/wepa#137", URL: "https://github.com/o/wepa/issues/137",
		Title: strings.Repeat("a long title ", 20) + "waits on o/adidas#1512 and #9",
		Why:   "held: blocked by o/x#19; 1 of 1 blockers open"}
	if err := r.a.escalate(context.Background(), loopParty, e, time.Hour); err != nil {
		t.Fatal(err)
	}
	note := r.calls[1]
	question := note[len(note)-1]
	if question != "What unblocks https://github.com/o/wepa/issues/137?" {
		t.Errorf("question %q", question)
	}
	sq := note[slices.Index(note, "--status-quo")+1]
	if got := post.Unlinked(question+" "+sq, ""); len(got) != 0 {
		t.Errorf("status quo %q names issues without their URL: %v", sq, got)
	}
}

// One item the note guard refuses never holds back the next.
func TestEscalateAllGoesOnPastAFailure(t *testing.T) {
	r := newLoopRig(t)
	r.answer = func(args []string) (string, int) {
		if args[0] == loopNote && slices.Contains(args, refOne) {
			return "note for pat refused", 2
		}
		return "", 0
	}
	es := []escalation{{Ref: refOne, URL: issueOneURL}, {Ref: idleRef, URL: "https://github.com/o/r/issues/2"}}
	done, err := r.a.escalateAll(context.Background(), loopParty, es, time.Hour)
	if err == nil || len(done) != 1 || done[0].Ref != idleRef {
		t.Errorf("escalated %+v, err %v: want o/r#2 escalated and o/r#1's failure returned", done, err)
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("it's"); got != `'it'\''s'` {
		t.Errorf("shellQuote = %s", got)
	}
}

// judgedItem is a blocked item since two days that only its Status holds,
// its note's facts read with nothing that holds it from the person or
// explains the block.
func judgedItem(r *loopRig) (escalation, escalateFacts) {
	it := blockedItem(refOne, blockedName)
	it.StatusSet = r.a.now.Add(-48 * time.Hour)
	it.Assignees = []string{"Me"}
	e := escalations([]board.Item{it}, blockedName, nil, nil)[0]
	f := escalateFacts{People: []string{"me"}, Person: loopPerson, Comments: []github.Comment{
		{Login: teammateLogin, Member: true, At: r.a.now.Add(-time.Hour), URL: issueOneURL + "#issuecomment-7"},
		{Login: "taylorbot", Automation: true, At: r.a.now, URL: issueOneURL + "#issuecomment-8"},
	}}
	return e, f
}

// A note names what the item waits on, who, since when and the evidence
// read: its blockers, its assignees, its newest comment by a person.
func TestJudgeNamesWhatWhoSinceAndEvidence(t *testing.T) {
	r := newLoopRig(t)
	e, f := judgedItem(r)
	e.item.Blockers, e.item.OpenBlockers, e.item.OpenBlockedBy = 1, 1, []string{"o/r#19"}
	f.Parked = []state.Agent{{Party: state.Party{Name: "worker-3"}, Park: &state.Park{At: r.a.now, Waits: "decision on the API"}}}
	got := r.a.judge(e, f)
	// Its open blocker explains it: no note, the evidence read all the same.
	if !strings.Contains(got.Skip, "its recorded blockers explain it") || got.Unchanged || got.State == "" {
		t.Fatalf("judged %+v, want it skipped for its blocker with a state", got)
	}
	ev := strings.Join(got.Evidence, "; ")
	for _, want := range []string{
		"since " + e.item.StatusSet.UTC().Format(time.DateOnly) + " (2d",
		"waits on: held: blocked by [r#19](https://github.com/o/r/issues/19)",
		"assigned to Me",
		"worker worker-3 parked on it since",
		"newest comment by a person: teammate on ",
		issueOneURL + "#issuecomment-7",
	} {
		if !strings.Contains(ev, want) {
			t.Errorf("evidence %q lacks %q", ev, want)
		}
	}
	if got.Who != "Me, worker worker-3" {
		t.Errorf("who %q", got.Who)
	}
	// The note carries it, every issue by its URL.
	got.Skip = ""
	if err := r.a.escalate(context.Background(), loopParty, got, time.Hour); err != nil {
		t.Fatal(err)
	}
	note := r.calls[len(r.calls)-1]
	sq := note[slices.Index(note, "--status-quo")+1]
	if !strings.Contains(sq, "Who: Me, worker worker-3. Evidence: in Blocked") || !strings.Contains(sq, issueOneURL+"#issuecomment-7") {
		t.Errorf("status quo %q", sq)
	}
	if got := post.Unlinked(sq, ""); len(got) != 0 {
		t.Errorf("status quo names issues without their URL: %v", got)
	}
}

// An item a teammate is assigned to, one the person's queue holds a note
// on, one the person answered since it was blocked and one whose newest
// comment by a person is a customer's file no note.
func TestJudgeSkipsWhatThePersonDoesNotHold(t *testing.T) {
	r := newLoopRig(t)
	for _, tc := range []struct {
		name string
		edit func(*escalation, *escalateFacts)
		skip string
	}{
		{"teammate", func(e *escalation, _ *escalateFacts) { e.item.Assignees = []string{"me", teammateLogin} }, "assigned to " + teammateLogin},
		{"open note", func(e *escalation, f *escalateFacts) {
			f.Notes = []state.Note{{ID: 4, For: strings.ToUpper(loopPerson), Refs: []string{strings.ToUpper(refOne)}}}
		}, "note #4 for"},
		{"held by a note", func(e *escalation, f *escalateFacts) {
			f.Notes = []state.Note{{ID: 5, For: loopPerson, Held: &state.ItemHold{Ref: refOne}}}
		}, "note #5 for"},
		{"answered", func(e *escalation, f *escalateFacts) {
			f.Answers = []answered{{About: refOne, For: loopPerson, Answer: "wait for the customer", At: r.a.now.Add(-time.Hour)}}
		}, `answered on its thread`},
		{"customer", func(e *escalation, f *escalateFacts) {
			f.Comments = append(f.Comments, github.Comment{Login: "customer", At: r.a.now, URL: issueOneURL + "#issuecomment-9"})
		}, "customer's, outside the organisation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, f := judgedItem(r)
			tc.edit(&e, &f)
			got := r.a.judge(e, f)
			if !strings.Contains(got.Skip, tc.skip) {
				t.Errorf("skip %q, want %q", got.Skip, tc.skip)
			}
		})
	}
	// Not skipped: an answer from before its Status was set, another
	// person's note, an answer on another thread.
	e, f := judgedItem(r)
	f.Answers = []answered{
		{About: refOne, For: loopPerson, At: e.item.StatusSet.Add(-time.Hour)},
		{About: idleRef, For: loopPerson, At: r.a.now},
	}
	f.Notes = []state.Note{{ID: 6, For: "someone else", Refs: []string{refOne}}}
	if got := r.a.judge(e, f); got.Skip != "" {
		t.Errorf("skipped %q", got.Skip)
	}
}

// The same blocked item is asked about once while it is unchanged: a
// re-check that finds the state its note was filed on files nothing, and
// a new comment by a person, a new assignee or blocker is a change; an
// automation's comment is none.
func TestJudgeAsksOnceWhileUnchanged(t *testing.T) {
	r := newLoopRig(t)
	e, f := judgedItem(r)
	first := r.a.judge(e, f)
	f.Asked = first.State
	f.Comments = append(f.Comments, github.Comment{Login: "renovate[bot]", Automation: true, At: r.a.now, URL: issueOneURL + "#issuecomment-10"})
	if got := r.a.judge(e, f); !got.Unchanged {
		t.Errorf("an automation's comment changed the state: %+v", got)
	}
	changed := []func(*escalation, *escalateFacts){
		func(_ *escalation, f *escalateFacts) {
			f.Comments = append(f.Comments, github.Comment{Login: "me", At: r.a.now, URL: issueOneURL + "#issuecomment-11"})
		},
		func(e *escalation, _ *escalateFacts) { e.item.OpenBlockedBy = []string{blockerRef} },
		func(e *escalation, _ *escalateFacts) { e.item.StatusSet = r.a.now },
	}
	for i, edit := range changed {
		e, f := judgedItem(r)
		f.Asked = first.State
		edit(&e, &f)
		if got := r.a.judge(e, f); got.Unchanged || got.Skip != "" {
			t.Errorf("change %d: %+v, want it asked", i, got)
		}
	}
}

// A note's timer carries the state it was filed on; a skipped or unchanged
// item gets its re-check timer and no note.
func TestEscalateTimerCarriesTheAskedState(t *testing.T) {
	r := newLoopRig(t)
	e := escalation{Ref: refOne, URL: issueOneURL, Title: escalatedTitle, Why: "Status " + blockedName, State: askedState}
	if err := r.a.escalate(context.Background(), loopParty, e, time.Hour); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 2 || !strings.HasSuffix(r.calls[0][5], " board escalate --asked '"+askedState+"' 'o/r#1'") {
		t.Fatalf("calls %q", r.calls)
	}
	for _, tc := range []struct {
		e      escalation
		suffix string
	}{
		{escalation{Ref: refOne, URL: issueOneURL, State: askedState, Skip: "assigned to " + teammateLogin}, " board escalate 'o/r#1'"},
		{escalation{Ref: refOne, URL: issueOneURL, State: askedState, Unchanged: true}, " board escalate --asked '" + askedState + "' 'o/r#1'"},
	} {
		r.calls = nil
		if err := r.a.escalate(context.Background(), loopParty, tc.e, time.Hour); err != nil {
			t.Fatal(err)
		}
		if len(r.calls) != 1 || r.calls[0][0] != loopTimer || !strings.HasSuffix(r.calls[0][5], tc.suffix) {
			t.Errorf("calls %q, want only the timer ending %q", r.calls, tc.suffix)
		}
	}
}

// A skip keeps the state the last note was filed on: fire, skip (the
// person's queue holds a note on it), fire again unchanged once that note
// closed without an answer: no note is filed.
func TestEscalateSkipKeepsTheAskedState(t *testing.T) {
	r := newLoopRig(t)
	e, f := judgedItem(r)
	asked := r.a.judge(e, f).State
	// Fire: a note on it is open, so it is skipped, and its re-check keeps
	// the asked state.
	f.Asked = asked
	f.Notes = []state.Note{{ID: 4, For: loopPerson, Refs: []string{refOne}}}
	skipped := r.a.judge(e, f)
	if skipped.Skip == "" || skipped.Asked != asked {
		t.Fatalf("judged %+v, want a skip that keeps %s", skipped, asked)
	}
	if err := r.a.escalate(context.Background(), loopParty, skipped, time.Hour); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 1 || !strings.HasSuffix(r.calls[0][5], " board escalate --asked '"+asked+"' 'o/r#1'") {
		t.Fatalf("calls %q, want only the timer carrying %s", r.calls, asked)
	}
	// Fire again: the note closed unanswered, the item unchanged.
	f.Notes = nil
	again := r.a.judge(e, f)
	if !again.Unchanged {
		t.Fatalf("judged %+v, want it unchanged", again)
	}
	r.calls = nil
	if err := r.a.escalate(context.Background(), loopParty, again, time.Hour); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 1 || r.calls[0][0] != loopTimer {
		t.Errorf("calls %q, want only the timer and no note", r.calls)
	}
}

// The blocks of https://github.com/giantswarm/beekeeper/issues/2142: an
// item with an open recorded blocker, one whose worker parked on the GitHub
// gate or on its pull request, and one whose own comment named its blocker
// are explained, and no note reaches the person. What explains nothing (a
// comment naming only the item, one older than the block, a worker parked
// on another item, an unparked worker) leaves it asked.
func TestJudgeSkipsWhatExplainsTheBlock(t *testing.T) {
	r := newLoopRig(t)
	const workerName = "worker-7"
	worker := func(task, on string) state.Agent {
		return state.Agent{Party: state.Party{Name: workerName, Session: "s-7"}, Task: task, Park: &state.Park{At: r.a.now, On: on, Waits: "the GitHub gate"}}
	}
	comment := func(e escalation, body string, before time.Duration) github.Comment {
		return github.Comment{Login: "desk-bot", Automation: true, At: e.item.StatusSet.Add(-before), URL: issueOneURL + "#issuecomment-12", Body: body}
	}
	for _, tc := range []struct {
		name string
		edit func(*escalation, *escalateFacts, *state.State)
		skip string
	}{
		{"open blocker", func(e *escalation, _ *escalateFacts, _ *state.State) {
			e.item.Blockers, e.item.OpenBlockers, e.item.OpenBlockedBy = 1, 1, []string{blockerRef}
		}, "held: blocked by [r#20](https://github.com/o/r/issues/20); 1 of 1 blockers open: its recorded blockers explain it"},
		{"worker on the gate, its task names the item", func(_ *escalation, _ *escalateFacts, st *state.State) {
			st.Agents = []state.Agent{worker("Work the board item "+issueOneURL+" in o/r.", "hold:github")}
		}, "worker " + workerName + " is parked on it (hold:github)"},
		{"worker on its pull request, its session serves the item", func(_ *escalation, _ *escalateFacts, st *state.State) {
			st.Agents = []state.Agent{worker("another task", "o/r#30")}
			st.Records = []state.Record{{Session: state.Party{Session: "s-7"}, Issue: issueOneURL}}
		}, "worker " + workerName + " is parked on it (o/r#30)"},
		{"worker parked on the item", func(_ *escalation, _ *escalateFacts, st *state.State) {
			st.Agents = []state.Agent{worker("", refOne)}
		}, "is parked on it (" + refOne + ")"},
		{"its comment names its blocker", func(e *escalation, f *escalateFacts, _ *state.State) {
			f.Comments = append(f.Comments, comment(*e, "Blocked until https://github.com/o/s/issues/1348 lands (closed since).", time.Hour))
		}, "its comment " + issueOneURL + "#issuecomment-12 (desk-bot, "},
		{"its comment names a pull request by its ref", func(e *escalation, f *escalateFacts, _ *state.State) {
			f.Comments = append(f.Comments, comment(*e, "waits on o/r#30 and "+refOne, -time.Hour))
		}, "names what holds it, [r#30](https://github.com/o/r/issues/30)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, f := judgedItem(r)
			st := &state.State{}
			tc.edit(&e, &f, st)
			if f.Parked == nil {
				f.Parked = agentsParkedOn(st, e.Ref, e.URL)
			}
			got := r.a.judge(e, f)
			if !strings.Contains(got.Skip, tc.skip) {
				t.Errorf("skip %q, want %q", got.Skip, tc.skip)
			}
		})
	}
	for _, tc := range []struct {
		name string
		edit func(*escalation, *escalateFacts, *state.State)
	}{
		{"comment naming only the item", func(e *escalation, f *escalateFacts, _ *state.State) {
			f.Comments = append(f.Comments, comment(*e, "still blocked: "+issueOneURL+" and "+refOne, 0))
		}},
		{"comment older than the block", func(e *escalation, f *escalateFacts, _ *state.State) {
			f.Comments = append([]github.Comment{comment(*e, "waits on o/r#30", 48*time.Hour)}, f.Comments...)
		}},
		{"worker parked on another item", func(_ *escalation, _ *escalateFacts, st *state.State) {
			st.Agents = []state.Agent{worker("Work the board item "+issueOneURL+"0 in o/r.", "hold:github")}
		}},
		{"worker on the item, not parked", func(_ *escalation, _ *escalateFacts, st *state.State) {
			ag := worker("Work the board item "+issueOneURL+" in o/r.", "")
			ag.Park = nil
			st.Agents = []state.Agent{ag}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, f := judgedItem(r)
			st := &state.State{}
			tc.edit(&e, &f, st)
			f.Parked = agentsParkedOn(st, e.Ref, e.URL)
			if got := r.a.judge(e, f); got.Skip != "" {
				t.Errorf("skipped %q, want it asked", got.Skip)
			}
		})
	}
}
