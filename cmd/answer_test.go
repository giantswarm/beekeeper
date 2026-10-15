package cmd

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The threads of the answer tests: a private one, a public slice under a
// public epic, and the private plan and private epic above them.
const (
	privateThread = "o/private#7"
	publicSlice   = "o/public#3"
	publicEpic    = "o/public#1"
	privateEpic   = "o/private#9"
	answerPlans   = "o/plans"
	flagRef       = "--ref"
	unknownThread = "o/r#404"
	noteFiler     = "filer"
)

// answerGH answers what beekeeper answer asks GitHub: the thread with its
// parents, the plans naming them and the comment, which it records.
type answerGH struct {
	// plan is the epic a private plan names, "" for none; epic is the
	// public epic's parent, "" for none.
	plan, epic string
	comments   []string
	repos      []string
}

// stubAnswerGH puts f in place of the board's gh for the test.
func stubAnswerGH(t *testing.T, f *answerGH) *answerGH {
	t.Helper()
	was := boardGH
	boardGH = f.run
	t.Cleanup(func() { boardGH = was })
	return f
}

func issueNode(ref string, private bool, parent string) string {
	repo, n, _ := strings.Cut(ref, "#")
	return fmt.Sprintf(`{"__typename":"Issue","id":"ID-%s","number":%s,"repository":{"nameWithOwner":%q,"isPrivate":%t},"parent":%s}`, ref, n, repo, private, parent)
}

func (f *answerGH) run(ctx context.Context, args ...string) ([]byte, error) {
	q := args[3]
	vs := map[string]string{}
	for i := 4; i+1 < len(args); i += 2 {
		k, v, _ := strings.Cut(args[i+1], "=")
		vs[k] = v
	}
	f.repos = append(f.repos, vs["r"])
	switch {
	case strings.Contains(q, "addComment"):
		f.comments = append(f.comments, vs["s"]+": "+vs["b"])
		return fmt.Appendf(nil, `{"data":{"addComment":{"commentEdge":{"node":{"url":"https://github.com/%s#issuecomment-%d"}}}}}`, strings.TrimPrefix(vs["s"], "ID-"), len(f.comments)), nil
	case strings.Contains(q, "search("):
		var parts []string
		for i := 0; strings.Contains(q, fmt.Sprintf("p%d:", i)); i++ {
			nodes := ""
			if f.plan != "" && strings.Contains(q, fmt.Sprintf(`p%d:search(query:"repo:%s is:pr \"%s\"`, i, answerPlans, strings.Replace(f.plan, "#", "/issues/", 1))) {
				owner, rest, _ := strings.Cut(f.plan, "/")
				repo, n, _ := strings.Cut(rest, "#")
				nodes = fmt.Sprintf(`{"id":"ID-o/plans#4","number":4,"url":"https://github.com/o/plans/pull/4","title":"plan","state":"OPEN","isDraft":true,"merged":false,"headRefOid":"abc","body":"Epic: https://github.com/%s/%s/issues/%s","repository":{"nameWithOwner":"o/plans","isPrivate":true}}`, owner, repo, n)
			}
			parts = append(parts, fmt.Sprintf(`"p%d":{"nodes":[%s]}`, i, nodes))
		}
		return []byte(`{"data":{` + strings.Join(parts, ",") + `}}`), nil
	case strings.Contains(q, "issueOrPullRequest"):
		ref := vs["o"] + "/" + vs["r"] + "#" + vs["n"]
		var item string
		switch ref {
		case privateThread:
			item = issueNode(ref, true, "null")
		case publicSlice:
			epic := "null"
			if f.epic != "" {
				epic = issueNode(f.epic, true, "null")
			}
			item = issueNode(ref, false, issueNode(publicEpic, false, epic))
		case unknownThread:
			return []byte(`{"data":{"repository":{"issueOrPullRequest":null}},"errors":[{"type":"NOT_FOUND","path":["repository","issueOrPullRequest"],"message":"Could not resolve to an issue or pull request"}]}`), nil
		default:
			item = issueNode(ref, true, "null")
		}
		return []byte(`{"data":{"repository":{"issueOrPullRequest":` + item + `}}}`), nil
	}
	return nil, fmt.Errorf("unexpected request %v", args)
}

func answerApp(t *testing.T) (*app, *bytes.Buffer) {
	t.Helper()
	a, out := noteApp(t)
	a.cfg.Plans = config.Plans{Repositories: []string{answerPlans}}
	return a, out
}

func runAnswer(a *app, args ...string) error {
	c := a.answerCmd()
	c.SetArgs(args)
	c.SetOut(a.out)
	c.SetErr(a.out)
	c.SilenceUsage, c.SilenceErrors = true, true
	return c.Execute()
}

// An answer without a note lands on its private thread, in the log, and
// with the session serving the thread.
func TestAnswerWithoutANoteLandsOnItsThread(t *testing.T) {
	a, out := answerApp(t)
	f := stubAnswerGH(t, &answerGH{})
	woke := stubWakes(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Records = append(st.Records,
			state.Record{Session: state.Party{Session: "w1", Name: "server"}, Issue: "https://github.com/o/private/issues/7"},
			state.Record{Session: state.Party{Session: "w2", Name: "another server"}, Issue: "o/private#8"})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := runAnswer(a, "https://github.com/o/private/issues/7", "the draft stays;", "implement at its head"); err != nil {
		t.Fatal(err)
	}
	if len(f.comments) != 1 || !strings.HasPrefix(f.comments[0], "ID-o/private#7: **Answer** from Pat via cli:\n\n> the draft stays; implement at its head\n") ||
		!strings.Contains(f.comments[0], "Written by an agent") {
		t.Fatalf("comments %q", f.comments)
	}
	const url = "https://github.com/o/private#7#issuecomment-1"
	if s := out.String(); !strings.Contains(s, "answer recorded on o/private#7: "+url) || !strings.Contains(s, "told server") {
		t.Errorf("output %q", s)
	}
	if len(*woke) != 1 || !strings.Contains((*woke)[0], "→ w1: ANSWER on o/private#7 by Pat: the draft stays; implement at its head ("+url+")") {
		t.Errorf("woke %q", *woke)
	}
	got, err := a.answeredSince(time.Time{})
	if err != nil || len(got) != 1 {
		t.Fatalf("answers %+v, %v", got, err)
	}
	if an := got[0]; an.About != privateThread || an.Comment != url || an.For != notePerson || an.Via != viaCLI || an.Answer != "the draft stays; implement at its head" || an.ID != 0 {
		t.Errorf("answer %+v", an)
	}
	// With nobody serving it, the answer is still on the thread and in the log.
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Records[0].Ended = relayNow
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runAnswer(a, privateThread, "--via", viaSlack, "yes"); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "no session serves o/private#7") || len(*woke) != 1 || len(f.comments) != 2 {
		t.Errorf("output %q, woke %q", s, *woke)
	}
	if err := runAnswer(a, privateThread); Code(err) != ExitUsage {
		t.Errorf("an answer without words: %v", err)
	}
	if err := runAnswer(a, privateThread, "--choice", "1", "yes"); Code(err) != ExitUsage {
		t.Errorf("--choice without --note: %v", err)
	}
}

// An answer about a public repository's issue lands on its private plan,
// else its nearest private epic, never on the public thread; with neither
// it is refused, nothing posted.
func TestAnAnswerAboutAPublicIssueLandsOnItsPrivateThread(t *testing.T) {
	for name, c := range map[string]struct {
		gh     answerGH
		thread string
		via    string
	}{
		"the plan of its epic": {answerGH{plan: publicEpic, epic: privateEpic}, "ID-o/plans#4", "o/plans#4 (the plan of " + publicEpic + ")"},
		"its own plan":         {answerGH{plan: publicSlice}, "ID-o/plans#4", "o/plans#4 (the plan of " + publicSlice + ")"},
		"the private epic":     {answerGH{epic: privateEpic}, "ID-" + privateEpic, privateEpic + " (an epic above " + publicSlice + ")"},
		"neither":              {answerGH{}, "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			a, out := answerApp(t)
			f := stubAnswerGH(t, &c.gh)
			stubWakes(t)
			err := runAnswer(a, publicSlice, "go")
			if c.thread == "" {
				if Code(err) != ExitRefused || !strings.Contains(err.Error(), publicSlice+" is public and has no private thread") || len(f.comments) != 0 {
					t.Fatalf("refusal %v, comments %q", err, f.comments)
				}
				if got, _ := a.answeredSince(time.Time{}); len(got) != 0 {
					t.Fatalf("logged %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(f.comments) != 1 || !strings.HasPrefix(f.comments[0], c.thread+": **Answer** from Pat via cli, about "+publicSlice+":") {
				t.Fatalf("comments %q", f.comments)
			}
			if !strings.Contains(out.String(), "answer recorded on "+c.via) {
				t.Errorf("output %q", out.String())
			}
			if got, _ := a.answeredSince(time.Time{}); len(got) != 1 || got[0].About != publicSlice {
				t.Errorf("logged %+v", got)
			}
		})
	}
	a, _ := answerApp(t)
	stubAnswerGH(t, &answerGH{})
	if err := runAnswer(a, unknownThread, "go"); Code(err) != ExitRefused || !strings.Contains(err.Error(), "no issue or pull request GitHub resolves") {
		t.Errorf("an unknown thread: %v", err)
	}
	if err := runAnswer(a, "not-a-ref", "go"); Code(err) != ExitUsage {
		t.Errorf("no thread: %v", err)
	}
}

// note answer is beekeeper answer on the note's first --ref: the note closes
// and its filer is told; a note without a --ref names the verb to use.
func TestNoteAnswerRecordsOnTheNotesThread(t *testing.T) {
	a, _ := answerApp(t)
	f := stubAnswerGH(t, &answerGH{})
	woke := stubWakes(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Notes = append(st.Notes,
			state.Note{ID: 1, For: notePerson, Kind: noteDecision, By: state.Party{Session: "w1", Name: noteFiler}, Text: "Keep the draft?", Refs: []string{privateThread}, Options: []string{optMerge, optWait}},
			state.Note{ID: 2, For: notePerson, Kind: noteDecision, By: state.Party{Session: "w1", Name: noteFiler}, Text: "Old question?"})
		st.NextNote = 2
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := noteCommand(a, verbAnswer, "1", flagChoice, "2", "after", "the", "release"); err != nil {
		t.Fatal(err)
	}
	if len(f.comments) != 1 || !strings.HasPrefix(f.comments[0], "ID-o/private#7: **Answer** from Pat via cli to note #1 (asked by filer: Keep the draft?):\n\n> wait — after the release\n") {
		t.Fatalf("comments %q", f.comments)
	}
	if len(*woke) != 1 || !strings.Contains((*woke)[0], "→ w1: NOTE ANSWERED #1 by Pat: wait — after the release (on o/private#7: ") {
		t.Errorf("woke %q", *woke)
	}
	got, _ := a.answeredSince(time.Time{})
	if len(got) != 1 || got[0].ID != 1 || got[0].Question != "Keep the draft?" || got[0].Asker != noteFiler || got[0].Answer != "wait — after the release" {
		t.Fatalf("answers %+v", got)
	}
	_, err := noteCommand(a, verbAnswer, "2", "yes")
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "beekeeper answer <owner/repo#n> --note 2") {
		t.Fatalf("a note without --ref: %v", err)
	}
	if err := runAnswer(a, privateThread, "--note", "2", "yes"); err != nil {
		t.Fatal(err)
	}
	if st, _ := a.store.Read(); len(st.Notes) != 0 {
		t.Errorf("open notes %+v", st.Notes)
	}
	if err := runAnswer(a, privateThread, "--note", "2", "yes"); Code(err) != ExitRefused {
		t.Errorf("a closed note: %v", err)
	}
}

// A decision (note add --for the person) without --ref is refused; a note on a thread the person
// answered on is filed with a warning quoting the answer.
func TestNoteForAPersonNeedsItsThreadAndIsWarnedOfItsAnswer(t *testing.T) {
	a, out := answerApp(t)
	stubAnswerGH(t, &answerGH{})
	stubWakes(t)
	noRef := slices.DeleteFunc(slices.Clone(decisionArgs), func(s string) bool { return s == flagRef || s == decisionRef })
	if err := addNote(a, append(noRef, "approve "+prURL+"?")...); Code(err) != ExitUsage || !strings.Contains(err.Error(), "it names no thread: --ref owner/repo#n") {
		t.Fatalf("a decision without --ref: %v", err)
	}
	if err := addNote(a, "--for", supervisorRole.title, "State of the watch"); Code(err) != ExitUsage {
		t.Fatalf("a memo --for a role: %v", err)
	}
	if err := runAnswer(a, decisionRef, "keep the draft"); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := addNote(a, append(slices.Clone(decisionArgs), "approve "+prURL+"?")...); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, `warning: "Pat" answered on o/r#7 `) || !strings.Contains(s, `: "keep the draft"`) {
		t.Errorf("output %q", s)
	}
}
