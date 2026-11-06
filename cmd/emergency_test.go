package cmd

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	emergencyTestRepo   = "o/r"
	emergencyReasonFlag = "--reason"
)

func emergencyApp(t *testing.T) *app {
	t.Helper()
	a, _ := noteApp(t)
	a.as = ""
	oldClock := emergencyNow
	emergencyNow = func() time.Time { return a.now }
	t.Cleanup(func() { emergencyNow = oldClock })
	for _, key := range []string{litBeekeeperCodexAgent, litBeekeeperOmpAgent, litBeekeeperAgyAgent, transcriptHostEnv} {
		t.Setenv(key, "")
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "emergency-guide")
	t.Setenv("CLAUDE_CODE_SESSION_NAME", mailGuide)
	me, err := a.caller()
	if err != nil {
		t.Fatal(err)
	}
	if err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide = &state.Role{Holder: &state.Supervisor{Party: me}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	return a
}

func emergencyCall(a *app, args ...string) error {
	c := a.emergencyCmd()
	c.SetArgs(args)
	c.SetOut(a.out)
	c.SetErr(a.out)
	c.SilenceUsage = true
	c.SilenceErrors = true
	return c.Execute()
}

func TestEmergencyRequiresGuideQuoteAndFutureExpiry(t *testing.T) {
	a := emergencyApp(t)
	for _, args := range [][]string{{"on"}, {"on", emergencyReasonFlag, "merge now"}, {"on", emergencyReasonFlag, "merge now", untilFlag, "-1m"}} {
		if err := emergencyCall(a, args...); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	a.as = mailGuide
	if err := emergencyCall(a, "on", emergencyReasonFlag, "merge now", untilFlag, "10m"); Code(err) != ExitRefused {
		t.Fatalf("--as authorized: %v", err)
	}
	a.as = ""
	if err := emergencyCall(a, "on", emergencyReasonFlag, "merge now", untilFlag, "10m"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.emergencyAction("test"); err != nil {
		t.Fatal(err)
	}
	a.now = a.now.Add(10 * time.Minute)
	if _, err := a.emergencyAction("expired"); Code(err) != ExitRefused {
		t.Fatalf("expiry: %v", err)
	}
	a.now = a.now.Add(-time.Minute)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) { st.Guide.Holder.Session = successorName; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := a.emergencyAction("old guide"); Code(err) != ExitRefused {
		t.Fatalf("relieved guide: %v", err)
	}
}

func TestEmergencyMergeMatchesApprovedHeadAndAuditsFailure(t *testing.T) {
	a := emergencyApp(t)
	const quote = "Merge the approved pull requests now."
	if err := emergencyCall(a, "on", emergencyReasonFlag, quote, untilFlag, "10m"); err != nil {
		t.Fatal(err)
	}
	old := github.RunGH
	t.Cleanup(func() { github.RunGH = old })
	approved := false
	fail := true
	var calls [][]string
	github.RunGH = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, slices.Clone(args))
		if args[1] == emergencyViewCommand {
			decision := "REVIEW_REQUIRED"
			if approved {
				decision = "APPROVED"
			}
			return []byte(`{"state":"OPEN","reviewDecision":"` + decision + `","headRefOid":"abc123"}`), nil
		}
		if fail {
			return nil, errors.New("head changed")
		}
		return nil, nil
	}
	if err := emergencyCall(a, gitMerge, emergencyTestRepo, "7"); Code(err) != ExitRefused {
		t.Fatalf("unapproved: %v", err)
	}
	if len(calls) != 1 {
		t.Fatal(calls)
	}
	approved = true
	if err := emergencyCall(a, gitMerge, emergencyTestRepo, "7"); err == nil || err.Error() != "head changed" {
		t.Fatal(err)
	}
	want := []string{"pr", gitMerge, "7", "--repo", emergencyTestRepo, "--squash", "--match-head-commit", "abc123"}
	if !slices.Equal(calls[len(calls)-1], want) {
		t.Fatalf("merge: %v", calls)
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return strings.HasPrefix(e.Verb, "emergency.") })
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if !strings.Contains(e.Detail, quote) {
			t.Fatalf("missing quoted reason: %+v", e)
		}
	}
	if !strings.Contains(evs[len(evs)-1].Detail, "failed: head changed") {
		t.Fatal(evs)
	}
	fail = false
	if err := emergencyCall(a, gitMerge, emergencyTestRepo, "7"); err != nil {
		t.Fatalf("approved merge: %v", err)
	}
	// No mutation after expiry, even though GitHub still approves it.
	a.now = a.now.Add(time.Hour)
	before := len(calls)
	if err := emergencyCall(a, gitMerge, emergencyTestRepo, "7"); Code(err) != ExitRefused {
		t.Fatal(err)
	}
	if len(calls) != before {
		t.Fatal("expired grant mutated GitHub")
	}
}

func TestEmergencyInstallPauseOnlyBlocksInstall(t *testing.T) {
	a := emergencyApp(t)
	if err := emergencyCall(a, "auto-install", emergencyPauseCommand, emergencyReasonFlag, "Wait until main is proven."); err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !st.AutoInstallPaused || st.Stop != nil || len(st.Holds) != 0 {
		t.Fatalf("pause changed other controls: %+v", st)
	}
	l := &supervisorLoop{a: a}
	if _, err := l.install(context.Background(), superloop.Action{Version: "v1.2.3"}); !errors.Is(err, errLoopLater) {
		t.Fatalf("paused install: %v", err)
	}
	if err := emergencyCall(a, "auto-install", agentResumeName, emergencyReasonFlag, "Main is proven."); err != nil {
		t.Fatal(err)
	}
	st, err = a.store.Read()
	if err != nil || st.AutoInstallPaused {
		t.Fatalf("resume: %+v %v", st, err)
	}
}

func TestEmergencyHoldControlsRequireGrantAndKeepReason(t *testing.T) {
	a := emergencyApp(t)
	if err := emergencyCall(a, "hold", emergencySetCommand, "starts", emergencyReasonFlag, "sort priorities"); Code(err) != ExitRefused {
		t.Fatalf("hold without grant: %v", err)
	}
	if err := emergencyCall(a, "on", emergencyReasonFlag, "Pause new workers now.", untilFlag, "10m"); err != nil {
		t.Fatal(err)
	}
	if err := emergencyCall(a, "hold", emergencySetCommand, "starts", emergencyReasonFlag, "sort priorities"); err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Holds) != 1 || st.Holds[0].Target != "starts" {
		t.Fatalf("hold: %+v", st.Holds)
	}
	if err := emergencyCall(a, "hold", "lift", "starts"); err != nil {
		t.Fatal(err)
	}
	st, err = a.store.Read()
	if err != nil || len(st.Holds) != 0 {
		t.Fatalf("lift: %+v %v", st.Holds, err)
	}
	if err := emergencyCall(a, "capacity", "secret.store", "bad"); err == nil {
		t.Fatal("capacity accepted unrelated config")
	}
}

func TestEmergencyMergeRechecksExpiryAfterGitHubRead(t *testing.T) {
	a := emergencyApp(t)
	if err := emergencyCall(a, "on", emergencyReasonFlag, "Merge now.", untilFlag, "10m"); err != nil {
		t.Fatal(err)
	}
	old := github.RunGH
	t.Cleanup(func() { github.RunGH = old })
	mutations := 0
	github.RunGH = func(_ context.Context, args ...string) ([]byte, error) {
		if args[1] == emergencyViewCommand {
			a.now = a.now.Add(11 * time.Minute)
			return []byte(`{"state":"OPEN","reviewDecision":"APPROVED","headRefOid":"abc123"}`), nil
		}
		mutations++
		return nil, nil
	}
	if err := emergencyCall(a, gitMerge, emergencyTestRepo, "7"); Code(err) != ExitRefused {
		t.Fatalf("expired during read: %v", err)
	}
	if mutations != 0 {
		t.Fatalf("%d GitHub mutations after expiry", mutations)
	}
}

func TestEmergencyMergePreservesStopsAndHolds(t *testing.T) {
	for _, target := range []string{bgStopCmd, holdMerges, emergencyTestRepo, holdGitHub} {
		t.Run(target, func(t *testing.T) {
			a := emergencyApp(t)
			if err := emergencyCall(a, "on", emergencyReasonFlag, "Merge now.", untilFlag, "10m"); err != nil {
				t.Fatal(err)
			}
			block := func() error {
				return a.store.Update(func(st *state.State) ([]state.Event, error) {
					if target == bgStopCmd {
						st.Stop = &state.Stop{Reason: "desk stopped"}
					} else {
						st.Holds = []state.Hold{{Target: target, Reason: "proving main"}}
					}
					return nil, nil
				})
			}
			old := github.RunGH
			t.Cleanup(func() { github.RunGH = old })
			mutations := 0
			github.RunGH = func(_ context.Context, args ...string) ([]byte, error) {
				if args[1] == emergencyViewCommand {
					if err := block(); err != nil {
						t.Fatal(err)
					}
					return []byte(`{"state":"OPEN","reviewDecision":"APPROVED","headRefOid":"abc123"}`), nil
				}
				mutations++
				return nil, nil
			}
			// The stop/hold appears while the GitHub read is in flight.
			if err := emergencyCall(a, gitMerge, emergencyTestRepo, "7"); Code(err) != ExitRefused {
				t.Fatalf("policy changed during read: %v", err)
			}
			// A standing stop/hold also prevents a later invocation.
			if err := emergencyCall(a, gitMerge, emergencyTestRepo, "7"); Code(err) != ExitRefused {
				t.Fatalf("standing policy: %v", err)
			}
			if mutations != 0 {
				t.Fatalf("%d GitHub mutations past %s", mutations, target)
			}
		})
	}
}

func TestEmergencyHostSessionCannotImpersonateGuideOrInheritGrant(t *testing.T) {
	for _, harness := range []string{"CODEX", "OMP", "AGY"} {
		t.Run(harness, func(t *testing.T) {
			a := emergencyApp(t)
			key := "BEEKEEPER_" + harness + "_AGENT"
			t.Setenv(key, "original")
			t.Setenv("BEEKEEPER_AGENT_NAME", guideName)
			setGuide := func() {
				t.Helper()
				me, err := a.caller()
				if err != nil {
					t.Fatal(err)
				}
				if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
					st.Guide.Holder.Party = me
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			setGuide()
			if err := emergencyCall(a, "on", emergencyReasonFlag, "Wait.", untilFlag, "10m"); err != nil {
				t.Fatal(err)
			}
			t.Setenv(key, successorName)
			if err := emergencyCall(a, "auto-install", "pause", emergencyReasonFlag, "Wait."); Code(err) != ExitRefused {
				t.Errorf("different session authorized: %v", err)
			}
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if st.AutoInstallPaused {
				t.Error("unauthorized pause persisted")
			}
			setGuide()
			if err := emergencyCall(a, "hold", "set", "starts", emergencyReasonFlag, "Wait."); Code(err) != ExitRefused {
				t.Fatalf("successor reused grant: %v", err)
			}
			st, err = a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Holds) != 0 {
				t.Fatal("unauthorized hold persisted")
			}
			if err := emergencyCall(a, "on", emergencyReasonFlag, "Wait.", untilFlag, "10m"); err != nil {
				t.Fatal(err)
			}
			if err := emergencyCall(a, "hold", "set", "starts", emergencyReasonFlag, "Wait."); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEmergencySameSession(t *testing.T) {
	base := state.Party{Session: "session", HostSession: "host-session", Name: guideName, Person: "owner@example.invalid", Host: installationA}
	for _, tc := range []struct {
		name  string
		other state.Party
		want  bool
	}{
		{"same session", base, true},
		{"session only", state.Party{Session: base.Session}, true},
		{"host session only", state.Party{HostSession: base.HostSession}, true},
		{"display name only", state.Party{Name: base.Name}, false},
		{"different session", state.Party{Session: successorName, Name: base.Name}, false},
		{"different host session", state.Party{HostSession: successorName, Name: base.Name}, false},
		{"different person", state.Party{Session: base.Session, Person: "other@example.invalid"}, false},
		{"different host", state.Party{HostSession: base.HostSession, Host: "installation-b"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := emergencySameSession(base, tc.other); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	if emergencySameSession(state.Party{Name: guideName}, state.Party{Name: guideName}) {
		t.Fatal("display names matched without sessions")
	}
}

const emergencyAdminHead = "abc1234"

// adminMergeCall runs an admin merge of o/r#7 at the head under the person's
// words, the pull request as given, and returns gh's merge calls.
func adminMergeCall(t *testing.T, words string, p superloop.Pull, behind int, authority bool) ([][]string, error) {
	t.Helper()
	a := emergencyApp(t)
	if err := emergencyCall(a, "on", emergencyReasonFlag, words, untilFlag, "10m"); err != nil {
		t.Fatal(err)
	}
	p.Ref = emergencyTestRepo + "#7"
	oldRead, oldBehind, oldGH := readLoopPull, readReviewedBehind, github.RunGH
	t.Cleanup(func() { readLoopPull, readReviewedBehind, github.RunGH = oldRead, oldBehind, oldGH })
	readReviewedBehind = func(context.Context, superloop.Pull) (int, error) { return behind, nil }
	readLoopPull = func(context.Context, string) (superloop.Pull, error) { return p, nil }
	var calls [][]string
	github.RunGH = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, slices.Clone(args))
		return nil, nil
	}
	if authority {
		if err := a.store.Record(about(event(loopParty, "merge.authorized", "%s at %s: go", "https://github.com/o/r/pull/7", emergencyAdminHead), p.Ref)); err != nil {
			t.Fatal(err)
		}
	}
	err := emergencyCall(a, gitMerge, emergencyTestRepo, "7", "--admin", "--head", emergencyAdminHead)
	return calls, err
}

func TestEmergencyAdminMergeNeedsTheNamedDecisionAtTheExactHead(t *testing.T) {
	green := superloop.Pull{State: superloop.Open, Head: emergencyAdminHead, Base: "main", Green: true,
		ReviewDecision: emergencyReviewRequired, Mergeable: emergencyMergeable, MergeState: emergencyBlocked}
	named := "Admin merge o/r#7 now."
	calls, err := adminMergeCall(t, named, green, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0][1] != gitMerge || !slices.Contains(calls[0], "--admin") || calls[0][len(calls[0])-1] != emergencyAdminHead {
		t.Fatalf("merge: %v", calls)
	}
	with := func(change func(*superloop.Pull)) superloop.Pull {
		p := green
		change(&p)
		return p
	}
	for name, tc := range map[string]struct {
		words     string
		pull      superloop.Pull
		behind    int
		authority bool
	}{
		"words name another": {words: "Admin merge o/r#77 now.", pull: green, authority: true},
		"words name nothing": {words: "Merge them all.", pull: green, authority: true},
		"head moved":         {words: named, pull: with(func(p *superloop.Pull) { p.Head = "def4567" }), authority: true},
		"checks failing":     {words: named, pull: with(func(p *superloop.Pull) { p.Green = false }), authority: true},
		"draft":              {words: named, pull: with(func(p *superloop.Pull) { p.Draft = true }), authority: true},
		"no go":              {words: named, pull: green},
		"behind state":       {words: named, pull: with(func(p *superloop.Pull) { p.MergeState = mergeStateBehind }), authority: true},
		"blocked and behind": {words: named, pull: green, behind: 2, authority: true},
		"conflicting":        {words: named, pull: with(func(p *superloop.Pull) { p.Mergeable = mergeableConflicting }), authority: true},
		"review refused":     {words: named, pull: with(func(p *superloop.Pull) { p.ReviewDecision = "CHANGES_REQUESTED" }), authority: true},
		"no base":            {words: named, pull: with(func(p *superloop.Pull) { p.Base = "" }), authority: true},
	} {
		t.Run(name, func(t *testing.T) {
			calls, err := adminMergeCall(t, tc.words, tc.pull, tc.behind, tc.authority)
			if Code(err) != ExitRefused || len(calls) != 0 {
				t.Fatalf("err %v, calls %v", err, calls)
			}
		})
	}
}

func TestEmergencyAdminNeedsItsHead(t *testing.T) {
	a := emergencyApp(t)
	if err := emergencyCall(a, gitMerge, emergencyTestRepo, "7", "--admin"); Code(err) != ExitUsage {
		t.Fatalf("no head: %v", err)
	}
}
