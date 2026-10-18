package cmd

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The board's statuses as the fake board names them.
const (
	holdUpNext  = "Up Next"
	holdWorking = "In Progress"
	holdBlocked = "Blocked / Waiting ⛔️"
)

// offBoardRef is an issue that is not on the fake board, verbDone the
// note command that closes a note by hand.
const (
	offBoardRef = "o/r#8"
	verbDone    = "done"
)

// heldRef is the board item the hold tests hold, decisionRef's issue.
const heldRef = decisionRef

// fakeBoard is a board project of owner o whose items are the issues of
// o/r, each with its Status by number; an issue without one is not on the
// board. It answers the reads and the Status writes of a hold and a
// release.
type fakeBoard struct {
	mu     sync.Mutex
	status map[int]string
	writes int
}

var holdStatuses = []string{holdUpNext, holdWorking, holdBlocked, "Done"}

// useFakeBoard puts a fake board with status as the board a's commands
// read and write, configured as o/7 offering its Up Next items.
func useFakeBoard(t *testing.T, a *app, status map[int]string) *fakeBoard {
	t.Helper()
	f := &fakeBoard{status: status}
	was := boardGH
	boardGH = f.gh
	t.Cleanup(func() { boardGH = was })
	a.cfg.Board.Owner, a.cfg.Board.Project = "o", 7
	a.cfg.Board.Order = []config.BoardStep{{Name: "next up", Status: []string{holdUpNext}}}
	return f
}

func (f *fakeBoard) gh(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q, vs := args[3], map[string]string{}
	for _, a := range args[4:] {
		if k, v, ok := strings.Cut(a, "="); ok {
			vs[k] = v
		}
	}
	switch {
	case strings.Contains(q, "fields(first:50)"):
		opts := make([]string, len(holdStatuses))
		for i, s := range holdStatuses {
			opts[i] = fmt.Sprintf(`{"id":"S%d","name":%q}`, i, s)
		}
		return fmt.Appendf(nil, `{"data":{"repositoryOwner":{"projectV2":{"id":"P","fields":{"nodes":[{"id":"FS","name":"Status","options":[%s]}]}}}}}`, strings.Join(opts, ",")), nil
	case strings.Contains(q, "projectItems(first:50)"):
		n, _ := strconv.Atoi(vs["n"])
		items := ""
		if s, ok := f.status[n]; ok {
			items = fmt.Sprintf(`{"id":"IT%d","project":{"id":"P"},"status":{"name":%q,"updatedAt":"2026-09-24T00:00:00Z"}}`, n, s)
		}
		return fmt.Appendf(nil, `{"data":{"repository":{"issue":{"id":"I%d","projectItems":{"nodes":[%s]}}}}}`, n, items), nil
	case strings.Contains(q, "updateProjectV2ItemFieldValue"):
		n, _ := strconv.Atoi(strings.TrimPrefix(vs["i"], "IT"))
		i, _ := strconv.Atoi(strings.TrimPrefix(vs["v"], "S"))
		f.status[n] = holdStatuses[i]
		f.writes++
		return []byte(`{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"x"}}}}`), nil
	}
	return nil, fmt.Errorf("unexpected request %q", q)
}

func (f *fakeBoard) statusOf(n int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status[n]
}

// offered is the item board next picks off the fake board for a: its
// configured order over the items as they stand, "" for none.
func (f *fakeBoard) offered(a *app) string {
	f.mu.Lock()
	snap := &board.Snapshot{Order: a.cfg.Board.Order}
	for n, s := range f.status {
		snap.Items = append(snap.Items, board.Item{Ref: fmt.Sprintf("o/r#%d", n), OnBoard: true, Status: s, Created: relayNow, Updated: relayNow})
	}
	f.mu.Unlock()
	slices.SortFunc(snap.Items, func(x, y board.Item) int { return strings.Compare(x.Ref, y.Ref) })
	res := nextFree(&state.State{}, board.Rank(snap, a.cfg.Board, relayNow), pickScope{me: state.Party{Session: "me"}, alive: func(state.Party) bool { return true }, listed: relayNow})
	if res.Pick == nil {
		return ""
	}
	return res.Pick.Ref
}

// heldDecision is an open decision for Pat on heldRef that holds it, filed
// when the item was Up Next.
const heldDecision = `{"nextNote": 1, "notes": [
	{"id": 1, "for": "Pat", "kind": "decision", "text": "merge o/r#7?", "question": "merge o/r#7?", "refs": ["o/r#7"],
	 "options": ["merge: the release ships tonight", "wait: the release ships Monday"],
	 "default": "the release stays unpublished", "due": "2026-09-26T00:00:00Z", "by": {"name": "Agent one"}, "at": "2026-09-24T00:00:00Z",
	 "held": {"ref": "o/r#7", "status": "Blocked / Waiting ⛔️", "was": "Up Next"}}]}`

func releasedEvents(t *testing.T, a *app) []state.Event {
	t.Helper()
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == boardReleased })
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// The reproduction: an item held Blocked / Waiting on a decision is offered
// by board next again once the decision is answered, with no other command.
func TestAnsweredDecisionReleasesItsHold(t *testing.T) {
	a, out := noteApp(t)
	f := useFakeBoard(t, a, map[int]string{7: holdBlocked, 8: holdWorking})
	writeState(t, a, heldDecision)
	if got := f.offered(a); got != "" {
		t.Fatalf("board next offers %s while the decision holds it", got)
	}
	if _, err := noteCommand(a, verbAnswer, "1", flagChoice, "1"); err != nil {
		t.Fatal(err)
	}
	if got := f.offered(a); got != heldRef {
		t.Fatalf("board next offers %q after the answer, want %s (its Status %q)", got, heldRef, f.statusOf(7))
	}
	if evs := releasedEvents(t, a); len(evs) != 1 || evs[0].Detail != "o/r#7 Blocked / Waiting ⛔️ → Up Next: note #1 answered" {
		t.Errorf("events %+v", evs)
	}
	if !strings.Contains(out.String(), "BOARD RELEASED: o/r#7 back to Up Next, note #1 answered") {
		t.Errorf("said %q", out.String())
	}
}

// note add --holds sets the item Blocked / Waiting and records the Status
// it had; board next skips it until the decision closes.
func TestNoteAddHoldsSetsTheItemBlocked(t *testing.T) {
	a, out := noteApp(t)
	f := useFakeBoard(t, a, map[int]string{7: holdUpNext})
	if err := addNote(a, decisionNote("--holds", "approve "+prURL)...); err != nil {
		t.Fatal(err)
	}
	if s := f.statusOf(7); s != holdBlocked {
		t.Fatalf("Status %q, want %q", s, holdBlocked)
	}
	st, err := a.store.Read()
	if err != nil || len(st.Notes) != 1 {
		t.Fatalf("notes %+v, %v", st, err)
	}
	if h := st.Notes[0].Held; h == nil || *h != (state.ItemHold{Ref: heldRef, Status: holdBlocked, Was: holdUpNext}) {
		t.Fatalf("hold %+v", h)
	}
	if !strings.Contains(out.String(), "holds o/r#7: "+holdBlocked+" until the decision closes, then Up Next again") {
		t.Errorf("said %q", out.String())
	}
	if got := f.offered(a); got != "" {
		t.Errorf("board next offers %s while held", got)
	}
}

// --holds without --ref, on an issue that is not on the board, or on a
// sign-in is refused (exit 3) naming why, nothing written.
func TestNoteAddHoldsRefusals(t *testing.T) {
	noRef := slices.DeleteFunc(slices.Clone(decisionArgs), func(s string) bool { return s == flagRef || s == decisionRef })
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no ref", append(noRef, "--holds", "approve "+prURL+"?"), "--holds needs --ref"},
		{"off the board", []string{flagRef, offBoardRef, flagFor, notePerson, flagStatusQuo, sqNow, flagWhy, whyNow, flagDefault, dfltOK, flagDue, "3h", "--holds", "approve " + prURL + "?"}, offBoardRef + " is not on the board"},
		{"a login", []string{"--kind=" + noteLogin, flagFor, notePerson, "--until", probeHolds, flagRef, decisionRef, "--holds", signIn}, "--holds is a decision's"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := noteApp(t)
			f := useFakeBoard(t, a, map[int]string{7: holdUpNext})
			err := addNote(a, tc.args...)
			if Code(err) != ExitRefused || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("exit %d, %v; want exit %d naming %q", Code(err), err, ExitRefused, tc.want)
			}
			if st, _ := a.store.Read(); len(st.Notes) != 0 || f.writes != 0 {
				t.Errorf("notes %+v, %d board writes", st.Notes, f.writes)
			}
		})
	}
}

// A Status set by hand while the item is held is left as it is, and the
// filer is told why: no board.released.
func TestReleaseLeavesAStatusSetWhileHeld(t *testing.T) {
	a, out := noteApp(t)
	f := useFakeBoard(t, a, map[int]string{7: holdWorking})
	writeState(t, a, heldDecision)
	if _, err := noteCommand(a, verbAnswer, "1", "go ahead"); err != nil {
		t.Fatal(err)
	}
	if s := f.statusOf(7); s != holdWorking || f.writes != 0 {
		t.Fatalf("Status %q after %d writes, want %q untouched", s, f.writes, holdWorking)
	}
	want := "HOLD LEFT: o/r#7 is In Progress, set since note #1 held it " + holdBlocked + ": left as it is, not set back to Up Next"
	if !strings.Contains(out.String(), want) {
		t.Errorf("said %q, want %q", out.String(), want)
	}
	if evs := releasedEvents(t, a); len(evs) != 0 {
		t.Errorf("events %+v", evs)
	}
}

// A note closed by hand or as overtaken releases its hold as an answer
// does.
func TestNoteDoneReleasesItsHold(t *testing.T) {
	for _, tc := range []struct {
		args []string
		how  string
	}{
		{[]string{verbDone, "1"}, closedDone},
		{[]string{verbDone, "1", "--overtaken", "merged meanwhile"}, closedOvertaken},
	} {
		t.Run(tc.how, func(t *testing.T) {
			a, out := noteApp(t)
			f := useFakeBoard(t, a, map[int]string{7: holdBlocked})
			writeState(t, a, heldDecision)
			if _, err := noteCommand(a, tc.args...); err != nil {
				t.Fatal(err)
			}
			if got := f.offered(a); got != heldRef {
				t.Fatalf("offered %q (Status %q)", got, f.statusOf(7))
			}
			if !strings.Contains(out.String(), "back to Up Next, note #1 "+tc.how) {
				t.Errorf("said %q", out.String())
			}
		})
	}
}

// Two decisions holding one item: the second keeps the Status the first
// recorded, and the item stays held until the last of them closes.
func TestItemHeldByTwoDecisionsWaitsForBoth(t *testing.T) {
	a, out := noteApp(t)
	f := useFakeBoard(t, a, map[int]string{7: holdUpNext})
	for _, q := range []string{"approve " + prURL, "ship " + prURL + " tonight?"} {
		if err := addNote(a, decisionNote("--holds", q)...); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := a.store.Read()
	if len(st.Notes) != 2 || st.Notes[1].Held == nil || st.Notes[1].Held.Was != holdUpNext {
		t.Fatalf("notes %+v", st.Notes)
	}
	if _, err := noteCommand(a, verbAnswer, "1", flagChoice, "1"); err != nil {
		t.Fatal(err)
	}
	if s := f.statusOf(7); s != holdBlocked || !strings.Contains(out.String(), "HOLD KEPT: o/r#7 stays "+holdBlocked+" after note #1 answered: note #2 holds it still") {
		t.Fatalf("after the first answer: Status %q, said %q", s, out.String())
	}
	if _, err := noteCommand(a, verbAnswer, "2", flagChoice, "2"); err != nil {
		t.Fatal(err)
	}
	if got := f.offered(a); got != heldRef {
		t.Fatalf("after both: offered %q (Status %q)", got, f.statusOf(7))
	}
}

// A decision closed by its default, or as overtaken, by the watch releases
// its hold with no other command: board next offers the item again.
func TestWatchReleasesTheHoldOfADefaultedOrOvertakenDecision(t *testing.T) {
	for _, tc := range []struct {
		name, ref, how string
		due            time.Duration
	}{
		{"default", github.Open, closedDefaulted, -time.Minute},
		{"overtaken", github.Merged, closedOvertaken, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, out := overtakingWatch(t, map[string]string{heldRef: tc.ref})
			f := useFakeBoard(t, w.app, map[int]string{7: holdBlocked})
			setNotes(t, w, state.Note{ID: 1, Kind: noteDecision, For: notePerson, Text: "merge o/r#7?", Refs: []string{heldRef},
				Default: dfltOK, Due: w.now.Add(tc.due), By: state.Party{Name: agentOne}, TaskEnded: w.now,
				Held: &state.ItemHold{Ref: heldRef, Status: holdBlocked, Was: holdUpNext}})
			w.pending(context.Background(), nil)
			w.timerActs.Wait()
			if ids := openIDs(t, w); len(ids) != 0 {
				t.Fatalf("open %v", ids)
			}
			if got := f.offered(w.app); got != heldRef {
				t.Fatalf("offered %q (Status %q)", got, f.statusOf(7))
			}
			if !strings.Contains(out.String(), "BOARD RELEASED: o/r#7 back to Up Next, note #1 "+tc.how) {
				t.Errorf("watch said %q", out.String())
			}
		})
	}
}
