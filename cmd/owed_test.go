//go:build unix

package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

// mergedAt is when GitHub merged the tests' stopped merge.
var mergedAt = time.Date(2026, 10, 14, 19, 47, 2, 0, time.UTC)

// stoppedMerged is the line a stopped merge of o/r#7 hands over once GitHub
// reports it merged.
const stoppedMerged = "devctl pr merge o/r 7 was stopped by SIGTERM (exit 143) before devctl reported its end; " +
	"GitHub says o/r#7 merged at 2026-10-14T19:47:02Z: do not merge it again, `devctl release wait o/r --pr 7` waits for its release"

// stubPull makes every pull request GitHub reads p, or err.
func stubPull(t *testing.T, p github.Pull, err error) {
	t.Helper()
	was := pullState
	pullState = func(context.Context, string, int) (github.Pull, error) { return p, err }
	t.Cleanup(func() { pullState = was })
}

// stoppedMerge is tellOwnerRun's run as the gated merge of o/r#7 its
// user manager stopped (SIGTERM) after GitHub merged it, before devctl
// reported the release.
func stoppedMerge(t *testing.T) (*app, childResult, *[]string) {
	t.Helper()
	a, r, woke := tellOwnerRun(t, state.Party{Session: "s1", Name: ownerName})
	r.spec.Argv = strings.Fields("devctl pr merge --timeout 1h0m0s o/r 7")
	r.rc, r.doc, r.stop = 128+int(syscall.SIGTERM), nil, syscall.SIGTERM
	return a, r, woke
}

// ownedState is a's owed outcomes.
func ownedState(t *testing.T, a *app) []state.Owed {
	t.Helper()
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	return st.Owed
}

// laterWatch is a watch on a's state, owedRetry after the merge-child that
// owed the outcome ended.
func laterWatch(t *testing.T, a *app, after time.Duration) *watcher {
	t.Helper()
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		for i := range st.Owed {
			st.Owed[i].Child = deadPID(t)
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	a.now = time.Now().Add(after)
	return &watcher{app: a}
}

// A merge whose waiter was stopped while GitHub did not answer (its user
// manager going down with the desktop session) stays owed: its owner hears
// nothing yet, the failure is logged once. The watch's next try, once
// GitHub answers, hands the merge over and logs it merged.
func TestAStoppedMergeGitHubDoesNotAnswerForIsHandedOverByTheWatch(t *testing.T) {
	a, r, woke := stoppedMerge(t)
	stubPull(t, github.Pull{}, errors.New("keychain OpenSession: Could not activate remote peer"))
	a.tellOwner(context.Background(), r)
	if len(*woke) != 0 {
		t.Fatalf("woke %q without GitHub's answer", *woke)
	}
	owed := ownedState(t, a)
	if len(owed) != 1 || !owed[0].Ask || owed[0].Repo != "o/r" || owed[0].PR != 7 || !strings.Contains(owed[0].Why, "GitHub does not answer for o/r#7") {
		t.Fatalf("owed %+v", owed)
	}

	w := laterWatch(t, a, time.Second)
	w.owedOutcomes(context.Background())
	if len(*woke) != 0 || len(ownedState(t, a)) != 1 {
		t.Fatalf("a try within owedRetry: woke %q, owed %+v", *woke, ownedState(t, a))
	}
	w.owedOutcomes(context.Background())
	evs, _ := a.store.Events(0, func(e state.Event) bool { return e.Verb == verbUnheard && strings.Contains(e.Detail, "GitHub does not answer") })
	if len(evs) != 1 || !strings.HasSuffix(evs[0].Detail, `owed to "worker", the watch tries again`) {
		t.Errorf("the failure is logged once: %+v", evs)
	}

	stubPull(t, github.Pull{State: github.Merged, MergedAt: mergedAt}, nil)
	w = laterWatch(t, a, owedRetry)
	w.owedOutcomes(context.Background())
	if len(*woke) != 1 || (*woke)[0] != "beekeeper gate → s1: "+stoppedMerged {
		t.Errorf("woke %q, want %q", *woke, stoppedMerged)
	}
	if owed := ownedState(t, a); len(owed) != 0 {
		t.Errorf("owed after the hand-over %+v", owed)
	}
	if evs, _ := a.store.Events(0, func(e state.Event) bool { return e.Verb == verbMerged }); len(evs) != 1 {
		t.Errorf("merged events %+v", evs)
	}
}

// A stopped merge GitHub reports open asks its owner to run it again; one
// GitHub never answers for within owedAsk is handed over without the
// answer.
func TestAStoppedMergeSaysWhatGitHubSays(t *testing.T) {
	a, r, woke := stoppedMerge(t)
	stubPull(t, github.Pull{State: github.Open}, nil)
	a.tellOwner(context.Background(), r)
	if want := "beekeeper gate → s1: devctl pr merge o/r 7 was stopped by SIGTERM (exit 143) before devctl reported its end; GitHub says o/r#7 is open, not merged: run the same command again"; len(*woke) != 1 || (*woke)[0] != want {
		t.Errorf("open: woke %q", *woke)
	}

	a, r, woke = stoppedMerge(t)
	stubPull(t, github.Pull{}, errors.New("unreachable"))
	a.tellOwner(context.Background(), r)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Owed[0].Since = st.Owed[0].Since.Add(-owedAsk)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	laterWatch(t, a, owedRetry).owedOutcomes(context.Background())
	if len(*woke) != 1 || !strings.Contains((*woke)[0], "GitHub has not answered for o/r#7 since: check the pull request") {
		t.Errorf("unanswered past owedAsk: woke %q", *woke)
	}
}

// A wake the service manager refuses (a stopping user manager) keeps the
// outcome owed; the watch wakes the owner once it can.
func TestARefusedWakeIsRetriedByTheWatch(t *testing.T) {
	a, r, woke := tellOwnerRun(t, state.Party{Session: "s1", Name: ownerName})
	refuse := errors.New("Transaction for beekeeper-wake.service/start is destructive")
	inner := wakeOwner
	wakeOwner = func(a *app, ctx context.Context, by state.Party, q, msg, mode string) error {
		if refuse != nil {
			return refuse
		}
		return inner(a, ctx, by, q, msg, mode)
	}
	a.tellOwner(context.Background(), r)
	if owed := ownedState(t, a); len(owed) != 1 || !strings.HasPrefix(owed[0].Why, `waking "worker" failed`) {
		t.Fatalf("owed %+v", owed)
	}
	refuse = nil
	laterWatch(t, a, owedRetry).owedOutcomes(context.Background())
	if want := "beekeeper gate → s1: devctl pr wait o/r 7 exit 0: green: every check passed (output in /k/pr-wait.log)"; len(*woke) != 1 || (*woke)[0] != want {
		t.Errorf("woke %q, want %q", *woke, want)
	}
	if owed := ownedState(t, a); len(owed) != 0 {
		t.Errorf("owed after the retry %+v", owed)
	}
}

// An outcome owed by a merge-child that ended before it handed it over
// (killed while it waited for its gate) is handed over by the watch, unless
// the gate's caller heard it; one owed past owedFor is given up, logged.
func TestTheWatchHandsOverWhatAKilledMergeChildOwed(t *testing.T) {
	a, r, woke := tellOwnerRun(t, state.Party{Session: "s1", Name: ownerName})
	o := r.owed(time.Now().Add(-owedRetry))
	if err := a.owe(o); err != nil {
		t.Fatal(err)
	}
	w := laterWatch(t, a, 0)
	w.owedOutcomes(context.Background())
	if len(*woke) != 1 || len(ownedState(t, a)) != 0 {
		t.Errorf("woke %q, owed %+v", *woke, ownedState(t, a))
	}

	a, r, woke = tellOwnerRun(t, state.Party{Session: "s1", Name: ownerName})
	o = r.owed(time.Now().Add(-owedRetry))
	if err := a.owe(o); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(o.Heard), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(o.Heard, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	laterWatch(t, a, 0).owedOutcomes(context.Background())
	if len(*woke) != 0 || len(ownedState(t, a)) != 0 {
		t.Errorf("a heard outcome: woke %q, owed %+v", *woke, ownedState(t, a))
	}

	a, r, woke = tellOwnerRun(t, state.Party{Session: "s1", Name: ownerName})
	if err := a.owe(r.owed(time.Now().Add(-owedFor - time.Minute))); err != nil {
		t.Fatal(err)
	}
	laterWatch(t, a, 0).owedOutcomes(context.Background())
	evs, _ := a.store.Events(0, func(e state.Event) bool { return e.Verb == verbUnheard && strings.Contains(e.Detail, "given up") })
	if len(*woke) != 0 || len(ownedState(t, a)) != 0 || len(evs) != 1 {
		t.Errorf("past owedFor: woke %q, owed %+v, events %+v", *woke, ownedState(t, a), evs)
	}
}

// The watch leaves an outcome to the gate and merge-child still handing it
// over.
func TestTheWatchLeavesAnOutcomeItsMergeChildHandsOver(t *testing.T) {
	a, r, woke := tellOwnerRun(t, state.Party{Session: "s1", Name: ownerName})
	if err := a.owe(r.owed(time.Now().Add(-owedRetry))); err != nil {
		t.Fatal(err)
	}
	a.now = time.Now()
	(&watcher{app: a}).owedOutcomes(context.Background())
	if len(*woke) != 0 || len(ownedState(t, a)) != 1 {
		t.Errorf("woke %q, owed %+v", *woke, ownedState(t, a))
	}
}
