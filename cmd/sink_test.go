package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
)

// sinkApp is an app on a scratch state whose configuration dispatches o/site's
// refresh.yaml on sinkYAML's verbs.
func sinkApp(t *testing.T, sinkYAML string) (*app, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("stateDir: "+dir+"\n"+sinkYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	return &app{cfg: cfg, store: store, now: time.Now(), out: &out, as: agentOne}, &out
}

// fakeSinkGH records each gh call of the sink and fails while fail is set.
func fakeSinkGH(t *testing.T, fail *bool) *[]string {
	t.Helper()
	var calls []string
	prev := sinkGH
	t.Cleanup(func() { sinkGH = prev })
	sinkGH = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		if *fail {
			return nil, errors.New("HTTP 422: Unexpected inputs provided")
		}
		return nil, nil
	}
	return &calls
}

// poll runs one poll's sink of w and waits for its dispatch.
func sinkPoll(w *watcher) {
	w.now = time.Now()
	w.sink(context.Background())
	w.bg.Wait()
}

// The watch dispatches the configured workflow once for each selected event
// with an issue or pull request as its subject, with the inputs verb,
// subject and time only, across the machine's watches; a failed dispatch is
// said, logged and tried again on the next poll.
func TestTheWatchDispatchesTheSinksWorkflowOnTheSelectedEvents(t *testing.T) {
	fail := false
	calls := fakeSinkGH(t, &fail)
	a, out := sinkApp(t, "sink:\n  repo: o/site\n  workflow: refresh.yaml\n  ref: main\n  verbs: [session.serve, merged]\n")
	w := a.newWatcher(false, true)
	sinkPoll(w) // starts the ledger: what was logged before is never replayed
	at := time.Now().UTC()
	serve := about(state.Event{At: at, By: sinkParty, Verb: verbServe, Detail: "agent-one: serves o/r#7"}, "https://github.com/o/r/issues/7")
	if err := a.store.Log(
		serve,
		state.Event{At: at, By: sinkParty, Verb: verbServe, Detail: "agent-one: serves a chore"},
		about(state.Event{At: at, By: sinkParty, Verb: verbPlanReady, Detail: "o/plans#3"}, "o/plans#3"),
		about(state.Event{At: at, By: sinkParty, Verb: "note.add", Detail: "#1 o/r#7"}, decisionRef),
	); err != nil {
		t.Fatal(err)
	}
	sinkPoll(w)
	want := "workflow run refresh.yaml --repo o/site --ref main -f verb=session.serve -f subject=o/r#7 -f time=" + at.Format(time.RFC3339)
	if !slices.Equal(*calls, []string{want}) {
		t.Fatalf("one dispatch for the selected event with a subject, none for an unselected verb or a serve of no issue:\n%q\nwant %q", *calls, want)
	}

	other := *a // the standby watch is a process of its own, with its own reading of the log
	var err error
	if other.store, err = state.Open(a.cfg.StateDir); err != nil {
		t.Fatal(err)
	}
	standby := other.newWatcher(true, true)
	sinkPoll(standby)
	sinkPoll(w)
	if len(*calls) != 1 {
		t.Fatalf("a second watch and a second poll dispatch the claimed event again: %q", *calls)
	}

	fail = true
	if err := a.store.Log(about(event(sinkParty, verbMerged, "o/r#8 exit 0, release v1.2.0"), mergedRef)); err != nil {
		t.Fatal(err)
	}
	sinkPoll(w)
	if !strings.Contains(out.String(), "SINK FAILED: merged o/r#8: o/site refresh.yaml: HTTP 422") {
		t.Fatalf("a failed dispatch is said:\n%s", out.String())
	}
	fail = false
	sinkPoll(standby)
	if len(*calls) != 3 || !strings.Contains((*calls)[2], "-f verb=merged -f subject=o/r#8 ") {
		t.Fatalf("the failed dispatch is tried again by the next poll of any watch: %q", *calls)
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return strings.HasPrefix(e.Verb, "sink.") })
	if err != nil {
		t.Fatal(err)
	}
	var verbs []string
	for _, e := range evs {
		verbs = append(verbs, e.Verb+" "+e.Detail)
	}
	if !slices.Equal(verbs, []string{
		"sink.dispatched session.serve o/r#7: o/site refresh.yaml",
		"sink.failed merged o/r#8: o/site refresh.yaml: HTTP 422: Unexpected inputs provided",
		"sink.dispatched merged o/r#8: o/site refresh.yaml",
	}) {
		t.Fatalf("each dispatch and failure is logged: %q", verbs)
	}
}

// Without a sink nothing is followed, claimed or dispatched.
func TestTheWatchDispatchesNothingWithoutASink(t *testing.T) {
	fail := false
	calls := fakeSinkGH(t, &fail)
	a, _ := sinkApp(t, "")
	w := a.newWatcher(false, true)
	sinkPoll(w)
	if err := a.store.Log(about(event(sinkParty, verbServe, "agent-one: serves o/r#7"), decisionRef)); err != nil {
		t.Fatal(err)
	}
	sinkPoll(w)
	if len(*calls) != 0 {
		t.Fatalf("dispatched without a sink: %q", *calls)
	}
	if _, err := os.Stat(filepath.Join(a.store.Dir(), "sink.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a ledger without a sink: %v", err)
	}
}

// A merge the gate recorded as merged carries its pull request as the
// subject the sink dispatches; a promotion, which has none, carries none.
func TestAMergedEventCarriesItsPullRequestAsItsSubject(t *testing.T) {
	for _, tc := range []struct {
		pr   int
		want string
	}{{7, decisionRef}, {0, ""}} {
		st := &state.State{Merges: []state.Merge{{Repo: scratchRepo, PR: tc.pr, Lane: scratchRepo, Phase: state.Running}}}
		evs := recordRun(st, 0, config.Lane{Name: scratchRepo}, sinkParty, runOutcome{out: merge.Outcome{Merged: true, Release: "v1.2.0"}}, time.Now(), "")
		i := slices.IndexFunc(evs, func(e state.Event) bool { return e.Verb == verbMerged })
		if i < 0 || evs[i].Subject != tc.want {
			t.Fatalf("PR %d: events %+v, want subject %q", tc.pr, evs, tc.want)
		}
	}
}

// about names the issue or pull request of a ref or its URL, and nothing
// for a ref that names none.
func TestAboutSetsTheSubjectFromARef(t *testing.T) {
	for ref, want := range map[string]string{
		decisionRef:                       decisionRef,
		"https://github.com/o/r/pull/8":   mergedRef,
		"https://github.com/o/q/issues/9": "o/q#9",
		"a chore":                         "",
		"#7":                              "",
	} {
		if got := about(state.Event{Verb: verbServe}, ref).Subject; got != want {
			t.Errorf("%q: subject %q, want %q", ref, got, want)
		}
	}
}

// sinkParty is the agent whose events the sink tests log.
var sinkParty = state.Party{Name: agentOne}

// mergedRef is the pull request the sink tests merge.
const mergedRef = "o/r#8"
