//go:build unix

package cmd

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A person-started session has no roster entry. Its harness id, rather
// than its UUID conversation id or a display name, owns the answer channel.
func TestNoteAnswerOffRosterDelivery(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reader  bool
		display string
	}{
		{name: "case 0: named filer", reader: true, display: "interactive session"},
		{name: "case 1: id only filer", reader: true},
		{name: "case 2: ended session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, out := noteApp(t)
			a.cfg.StateDir = t.TempDir()
			inbox := codex.InboxPath(a.cfg.StateDir, offID)
			if err := codex.MakeInbox(inbox); err != nil {
				t.Fatal(err)
			}
			if tc.reader {
				r, err := os.OpenFile(inbox, os.O_RDWR, 0) //nolint:gosec // synthetic test inbox
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = r.Close() })
			}
			n := state.Note{ID: 1, Kind: noteDecision, For: notePerson, Text: laneQuestion, Refs: []string{decisionRef}, By: state.Party{Session: offID, HostSession: codex.HostPrefix + offID, Name: tc.display, Team: "test-team"}}
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Notes = []state.Note{n}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := noteCommand(a, verbAnswer, "1", "yes"); err != nil {
				t.Fatal(err)
			}
			answerOutput := out.String()
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Agents) != 0 || len(st.Notes) != 0 {
				t.Fatal("answer registered the filer or left the note open")
			}
			wantStatus := "failed"
			if tc.reader {
				wantStatus = "delivered"
				msgs, err := codex.Drain(inbox)
				if err != nil || len(msgs) != 1 || !strings.Contains(msgs[0], "NOTE ANSWERED #1") || !strings.Contains(msgs[0], "yes") {
					t.Fatalf("answer inbox: %q, %v", msgs, err)
				}
			} else if !strings.Contains(answerOutput, "notification failed") {
				t.Fatalf("answer hides failed notification: %s", answerOutput)
			}
			// A fresh command recovers both the answer and delivery status from
			// retained events, including a filer whose team is no longer in the note.
			fresh := *a
			for _, asJSON := range []bool{false, true} {
				out.Reset()
				fresh.json = asJSON
				if _, err := noteCommand(&fresh, noteShowName, "#1"); err != nil {
					t.Fatal(err)
				}
				if asJSON {
					var got shownNote
					if err := json.Unmarshal(out.Bytes(), &got); err != nil {
						t.Fatal(err)
					}
					if got.Status != closedAnswered || got.Delivery == nil || got.Delivery.Status != wantStatus || got.Answer != "yes" {
						t.Fatalf("retained status: %+v", got)
					}
					if !tc.reader && (got.Delivery.Detail == "" || !strings.Contains(got.Delivery.Detail, "no longer runs")) {
						t.Fatalf("failure reason: %+v", got.Delivery)
					}
				} else if !strings.Contains(out.String(), "notification: "+wantStatus) {
					t.Fatalf("status missing: %s", out.String())
				}
			}
		})
	}
}
