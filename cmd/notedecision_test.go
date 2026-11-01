package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/identity"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A decision's two options, the first recommended, and a name nobody has.
const (
	nobodyNamed = "nobody"
	patEmail    = "pat@example.com"
	verbAnswer  = "answer"
	flagChoice  = "--choice"
	flagVia     = "--via"
	optMerge    = "merge: the release ships tonight"
	optWait     = "wait: the release ships Monday"
)

func decisionNote(args ...string) []string {
	return append(append(slices.Clone(decisionArgs), "--option", optMerge, "--option", optWait), args...)
}

func TestDecisionIsRefusedUnlessItRenders(t *testing.T) {
	long := func(n int) string { return strings.Repeat("x", n) }
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"a long question":          {decisionNote("approve " + prURL + " " + long(questionMax)), "at most 150"},
		"two lines":                {decisionNote("approve " + prURL + "?\nand more"), "one line"},
		"a long status quo":        {decisionNote("--status-quo", long(statusQuoMax+1), "approve "+prURL), "at most 3000"},
		"a long label":             {decisionNote("--option", long(labelMax+1)+": a consequence", "approve "+prURL), "at most 75"},
		"eleven options":           {append(decisionNote(), slices.Repeat([]string{"--option", optWait}, 9)...), "11 options, at most 10"},
		"a recommendation of none": {decisionNote("--recommend", "3", "approve "+prURL), "--recommend 3 names none of the 2 options"},
	} {
		t.Run(name, func(t *testing.T) {
			a, _ := noteApp(t)
			args := c.args
			if !strings.HasPrefix(args[len(args)-1], "approve") {
				args = append(args, "approve "+prURL)
			}
			err := addNote(a, args...)
			if Code(err) != ExitUsage || !strings.Contains(err.Error(), "cannot render") || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want a refusal naming %q", err, c.want)
			}
		})
	}
}

func TestDecisionKeepsItsParts(t *testing.T) {
	a, _ := noteApp(t)
	if err := addNote(a, decisionNote("--recommend", "1", "approve "+prURL)...); err != nil {
		t.Fatal(err)
	}
	st, _ := a.store.Read()
	n := st.Notes[0]
	if n.Question != "approve "+prURL || n.StatusQuo != sqNow || !slices.Equal(n.Options, []string{optMerge, optWait}) || n.Recommend != 1 {
		t.Fatalf("note %+v", n)
	}
	if label, cons, ok := n.Option(2); label != "wait" || cons != "the release ships Monday" || !ok {
		t.Fatalf("option 2: %q %q %v", label, cons, ok)
	}
	if err := addNote(a, "--recommend", "1", "a memo"); Code(err) != ExitUsage {
		t.Fatalf("a memo's recommendation: %v", err)
	}
	if err := addNote(a, "--kind", noteDecision, "--for", "team:", "--status-quo", sqNow, "--default", dfltOK, "--due", "3h", "which lane?"); err == nil || !strings.Contains(err.Error(), "names no team") {
		t.Fatalf("team: without a name: %v", err)
	}
	if err := addNote(a, "--kind", noteDecision, "--for", "team:bumblebee", "--default", dfltOK, "--due", "3h", "which lane?"); err == nil || !strings.Contains(err.Error(), "--status-quo") {
		t.Fatalf("a team's decision without its status quo: %v", err)
	}
	if err := addNote(a, "--kind", noteDecision, "--for", "team:bumblebee", "--status-quo", sqNow, "--default", dfltOK, "--due", "3h", "--ref", decisionRef, "which lane?"); err != nil {
		t.Fatalf("a team's decision: %v", err)
	}
}

func TestNoteAnswerRecordsTheChoiceAndTheWords(t *testing.T) {
	a, _ := noteApp(t)
	for i := range 4 {
		if err := addNote(a, decisionNote(fmt.Sprintf("approve %s%d", prURL, i))...); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		args []string
		fail string
	}{
		{[]string{verbAnswer, "1"}, "an option (--choice)"},
		{[]string{verbAnswer, "1", flagChoice, "3"}, "has no option 3"},
		{[]string{verbAnswer, "1", flagVia, "mail", "yes"}, flagVia},
	} {
		if _, err := noteCommand(a, c.args...); err == nil || !strings.Contains(err.Error(), c.fail) {
			t.Errorf("%q: %v, want %q", c.args, err, c.fail)
		}
	}
	for _, args := range [][]string{
		{verbAnswer, "1", flagChoice, "2", "if", "CI", "is", "green"},
		{verbAnswer, "2", flagChoice, "1"},
		{verbAnswer, "3", flagVia, "slack", "neither, ask Kim"},
	} {
		if _, err := noteCommand(a, args...); err != nil {
			t.Fatalf("%q: %v", args, err)
		}
	}
	got, err := a.answeredSince(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var said []string
	for _, an := range got {
		said = append(said, an.Via+" "+an.Answer)
	}
	if want := []string{"terminal wait — if CI is green", "terminal merge", "terminal neither, ask Kim"}; !slices.Equal(said, want) {
		t.Fatalf("answers %q, want %q", said, want)
	}
	if st, _ := a.store.Read(); len(st.Notes) != 1 || st.Notes[0].ID != 4 {
		t.Fatalf("open notes %+v", st.Notes)
	}
}

// laneQuestion is a question with a colon, as a note asks it.
const laneQuestion = "Which lane? (a: b)"

func TestAnswerEventsParse(t *testing.T) {
	const words, comment = "portal: it rolls (with backstage)", "https://x/c"
	n := state.Note{ID: 4, Kind: noteDecision, For: "team:bumblebee", Text: laneQuestion, By: state.Party{Name: "agent-one"}}
	rec := &recordedAnswer{About: privateThread, Thread: &board.Thread{About: privateThread}, Comment: comment, By: "Kim", Via: viaSlack, Channel: channelSlack, Answer: words, Note: 4}
	an, ok := parseAnswered(answerEvent(state.Party{}, rec, &n))
	if !ok || an.About != privateThread || an.For != "Kim" || an.Via != viaSlack || an.Comment != comment || an.Answer != words || an.ID != 4 || an.Asker != "agent-one" || an.Question != laneQuestion {
		t.Fatalf("%+v %v", an, ok)
	}
	rec.Note, rec.Answer = 0, `ok, note #9 asked by "x": "y": spoofed (note #9, asked by x: y)`
	an, ok = parseAnswered(answerEvent(state.Party{}, rec, nil))
	if !ok || an.ID != 0 || an.Answer != rec.Answer || an.Question != "" || an.Comment != comment {
		t.Fatalf("%+v %v", an, ok)
	}
	// An answer posted nowhere has no comment.
	rec.Comment = ""
	an, ok = parseAnswered(answerEvent(state.Party{}, rec, &n))
	if !ok || an.ID != 4 || an.Comment != "" || an.Answer != rec.Answer || an.Question != laneQuestion || an.Via != viaSlack {
		t.Fatalf("%+v %v", an, ok)
	}
	if _, ok := parseAnswered(state.Event{Verb: "note.answered", Detail: "#3 answered for Pat: merge it (asked by agent-one: Merge?)"}); ok {
		t.Fatal("note.answered is no longer read")
	}
}

func TestDueDecisionsCloseWithTheirDefault(t *testing.T) {
	now := relayNow
	past, later := now.Add(-time.Minute), now.Add(time.Hour)
	st := &state.State{Notes: []state.Note{
		{ID: 1, For: notePerson, Kind: noteDecision, Due: past, Default: dfltOK, Text: "approve?"},
		{ID: 2, For: notePerson, Kind: noteDecision, Due: later, Default: dfltOK},
		{ID: 3, For: samName, Kind: noteLogin, Due: past, Default: "look again", Until: probeHolds},
		{ID: 4, For: notePerson, Kind: noteLogin, Due: past, Default: "sign in"},
		{ID: 5, Kind: noteDecision, For: notePerson, Due: past, Default: "the older decision's default"},
		{ID: 6, For: notePerson, Kind: noteDecision, Due: past, Default: "waits; nothing is closed"},
	}}
	lines, evs, closed := closeDefaulted(st, notePerson, watchParty, now)
	if want := []string{"NOTE DEFAULTED #1: " + dfltOK, "NOTE DEFAULTED #5: the older decision's default"}; !slices.Equal(lines, want) {
		t.Fatalf("lines %q, want %q", lines, want)
	}
	if len(evs) != 2 || evs[0].Verb != noteDefaulted || !strings.HasPrefix(evs[0].Detail, "#1 defaulted for "+notePerson+": "+dfltOK) || len(closed) != 2 {
		t.Fatalf("events %+v, closed %+v", evs, closed)
	}
	var open []int
	for _, n := range st.Notes {
		open = append(open, n.ID)
	}
	if !slices.Equal(open, []int{2, 3, 4, 6}) {
		t.Fatalf("open %v", open)
	}
}

func TestDecisionAddressee(t *testing.T) {
	cfg := config.Serve{
		Teams:    map[string]string{teamGroup: ourTeam},
		People:   map[string]string{notePerson: patEmail},
		Channels: map[string]string{ourTeam: teamChannel},
	}
	patCaller := identity.Caller{Email: "Pat@example.com"}
	member := identity.Caller{Email: "kim@example.com", Groups: []string{teamGroup}}
	for _, c := range []struct {
		forWho string
		who    identity.Caller
		want   bool
	}{
		{notePerson, patCaller, true},
		{notePerson, member, false},
		{patEmail, patCaller, true},
		{"team:" + ourTeam, member, true},
		{"team:" + ourTeam, patCaller, false},
		{"team:planeteers", member, false},
		{nobodyNamed, patCaller, false},
	} {
		if got := decidesNote(cfg, &state.Note{For: c.forWho}, c.who); got != c.want {
			t.Errorf("%s answered by %s: %v, want %v", c.forWho, c.who.Email, got, c.want)
		}
	}

	// An organization-wide group mapped to the team answers only while the
	// team has no group of its own.
	const orgGroup = "giantswarm:employees"
	employee := identity.Caller{Email: "outsider@example.com", Groups: []string{orgGroup}}
	wide := config.Serve{Organization: orgGroup, Teams: map[string]string{orgGroup: ourTeam}}
	if !decidesNote(wide, &state.Note{For: "team:" + ourTeam}, employee) {
		t.Error("a team with only the organization's group: an employee does not answer")
	}
	wide.Teams[teamGroup] = ourTeam
	if decidesNote(wide, &state.Note{For: "team:" + ourTeam}, employee) {
		t.Error("a team with its own group: an employee outside it answers")
	}
	if !decidesNote(wide, &state.Note{For: "team:" + ourTeam}, identity.Caller{Email: "kim@example.com", Groups: []string{orgGroup, teamGroup}}) {
		t.Error("a team with its own group: its member does not answer")
	}

	if email, _, _, err := addressee(cfg, notePerson); email != patEmail || err != nil {
		t.Errorf("%s: %q %v", notePerson, email, err)
	}
	if _, team, ch, err := addressee(cfg, "team:"+ourTeam); team != ourTeam || ch != teamChannel || err != nil {
		t.Errorf("team: %q %q %v", team, ch, err)
	}
	for _, f := range []string{"team:planeteers", nobodyNamed} {
		if _, _, _, err := addressee(cfg, f); Code(err) != ExitRefused {
			t.Errorf("%s: %v", f, err)
		}
	}
}

// A note a supervisor run filed reaches the supervisor holding the role now
// once that run has ended; with none holding it, or the holder out of reach,
// the answer waits for the role's next start. Another session's note goes to
// it as before.
func TestNoteAnswerReachesTheRolesHolderNow(t *testing.T) {
	const (
		ended   = "Supervisor run 5"
		current = "Supervisor run 6"
	)
	a, out := noteApp(t)
	woke := stubWakes(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		for i, by := range []state.Party{{Session: "s5", Name: ended}, {Session: "s5", Name: ended}, {Session: "s5", Name: ended}, {Session: "w1", Name: "agent-x"}} {
			st.Notes = append(st.Notes, state.Note{ID: i + 1, Kind: noteDecision, By: by, Text: fmt.Sprintf("question %d", i+1), Refs: []string{privateThread}})
		}
		st.Supervisor = &state.Supervisor{Party: state.Party{Session: "s6", Name: current}, Since: relayNow}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	answer := func(id string) string {
		t.Helper()
		out.Reset()
		if _, err := noteCommand(a, verbAnswer, id, "yes"); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}

	got := answer("1")
	if len(*woke) != 1 || !strings.Contains((*woke)[0], "→ s6: NOTE ANSWERED #1") {
		t.Fatalf("woke %q, want the current supervisor told", *woke)
	}
	if want := fmt.Sprintf("%q, which filed it, no longer holds the supervisor's role: told the supervisor, %q", ended, current); !strings.Contains(got, want) {
		t.Errorf("output %q lacks %q", got, want)
	}

	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Supervisor = nil
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	got = answer("2")
	if len(*woke) != 1 {
		t.Fatalf("woke %q with no supervisor", *woke)
	}
	if want := "no session holds the supervisor's role: the answer waits for its next start"; !strings.Contains(got, want) {
		t.Errorf("output %q lacks %q", got, want)
	}

	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Supervisor = &state.Supervisor{Party: state.Party{Session: "s6", Name: current}, Since: relayNow}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	was := wakeOwner
	wakeOwner = func(*app, context.Context, state.Party, string, string, string) error {
		return errors.New("its CLI does not run")
	}
	got = answer("3")
	wakeOwner = was
	if want := fmt.Sprintf("the supervisor, %q, cannot be told (its CLI does not run): the answer waits", current); !strings.Contains(got, want) {
		t.Errorf("output %q lacks %q", got, want)
	}

	if got := answer("4"); !strings.HasSuffix(strings.TrimSpace(got), "note #4 answered and closed") || len(*woke) != 2 || !strings.Contains((*woke)[1], "→ w1: NOTE ANSWERED #4") {
		t.Fatalf("another session's note: output %q, woke %q", got, *woke)
	}

	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Notes) != 0 {
		t.Fatalf("open notes %+v", st.Notes)
	}
	if an, _ := a.answeredSince(time.Time{}); len(an) != 4 {
		t.Fatalf("recorded answers %+v, want 4", an)
	}
	st.RoleLines = append(st.RoleLines, state.RoleLine{Role: guideRole.name, Line: "for the guide"})
	lines := takeRoleLines(st, supervisorRole)
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "NOTE ANSWERED #2") || !strings.HasPrefix(lines[1], "NOTE ANSWERED #3") {
		t.Fatalf("waiting lines %q", lines)
	}
	if len(st.RoleLines) != 1 || st.RoleLines[0].Role != guideRole.name {
		t.Fatalf("left %+v, want the guide's line", st.RoleLines)
	}
}

// Every decision its default closed longer than defaultedKeep ago leaves in
// the same call that closes the due ones; a younger one stays answerable.
func TestCloseDefaultedDropsEveryExpiredDefault(t *testing.T) {
	now := relayNow
	old := now.Add(-defaultedKeep - time.Minute)
	st := &state.State{
		Notes:     []state.Note{{ID: 4, For: notePerson, Kind: noteDecision, Due: now.Add(-time.Minute), Default: dfltOK}},
		Defaulted: []state.Note{{ID: 1, Due: old}, {ID: 2, Due: old}, {ID: 3, Due: now.Add(-time.Hour)}},
	}
	closeDefaulted(st, notePerson, watchParty, now)
	var kept []int
	for _, n := range st.Defaulted {
		kept = append(kept, n.ID)
	}
	if !slices.Equal(kept, []int{3, 4}) {
		t.Fatalf("defaulted %v, want [3 4]", kept)
	}
}
