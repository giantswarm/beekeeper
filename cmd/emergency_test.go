package cmd

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
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
	for _, method := range []string{"", mergeMethodSquash, mergeMethodRebase, gitMerge} {
		t.Run("method="+method, func(t *testing.T) {
			flag := method
			if flag == "" {
				flag = mergeMethodSquash
			}
			a := emergencyApp(t)
			a.cfg.Merge.Methods = map[string]string{emergencyTestRepo: method}
			const quote = "Merge the approved pull requests now."
			if err := emergencyCall(a, "on", emergencyReasonFlag, quote, untilFlag, "10m"); err != nil {
				t.Fatal(err)
			}
			old := github.RunGH
			t.Cleanup(func() { github.RunGH = old })
			oldRead := loop.ReadLoopPull
			t.Cleanup(func() { loop.ReadLoopPull = oldRead })
			loop.ReadLoopPull = func(context.Context, string) (superloop.Pull, error) { return superloop.Pull{}, nil }
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
			want := []string{"pr", gitMerge, "7", "--repo", emergencyTestRepo, "--" + flag, "--match-head-commit", "abc123"}
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
		})
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
	l := &loop.Loop{Host: a}
	if _, err := l.Install(context.Background(), superloop.Action{Version: "v1.2.3"}); !errors.Is(err, loop.ErrLoopLater) {
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

func TestEmergencyOnlyAuthenticatedSuccessorInheritsGrant(t *testing.T) {
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
			original := *st.Emergency
			setGuide()
			if err := emergencyCall(a, "hold", "set", "starts", emergencyReasonFlag, "Wait."); err != nil {
				t.Fatalf("authenticated successor lost role grant: %v", err)
			}
			st, err = a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Holds) != 1 {
				t.Fatal("authorized successor hold missing")
			}
			if *st.Emergency != original {
				t.Fatal("relay changed grant provenance or expiry")
			}
			a.now = original.Until
			if _, err := a.emergencyAction("expired successor"); Code(err) != ExitRefused {
				t.Fatalf("successor extended expiry: %v", err)
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
func adminMergeCall(t *testing.T, words string, p superloop.Pull, behind int, authority bool, methods ...string) ([][]string, error) {
	t.Helper()
	a := emergencyApp(t)
	if len(methods) > 0 {
		a.cfg.Merge.Methods = map[string]string{emergencyTestRepo: methods[0]}
	}
	if err := emergencyCall(a, "on", emergencyReasonFlag, words, untilFlag, "10m"); err != nil {
		t.Fatal(err)
	}
	p.Ref = emergencyTestRepo + "#7"
	oldRead, oldBehind, oldGH := loop.ReadLoopPull, readReviewedBehind, github.RunGH
	t.Cleanup(func() { loop.ReadLoopPull, readReviewedBehind, github.RunGH = oldRead, oldBehind, oldGH })
	readReviewedBehind = func(context.Context, superloop.Pull) (int, error) { return behind, nil }
	loop.ReadLoopPull = func(context.Context, string) (superloop.Pull, error) { return p, nil }
	var calls [][]string
	github.RunGH = func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == workerGitHubAPIGroup {
			if args[1] == "graphql" {
				return []byte(`{"data":{"repository":{"ref":{"branchProtectionRule":null}}}}`), nil
			}
			return []byte(`[[{"type":"pull_request","parameters":{"require_extra_approval_for_unattributed_changes":true}}]]`), nil
		}
		calls = append(calls, slices.Clone(args))
		return nil, nil
	}
	if authority {
		if err := a.store.Record(about(event(loop.LoopParty, "merge.authorized", "%s at %s: go", "https://github.com/o/r/pull/7", emergencyAdminHead), p.Ref)); err != nil {
			t.Fatal(err)
		}
	}
	err := emergencyCall(a, gitMerge, emergencyTestRepo, "7", "--admin", "--head", emergencyAdminHead)
	results, logErr := a.store.Events(0, func(e state.Event) bool { return e.Verb == "emergency.result" })
	if logErr != nil {
		t.Fatal(logErr)
	}
	if len(results) != 1 {
		t.Fatalf("want one audited outcome, got %v", results)
	}
	for _, detail := range []string{"https://github.com/o/r/pull/7", emergencyAdminHead, words} {
		if !strings.Contains(results[0].Detail, detail) {
			t.Fatalf("outcome missing %q: %s", detail, results[0].Detail)
		}
	}
	return calls, err
}

func TestEmergencyAdminMergeNeedsAuthorityAtTheExactHead(t *testing.T) {
	green := superloop.Pull{State: superloop.Open, Head: emergencyAdminHead, Base: mainBranch, Green: true,
		ReviewDecision: emergencyReviewRequired, Mergeable: emergencyMergeable, MergeState: emergencyBlocked}
	generalGrant := "Merge them all."
	for _, words := range []string{
		"Admin merge o/r#7 now.",
		generalGrant,
		"Yes use --admin",
		"Stop asking me for merges!!!! I told you to merge and be pragmatic.",
	} {
		t.Run(words, func(t *testing.T) {
			calls, err := adminMergeCall(t, words, green, 0, true)
			if err != nil {
				t.Fatal(err)
			}
			if len(calls) != 1 || calls[0][1] != gitMerge || !slices.Contains(calls[0], "--admin") || calls[0][len(calls[0])-1] != emergencyAdminHead {
				t.Fatalf("merge: %v", calls)
			}
		})
	}
	t.Run("behind without strict protection", func(t *testing.T) {
		calls, err := adminMergeCall(t, generalGrant, green, 2, true)
		if err != nil || len(calls) != 1 {
			t.Fatalf("behind merge refused: %v, calls %v", err, calls)
		}
	})
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
		"head moved":     {words: generalGrant, pull: with(func(p *superloop.Pull) { p.Head = "def4567" }), authority: true},
		"checks failing": {words: generalGrant, pull: with(func(p *superloop.Pull) { p.Green = false }), authority: true},
		"draft":          {words: generalGrant, pull: with(func(p *superloop.Pull) { p.Draft = true }), authority: true},
		"no go":          {words: generalGrant, pull: green},
		"behind state":   {words: generalGrant, pull: with(func(p *superloop.Pull) { p.MergeState = mergeStateBehind }), authority: true},
		"conflicting":    {words: generalGrant, pull: with(func(p *superloop.Pull) { p.Mergeable = loop.MergeableConflicting }), authority: true},
		"review refused": {words: generalGrant, pull: with(func(p *superloop.Pull) { p.ReviewDecision = "CHANGES_REQUESTED" }), authority: true},
		"no base":        {words: generalGrant, pull: with(func(p *superloop.Pull) { p.Base = "" }), authority: true},
	} {
		t.Run(name, func(t *testing.T) {
			calls, err := adminMergeCall(t, tc.words, tc.pull, tc.behind, tc.authority)
			if Code(err) != ExitRefused || len(calls) != 0 {
				t.Fatalf("err %v, calls %v", err, calls)
			}
		})
	}
}

// An unset GitHub review decision is normal when protection requires no review.
// It still needs exact-head desk authority, green checks and the bounded grant.
func TestEmergencyAdminMergeWithUnsetReviewDecision(t *testing.T) {
	green := superloop.Pull{State: superloop.Open, Head: emergencyAdminHead, Base: mainBranch, Green: true,
		Mergeable: emergencyMergeable, MergeState: emergencyClean}
	for _, name := range []string{"accepted", "unset head moved", "unset checks failing", litDraft, noteStatusClosed, "unset without authority"} {
		t.Run(name, func(t *testing.T) {
			p := green
			switch name {
			case "unset head moved":
				p.Head = "def4567"
			case "unset checks failing":
				p.Green = false
			case litDraft:
				p.Draft = true
			case noteStatusClosed:
				p.State = "CLOSED"
			}
			calls, err := adminMergeCall(t, "Merge them all.", p, 0, name != "unset without authority")
			if name != "accepted" {
				if Code(err) != ExitRefused || len(calls) != 0 {
					t.Fatalf("err %v, calls %v", err, calls)
				}
				return
			}
			want := strings.Fields("pr merge 7 " + loop.LoopRepoFlag + " " + emergencyTestRepo + " --squash --admin --match-head-commit " + emergencyAdminHead)
			if err != nil || len(calls) != 1 || !slices.Equal(calls[0], want) {
				t.Fatalf("err %v, calls %v", err, calls)
			}
		})
	}
}

func TestEmergencyAdminMergeUsesConfiguredMethod(t *testing.T) {
	green := superloop.Pull{State: superloop.Open, Head: emergencyAdminHead, Base: mainBranch, Green: true,
		ReviewDecision: emergencyReviewRequired, Mergeable: emergencyMergeable, MergeState: emergencyBlocked}
	for _, method := range []string{mergeMethodSquash, mergeMethodRebase, gitMerge} {
		t.Run(method, func(t *testing.T) {
			calls, err := adminMergeCall(t, "Merge them all.", green, 0, true, method)
			if err != nil || len(calls) != 1 || !slices.Contains(calls[0], "--"+method) || !slices.Contains(calls[0], "--admin") {
				t.Fatalf("method %s: calls %v, error %v", method, calls, err)
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

// Exercise the real pull parser and behind reader: green reported checks do
// not prove that the required security scan ever ran on the authorized head.
func TestEmergencyAdminRefusesAbsentRequiredScan(t *testing.T) {
	a := emergencyApp(t)
	if err := emergencyCall(a, "on", emergencyReasonFlag, "Admin merge o/r#7 now.", untilFlag, "10m"); err != nil {
		t.Fatal(err)
	}
	if err := a.store.Record(about(event(loop.LoopParty, "merge.authorized", "https://github.com/o/r/pull/7 at %s: go", emergencyAdminHead), "o/r#7")); err != nil {
		t.Fatal(err)
	}
	old := github.RunGH
	t.Cleanup(func() { github.RunGH = old })
	requiredRead, merged := false, false
	github.RunGH = func(_ context.Context, args ...string) ([]byte, error) {
		call := strings.Join(args, " ")
		switch {
		case args[0] == "pr" && args[1] == emergencyViewCommand:
			return []byte(`{"state":"OPEN","headRefOid":"abc1234","baseRefName":"main","reviewDecision":"REVIEW_REQUIRED","mergeable":"MERGEABLE","mergeStateStatus":"BLOCKED","statusCheckRollup":[{"name":"lint","status":"COMPLETED","conclusion":"SUCCESS"}]}`), nil
		case call == "api --paginate repos/o/r/actions/runs?head_sha="+emergencyAdminHead+"&per_page=100":
			return []byte(`{"workflow_runs":[]}`), nil
		case strings.Contains(call, "/compare/"):
			return []byte("0"), nil
		case args[1] == "graphql":
			return []byte(`{"data":{"repository":{"ref":{"branchProtectionRule":null}}}}`), nil
		case strings.Contains(call, "rules/branches/"):
			requiredRead = true
			return []byte(`[[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"security-scan"}]}}]]`), nil
		case strings.Contains(call, "/check-runs"):
			return []byte(`[{"check_runs":[{"id":1,"name":"lint","status":"completed","conclusion":"success"}]}]`), nil
		case strings.Contains(call, "/statuses"):
			return []byte(`[[]]`), nil
		case args[0] == "pr" && args[1] == gitMerge:
			merged = true
			return nil, nil
		default:
			t.Fatalf("unexpected request: %v", args)
			return nil, nil
		}
	}
	err := emergencyCall(a, gitMerge, emergencyTestRepo, "7", "--admin", "--head", emergencyAdminHead)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "security-scan") || !requiredRead || merged {
		t.Fatalf("err %v; requirements read %v; merged %v", err, requiredRead, merged)
	}
}

func TestEmergencyRelayHandoverAndRevocation(t *testing.T) {
	a := emergencyApp(t)
	const words = "Unblock the system now."
	if err := emergencyCall(a, "on", emergencyReasonFlag, words, untilFlag, "10m"); err != nil {
		t.Fatal(err)
	}
	original, err := a.caller()
	if err != nil {
		t.Fatal(err)
	}
	successor := state.Party{Name: "Successor guide", Session: "successor-guide"}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide.Holder.Party = successor
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.emergencyAction("relieved guide"); Code(err) != ExitRefused {
		t.Fatalf("outgoing guide authorized: %v", err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", successor.Session)
	t.Setenv("CLAUDE_CODE_SESSION_NAME", successor.Name)
	grant, err := a.emergencyAction("successor")
	if err != nil {
		t.Fatal(err)
	}
	if grant.By != original || grant.Reason != words || !grant.Until.Equal(a.now.Add(10*time.Minute)) {
		t.Fatalf("grant changed: %+v", grant)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	a.cfg.Guide.Skill = guideName
	if err := a.printGuidePrompt(st.GuideRole(), nil, "", nil, st); err != nil {
		t.Fatal(err)
	}
	if err := emergencyCall(a); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{emergencyText(st, a.now), words, original.Session, successor.Session, successor.Name, grant.Until.Format(time.RFC3339)} {
		if !strings.Contains(a.out.(*bytes.Buffer).String(), want) {
			t.Fatalf("handover missing %q: %s", want, a.out)
		}
	}
	if err := emergencyCall(a, "off"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.emergencyAction("revoked"); Code(err) != ExitRefused {
		t.Fatalf("revoked grant authorized: %v", err)
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide.Holder.Party = original
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", original.Session)
	t.Setenv("CLAUDE_CODE_SESSION_NAME", original.Name)
	if _, err := a.emergencyAction("revoked predecessor"); Code(err) != ExitRefused {
		t.Fatalf("revoked grant restored: %v", err)
	}
}

func TestEmergencyMergeAcceptsOnlyTrustedExactHeadDeskApproval(t *testing.T) {
	for _, name := range []string{"accepted desk verdict", "previous head", "untrusted reviewer", "failing CI", "review rejected"} {
		t.Run(name, func(t *testing.T) {
			a := emergencyApp(t)
			if err := emergencyCall(a, "on", emergencyReasonFlag, "Merge approved work.", untilFlag, "10m"); err != nil {
				t.Fatal(err)
			}
			oldRead, oldGH := loop.ReadLoopPull, github.RunGH
			t.Cleanup(func() { loop.ReadLoopPull, github.RunGH = oldRead, oldGH })
			review := superloop.Review{Head: emergencyAdminHead, Verdict: superloop.Approved, Member: true, Body: loopApproved}
			if name == "previous head" {
				review.Head = "fedcba9"
			}
			if name == "untrusted reviewer" {
				review.Member = false
				review.Author = "untrusted-login"
				a.cfg.Supervisor.Ownership.Authors = []string{"trusted"}
			}
			if name == "review rejected" {
				review.Verdict = superloop.ChangesRequested
			}
			loop.ReadLoopPull = func(context.Context, string) (superloop.Pull, error) {
				return superloop.Pull{State: superloop.Open, Head: emergencyAdminHead, Green: name != "failing CI", Reviews: []superloop.Review{review}}, nil
			}
			merges := 0
			github.RunGH = func(_ context.Context, args ...string) ([]byte, error) {
				if args[1] == emergencyViewCommand {
					return []byte(`{"state":"OPEN","reviewDecision":"REVIEW_REQUIRED","headRefOid":"abc1234"}`), nil
				}
				merges++
				if slices.Contains(args, "--admin") || args[len(args)-1] != emergencyAdminHead {
					t.Fatalf("unsafe merge %v", args)
				}
				return nil, nil
			}
			err := emergencyCall(a, gitMerge, emergencyTestRepo, "7")
			if name == "accepted desk verdict" {
				if err != nil || merges != 1 {
					t.Fatalf("merge: %v calls %d", err, merges)
				}
			} else if Code(err) != ExitRefused || merges != 0 {
				t.Fatalf("refusal: %v calls %d", err, merges)
			}
		})
	}
}

func TestMergeReviewRequirementAcceptsCurrentDeskReviewFormat(t *testing.T) {
	previous := loop.ReadBaseMerge
	t.Cleanup(func() { loop.ReadBaseMerge = previous })
	// The stale review belongs to an unrelated head, not a base-only update.
	loop.ReadBaseMerge = func(context.Context, superloop.Pull, string) (bool, error) { return false, nil }
	a := emergencyApp(t)
	a.cfg.Merge.RequireReview = true
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	p := superloop.Pull{Ref: emergencyTestRepo + "#7", Head: emergencyAdminHead, Reviews: []superloop.Review{{Member: true, Verdict: superloop.Approved, Head: emergencyAdminHead, Body: "Review: approved\nHead reviewed: `abc1234`"}}}
	why, _, err := a.checkMergeHeadAuthority(context.Background(), st, p, p.Head)
	if err != nil || why != "" {
		t.Fatalf("current review rejected: %s %v", why, err)
	}
	p.Reviews[0].Head = "9876abc"
	why, _, err = a.checkMergeHeadAuthority(context.Background(), st, p, p.Head)
	if err != nil || why == "" {
		t.Fatalf("stale review accepted: %s %v", why, err)
	}
}
