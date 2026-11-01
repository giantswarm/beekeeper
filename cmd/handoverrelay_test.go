package cmd

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The relay board's kinds, its step, the default of the decision that
// holds an item, and a JSON null.
const (
	kindEpic     = "Epic"
	relayStep    = "work"
	relayDefault = "it waits"
	jsonNull     = "null"
)

// relayIssue is an issue of the relay board: its board fields, its rank (0
// for none) and its open blocker (0 for none).
type relayIssue struct {
	n           int
	title, kind string
	status      string
	rank        int
	blocker     int
}

// relayBoard is the board the relay test reads: two ranked epics, the top
// one blocked by an open issue, an unranked task, and an item a decision
// holds Blocked / Waiting, which no step of the order reads.
var relayBoard = []relayIssue{
	{n: 1, title: "Epic one", kind: kindEpic, status: holdWorking, rank: 2},
	{n: 2, title: "Epic two", kind: kindEpic, status: holdWorking, rank: 1, blocker: 9},
	{n: 3, title: "Task three", kind: "Feature", status: holdUpNext},
	{n: 7, title: "Task seven", kind: "Feature", status: holdBlocked},
}

// relayGH answers the board reads of board next and the hand-over over
// relayBoard: the fields, the items of a query's statuses and kinds, and
// the priority list. Anything else fails the read.
func relayGH(_ context.Context, args ...string) ([]byte, error) {
	q, vs := args[3], map[string]string{}
	for _, a := range args[4:] {
		if k, v, ok := strings.Cut(a, "="); ok {
			vs[k] = v
		}
	}
	switch {
	case strings.Contains(q, "fields(first:50)"):
		opts := func(names ...string) string {
			out := make([]string, len(names))
			for i, s := range names {
				out[i] = fmt.Sprintf(`{"id":"O%d","name":%q}`, i, s)
			}
			return strings.Join(out, ",")
		}
		return fmt.Appendf(nil, `{"data":{"repositoryOwner":{"projectV2":{"id":"P","fields":{"nodes":[
{"id":"FS","name":"Status","options":[%s]},{"id":"FK","name":"Kind","options":[%s]},{"id":"FP","name":"Priority","dataType":"NUMBER"}]}}}}}`,
			opts(holdStatuses...), opts(kindEpic, "Feature")), nil
	case strings.Contains(q, "items(first:100"):
		var nodes []string
		for _, is := range relayBoard {
			if strings.Contains(vs["q"], "status:") && !strings.Contains(vs["q"], fmt.Sprintf("%q", is.status)) ||
				strings.Contains(vs["q"], "kind:") && !strings.Contains(vs["q"], fmt.Sprintf("%q", is.kind)) {
				continue
			}
			nodes = append(nodes, is.node())
		}
		return []byte(`{"data":{"repositoryOwner":{"projectV2":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[` + strings.Join(nodes, ",") + `]}}}}}`), nil
	}
	return nil, fmt.Errorf("unexpected request %q", q)
}

func (is relayIssue) node() string {
	priority, deps, blockedBy := jsonNull, `{"blockedBy":0,"totalBlockedBy":0}`, ""
	if is.rank > 0 {
		priority = fmt.Sprintf(`{"number":%d,"updatedAt":"2026-10-01T00:00:00Z","creator":{"login":"teemow"}}`, is.rank)
	}
	if is.blocker > 0 {
		deps = `{"blockedBy":1,"totalBlockedBy":1}`
		blockedBy = fmt.Sprintf(`{"closedAt":null,"number":%d,"state":"OPEN","repository":{"nameWithOwner":"o/r"}}`, is.blocker)
	}
	return fmt.Sprintf(`{"id":"IT%d","status":{"name":%q,"updatedAt":"2026-10-01T00:00:00Z"},"kind":{"name":%q},"priority":%s,
"content":{"number":%d,"url":"https://github.com/o/r/issues/%d","title":%q,"state":"OPEN","createdAt":"2026-10-01T00:00:00Z","updatedAt":"2026-10-01T00:00:00Z",
"repository":{"nameWithOwner":"o/r"},"labels":{"nodes":[]},"assignees":{"nodes":[]},"issueDependenciesSummary":%s,"subIssuesSummary":{"total":0,"completed":0},
"parent":null,"blockedBy":{"nodes":[%s]}}}`, is.n, is.status, is.kind, priority, is.n, is.n, is.title, deps, blockedBy)
}

// promptSection is the part of a prompt from the heading title to the next
// one of its level.
func promptSection(t *testing.T, prompt, title string) string {
	t.Helper()
	_, rest, ok := strings.Cut(prompt, "### "+title+"\n")
	if !ok {
		t.Fatalf("the prompt has no section %q:\n%s", title, prompt)
	}
	if i := strings.Index(rest, "\n### "); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

func runIn(a *app, c interface {
	SetArgs([]string)
	Execute() error
}, args ...string) (string, error) {
	out := a.out.(interface {
		String() string
		Reset()
	})
	out.Reset()
	c.SetArgs(args)
	err := c.Execute()
	return out.String(), err
}

// A supervisor relay keeps the priorities and the holds without a note: the
// successor's prompt carries the ranked epics with their holds and plans and
// every held item with what holds it, read from GitHub as the predecessor's
// was, its first board next walks the same order past the same holds, and
// the answer given before the relay is in its prompt. Nothing is filed for
// it.
func TestARelayKeepsThePrioritiesAndHoldsWithoutANote(t *testing.T) {
	a, _ := stubApp(t)
	plat.Machine = tableMachine{plat.Machine}
	now := time.Now()
	a.now = now
	a.cfg.Guide.Person = notePerson
	a.cfg.Board.Owner, a.cfg.Board.Project = "o", 7
	a.cfg.Board.Order = []config.BoardStep{{Name: relayStep, Status: []string{holdUpNext, holdWorking}}}
	was := boardGH
	boardGH = relayGH
	t.Cleanup(func() { boardGH = was })
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide = nil
		_, evs, err := guideRole.start(st, supA, true, false, now.Add(-8*time.Hour))
		st.NextNote = 1
		st.Notes = []state.Note{{ID: 1, Kind: noteDecision, For: notePerson, Text: "ship o/r#7 now?", Refs: []string{decisionRef},
			Default: relayDefault, By: agentC, At: now.Add(-time.Hour),
			Held: &state.ItemHold{Ref: decisionRef, Status: holdBlocked, Was: holdUpNext}}}
		return evs, err
	}); err != nil {
		t.Fatal(err)
	}
	if err := runAnswer(a, refOne, "epic one goes first after the blocker"); err != nil {
		t.Fatal(err)
	}

	prompt := func() string {
		c := a.handoverCmd()
		c.SetOut(a.out)
		c.SilenceUsage, c.SilenceErrors = true, true
		s, err := runIn(a, c, "--prompt")
		if err != nil {
			t.Fatalf("handover --prompt: %v\n%s", err, s)
		}
		return s
	}
	next := func() string {
		c := a.boardCmd()
		c.SetOut(a.out)
		c.SilenceUsage, c.SilenceErrors = true, true
		s, err := runIn(a, c, "next")
		if err != nil {
			t.Fatalf("board next: %v\n%s", err, s)
		}
		return s
	}
	before, beforeNext := prompt(), next()

	// The relay at the clock its events are stamped with: the successor's
	// start is its own, the predecessor's the last relay.
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		_, evs, err := guideRole.relay(st, supA, supB, time.Now(), 15*time.Minute)
		if err != nil {
			return nil, err
		}
		_, more, err := guideRole.start(st, supB, true, false, time.Now())
		return append(evs, more...), err
	}); err != nil {
		t.Fatal(err)
	}
	a.now = time.Now()
	after, afterNext := prompt(), next()

	held := promptSection(t, after, "Priorities and holds")
	for _, want := range []string{
		"1. o/r#2 Epic two (" + holdWorking + "): no plan pull request names it, no go recorded; held: blocked by o/r#9; 1 of 1 blockers open",
		"2. o/r#1 Epic one (" + holdWorking + "): no plan pull request names it, no go recorded",
		"- o/r#7 (" + holdBlocked + "): decision #1 for Pat: ship o/r#7 now? (back to " + holdUpNext + " once it closes)",
		"- o/r#2 Epic two (" + holdWorking + "): held: blocked by o/r#9; 1 of 1 blockers open",
	} {
		if !strings.Contains(held, want) {
			t.Errorf("the successor's priorities and holds lack %q:\n%s", want, held)
		}
	}
	if strings.Index(held, "1. o/r#2") > strings.Index(held, "2. o/r#1") {
		t.Errorf("the ranks are out of order:\n%s", held)
	}
	if was := promptSection(t, before, "Priorities and holds"); was != held {
		t.Errorf("the relay changed the priorities and holds:\nbefore:\n%s\nafter:\n%s", was, held)
	}
	gc := a.guideHandoverCmd()
	gc.SetOut(a.out)
	gc.SilenceUsage, gc.SilenceErrors = true, true
	guide, err := runIn(a, gc, "--prompt")
	if err != nil {
		t.Fatalf("guide handover --prompt: %v\n%s", err, guide)
	}
	if got := promptSection(t, guide, "Priorities and holds"); got != held {
		t.Errorf("the guide's priorities and holds differ from the supervisor's:\n%s", got)
	}
	if epics := promptSection(t, after, "Epics"); !strings.Contains(epics, "o/r#1 Epic one ("+holdWorking+"): no plan pull request names it") {
		t.Errorf("the successor's epics lack o/r#1's plan line:\n%s", epics)
	}
	if ans := promptSection(t, after, "Answered since the last relay"); !strings.Contains(ans, "epic one goes first after the blocker") {
		t.Errorf("the successor's prompt lacks the answer:\n%s", ans)
	}

	if afterNext != beforeNext {
		t.Errorf("the successor's board next differs:\nbefore:\n%s\nafter:\n%s", beforeNext, afterNext)
	}
	for _, want := range []string{"skipped o/r#2: held: blocked by o/r#9; 1 of 1 blockers open", refOne, "priority 2"} {
		if !strings.Contains(afterNext, want) {
			t.Errorf("the successor's board next lacks %q:\n%s", want, afterNext)
		}
	}
	if strings.Contains(afterNext, decisionRef) {
		t.Errorf("board next offers the item the decision holds:\n%s", afterNext)
	}

	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Notes) != 1 || st.Notes[0].ID != 1 || st.NextNote != 1 {
		t.Errorf("the relay filed a note: %+v (next %d)", st.Notes, st.NextNote)
	}
}
