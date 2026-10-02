package cmd

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/identity"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A decision's two options, the first recommended, and a name nobody has.
const (
	nobodyNamed = "nobody"
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
	if err := addNote(a, "--kind", noteDecision, "--for", "team:bumblebee", "--status-quo", sqNow, "--default", dfltOK, "--due", "3h", "which lane?"); err != nil {
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
		{[]string{"answer", "1"}, "an option (--choice)"},
		{[]string{"answer", "1", "--choice", "3"}, "has no option 3"},
		{[]string{"answer", "1", "--via", "mail", "yes"}, "--via"},
	} {
		if _, err := noteCommand(a, c.args...); err == nil || !strings.Contains(err.Error(), c.fail) {
			t.Errorf("%q: %v, want %q", c.args, err, c.fail)
		}
	}
	for _, args := range [][]string{
		{"answer", "1", "--choice", "2", "if", "CI", "is", "green"},
		{"answer", "2", "--choice", "1"},
		{"answer", "3", "--via", "slack", "neither, ask Kim"},
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
	if want := []string{"cli wait — if CI is green", "cli merge", "slack neither, ask Kim"}; !slices.Equal(said, want) {
		t.Fatalf("answers %q, want %q", said, want)
	}
	if st, _ := a.store.Read(); len(st.Notes) != 1 || st.Notes[0].ID != 4 {
		t.Fatalf("open notes %+v", st.Notes)
	}
}

func TestAnsweredEventsBeforeViaStillParse(t *testing.T) {
	e := state.Event{Verb: noteAnswered, Detail: "#3 answered for Pat: merge it (asked by agent-one: Merge? Status quo: x.)"}
	an, ok := parseAnswered(e)
	if !ok || an.ID != 3 || an.For != "Pat" || an.Via != "" || an.Answer != "merge it" {
		t.Fatalf("%+v %v", an, ok)
	}
	n := state.Note{ID: 4, For: "team:bumblebee", Text: "Which lane?", By: state.Party{Name: "agent-one"}}
	an, ok = parseAnswered(answeredEvent(state.Party{}, &n, viaSlack, "portal: it rolls with backstage"))
	if !ok || an.For != "team:bumblebee" || an.Via != viaSlack || an.Answer != "portal: it rolls with backstage" || an.Question != "Which lane?" {
		t.Fatalf("%+v %v", an, ok)
	}
}

func TestDueDecisionsCloseWithTheirDefault(t *testing.T) {
	now := relayNow
	past, later := now.Add(-time.Minute), now.Add(time.Hour)
	st := &state.State{Notes: []state.Note{
		{ID: 1, For: notePerson, Kind: noteDecision, Due: past, Default: dfltOK, Text: "approve?"},
		{ID: 2, For: notePerson, Kind: noteDecision, Due: later, Default: dfltOK},
		{ID: 3, Kind: noteMemo, Due: past, Default: "look again"},
		{ID: 4, For: notePerson, Kind: noteLogin, Due: past, Default: "sign in"},
		{ID: 5, For: notePerson, Due: past, Default: "the older decision's default"},
		{ID: 6, For: notePerson, Kind: noteDecision, Due: past, Default: "waits; nothing is closed"},
	}}
	lines, evs, closed := closeDefaulted(st, notePerson, watchParty, now)
	if want := []string{"NOTE DEFAULTED #1: " + dfltOK, "NOTE DEFAULTED #5: the older decision's default"}; !slices.Equal(lines, want) {
		t.Fatalf("lines %q, want %q", lines, want)
	}
	if len(evs) != 2 || evs[0].Verb != noteDefaulted || !strings.HasPrefix(evs[0].Detail, "#1 defaulted for Pat: "+dfltOK) || len(closed) != 2 {
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
		People:   map[string]string{"Pat": "pat@example.com"},
		Channels: map[string]string{ourTeam: teamChannel},
	}
	pat := identity.Caller{Email: "Pat@example.com"}
	member := identity.Caller{Email: "kim@example.com", Groups: []string{teamGroup}}
	for _, c := range []struct {
		forWho string
		who    identity.Caller
		want   bool
	}{
		{"pat", pat, true},
		{"pat", member, false},
		{"pat@example.com", pat, true},
		{"team:" + ourTeam, member, true},
		{"team:" + ourTeam, pat, false},
		{"team:planeteers", member, false},
		{nobodyNamed, pat, false},
	} {
		if got := decidesNote(cfg, &state.Note{For: c.forWho}, c.who); got != c.want {
			t.Errorf("%s answered by %s: %v, want %v", c.forWho, c.who.Email, got, c.want)
		}
	}
	if email, _, _, err := addressee(cfg, "pat"); email != "pat@example.com" || err != nil {
		t.Errorf("pat: %q %v", email, err)
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
