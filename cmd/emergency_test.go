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
	for _, key := range []string{"BEEKEEPER_CODEX_AGENT", "BEEKEEPER_OMP_AGENT", "BEEKEEPER_AGY_AGENT", "CLAUDE_CODE_HOST_SESSION_ID"} {
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
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) { st.Guide.Holder.Session = "successor"; return nil, nil }); err != nil {
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
