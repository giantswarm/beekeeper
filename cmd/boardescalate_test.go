package cmd

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The names the escalation tests share.
const (
	blockedName    = "Blocked / Waiting ⛔️"
	statusHeldRef  = "o/r#15"
	escalatedTitle = "a blocked task"
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
	e := escalation{Ref: refOne, URL: "https://github.com/o/r/issues/1", Title: escalatedTitle, Why: "Status " + blockedName}
	if err := r.a.escalate(context.Background(), loopParty, e, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 2 {
		t.Fatalf("calls %v, want a timer and a note", r.calls)
	}
	// The timer first: a failed note is retried when it fires.
	timer, note := r.calls[0], r.calls[1]
	if !slices.Equal(note[:6], []string{loopNote, loopAdd, "--for", loopPerson, loopRefFlag, refOne}) ||
		!strings.Contains(note[len(note)-1], "https://github.com/o/r/issues/1 is blocked") {
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
	e := escalation{Ref: refOne, URL: "https://github.com/o/r/issues/1", Title: escalatedTitle, Why: "Status " + blockedName}
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

func TestShellQuote(t *testing.T) {
	if got := shellQuote("it's"); got != `'it'\''s'` {
		t.Errorf("shellQuote = %s", got)
	}
}
