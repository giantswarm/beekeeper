package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/state"
)

const noteDeliveryWords = "Use the fixture"

func TestNoteDeliveryKeepsAnswerOutcome(t *testing.T) {
	a, _ := noteApp(t)
	n := state.Note{ID: 1, By: state.Party{Session: offID, Name: agentOne}, Text: laneQuestion}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Notes = []state.Note{n}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	previous := wakeOwner
	t.Cleanup(func() { wakeOwner = previous })
	failure := errors.New("session unavailable")
	wakeOwner = func(*app, context.Context, state.Party, string, string, string) error { return failure }
	line := answeredLine(n.ID, notePerson, noteDeliveryWords)
	if got := a.tellFiler(t.Context(), state.Party{}, &n, line); !strings.Contains(got, failure.Error()) {
		t.Fatalf("failure not reported: %q", got)
	}
	check := func(want string) {
		t.Helper()
		shown, err := a.localNote(n.ID)
		if err != nil || shown == nil || shown.Delivery == nil || shown.Delivery.Status != want {
			t.Fatalf("notification: %+v, %v, want %s", shown, err, want)
		}
	}
	check("failed")
	failure = nil
	a.tellFiler(t.Context(), state.Party{}, &n, "HOLD RELEASED")
	check("failed") // a later hold message cannot hide a failed answer
	a.tellFiler(t.Context(), state.Party{}, &n, line)
	check("delivered") // an actual successful retry supersedes the failure
}

func TestNoteDeliveryMailboxFailure(t *testing.T) {
	a, _ := noteApp(t)
	a.central = true
	n := state.Note{ID: 1, By: state.Party{Name: agentOne, Host: "test-machine", Team: "test-team"}}
	a.mailFiler = func(context.Context, *state.Note, string) error { return errors.New("mailbox unavailable") }
	got := a.tellFiler(t.Context(), state.Party{}, &n, answeredLine(1, notePerson, noteDeliveryWords))
	if !strings.Contains(got, "notification failed") {
		t.Fatalf("mailbox failure: %q", got)
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == noteDeliveryEvent })
	if err != nil || len(evs) != 1 || !strings.Contains(evs[0].Detail, "mailbox unavailable") || !strings.Contains(evs[0].Detail, `"status":"failed"`) {
		t.Fatalf("retained failure: %+v, %v", evs, err)
	}
}
