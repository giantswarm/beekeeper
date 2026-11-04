package cmd

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

func TestWatchDefaultedWakesOncePerSession(t *testing.T) {
	for _, sessions := range []int{1, 2} {
		for _, failed := range []bool{false, true} {
			t.Run(strings.Repeat("session", sessions)+map[bool]string{false: "/success", true: "/failure"}[failed], func(t *testing.T) {
				w, _ := onceWatch(t, openRefs)
				var notes []state.Note
				for session := 0; session < sessions; session++ {
					for i := 0; i < 3; i++ {
						notes = append(notes, state.Note{ID: len(notes) + 1, Kind: noteDecision, For: w.cfg.Guide.Person, Text: "Apply the fixture", Default: "Use the fixture", Due: w.now.Add(-time.Minute), By: state.Party{Session: []string{"fixture-session-a", "fixture-session-b"}[session]}})
					}
				}
				setNotes(t, w, notes...)
				previous := wakeOwner
				t.Cleanup(func() { wakeOwner = previous })
				var mu sync.Mutex
				messages := map[string][]string{}
				wakeOwner = func(_ *app, _ context.Context, _ state.Party, q, msg, _ string) error {
					mu.Lock()
					defer mu.Unlock()
					messages[q] = append(messages[q], msg)
					if failed {
						return errors.New("fixture delivery failed")
					}
					return nil
				}
				w.pending(t.Context(), nil)
				w.timerActs.Wait()
				if len(messages) != sessions {
					t.Fatalf("woke %d sessions, want %d", len(messages), sessions)
				}
				for _, n := range notes {
					got := messages[wakeID(n.By)]
					if len(got) != 1 || strings.Count(got[0], defaultedLine(&n)) != 1 {
						t.Fatalf("note %d: wakes = %q, want one containing its default", n.ID, got)
					}
					shown, err := w.localNote(n.ID)
					want := "delivered"
					if failed {
						want = noteDeliveryFailed
					}
					if err != nil || shown == nil || shown.Delivery == nil || shown.Delivery.Status != want {
						t.Fatalf("note %d delivery: %+v, %v, want %s", n.ID, shown, err, want)
					}
				}
			})
		}
	}
}

func TestWatchDefaultedBatchesCurrentRoleHolder(t *testing.T) {
	w, _ := onceWatch(t, openRefs)
	holder := state.Party{Session: "fixture-current-session", Name: litGuideRun2}
	notes := []state.Note{
		{ID: 1, By: state.Party{Name: firstRun}, Default: "first fixture"},
		{ID: 2, By: holder, Default: "second fixture"},
	}
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide = &state.Role{Holder: &state.Supervisor{Party: holder, Since: w.now}}
		st.Defaulted = notes
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	previous := wakeOwner
	t.Cleanup(func() { wakeOwner = previous })
	var messages []string
	wakeOwner = func(_ *app, _ context.Context, _ state.Party, q, msg, _ string) error {
		if q != wakeID(holder) {
			t.Errorf("recipient = %q, want current holder", q)
		}
		messages = append(messages, msg)
		return nil
	}
	w.tellDefaulted(t.Context(), notes)
	if len(messages) != 1 {
		t.Fatalf("wakes = %q, want one", messages)
	}
	for _, n := range notes {
		if !strings.Contains(messages[0], defaultedLine(&n)) {
			t.Errorf("wake lacks note %d: %q", n.ID, messages[0])
		}
	}
}
