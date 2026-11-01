//go:build unix

package cmd

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	candidateA = "v1.2.4-rc.1"
	candidateB = "v1.2.4-rc.2"
)

// promoteArgv is devctl release promote o/r with extra flags.
func promoteArgv(extra ...string) []string {
	return append([]string{merge.Tool, "release", promoteArg, scratchRepo}, extra...)
}

// stubCandidate answers promoteCandidate with *newest.
func stubCandidate(t *testing.T, newest *string) *int {
	t.Helper()
	asked := new(int)
	was := promoteCandidate
	t.Cleanup(func() { promoteCandidate = was })
	promoteCandidate = func(context.Context, string) (string, error) {
		*asked++
		return *newest, nil
	}
	return asked
}

// promotePlace is o/r's waiting promotion in a free lane with a fresh
// budget, queued for candidateA, and the queued run that waits for it.
func promotePlace(t *testing.T) (*app, *gateRun) {
	t.Helper()
	me, gate := state.Party{Session: "s1", Name: ownerName}, sleeper(t)
	a := queueApp(t, state.Merge{Repo: scratchRepo, Lane: scratchRepo, By: me, PID: gate, Phase: state.Waiting,
		Joined: time.Now(), Seen: time.Now(), Candidate: candidateA})
	// The machine's own devctl processes count against the cap.
	a.cfg.Merge.BudgetFresh, a.cfg.Merge.Cap = config.Duration{Duration: time.Hour}, 1000
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Budget = &state.Budget{Remaining: 5000, Limit: 5000, Reset: time.Now().Add(time.Hour), At: time.Now()}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	return a, &gateRun{app: a, ctx: context.Background(), argv: promoteArgv(), repo: scratchRepo, lane: a.cfg.LaneOf(scratchRepo),
		me: me, pid: os.Getpid(), queued: true, placed: true, from: gate}
}

// A pinned promotion behind a busy lane waits on in a run of its own (exit
// 76), its place recording the candidate it pins, not the newest; the run
// carries the pin.
func TestAQueuedPromotionRecordsItsPinnedCandidate(t *testing.T) {
	stubGitHub(t, github.Open, "")
	_, launched := stubSelf(t)
	newest := candidateB
	asked := stubCandidate(t, &newest)
	a := busyLaneApp(t)

	if err := a.gate(context.Background(), promoteArgv("--version", candidateA), 0, 0, false); Code(err) != ExitGateQueued {
		t.Fatalf("exit %d (%v), want %d", Code(err), err, ExitGateQueued)
	}
	if spec := launched(); !slices.Contains(spec.Argv, "--version") || !slices.Contains(spec.Argv, candidateA) {
		t.Errorf("the queued run drops the pin: %q", spec.Argv)
	}
	if *asked != 0 {
		t.Errorf("a pinned promotion read the newest candidate %d times", *asked)
	}
	st, _ := a.store.Read()
	i := slices.IndexFunc(st.Merges, func(m state.Merge) bool { return m.PR == 0 && m.Phase == state.Waiting })
	if i < 0 || st.Merges[i].Candidate != candidateA {
		t.Fatalf("the promotion's place does not record %s: %+v", candidateA, st.Merges)
	}
	evs, _ := a.store.Events(0, func(e state.Event) bool { return e.Verb == "merge.queued" })
	if len(evs) == 0 || evs[0].Detail != "o/r promote in lane o/r, for candidate "+candidateA {
		t.Errorf("queued events %+v", evs)
	}
}

// An unpinned promotion is never queued: where it would wait on outside the
// call it is refused (77), its place dropped, and the refusal names the
// newest candidate to pin.
func TestAnUnpinnedPromotionIsNeverQueued(t *testing.T) {
	stubGitHub(t, github.Open, "")
	stubSelf(t)
	newest := candidateA
	stubCandidate(t, &newest)
	a := busyLaneApp(t)

	err := a.gate(context.Background(), promoteArgv(), 0, 0, false)
	if Code(err) != ExitGateRefused {
		t.Fatalf("exit %d (%v), want %d", Code(err), err, ExitGateRefused)
	}
	if d, want := lastEventOf(t, a, "merge.refused"), "release promote o/r --version <the candidate you proved> (the newest is "+candidateA+" now)"; !strings.Contains(d, want) {
		t.Errorf("refusal %q, want %q", d, want)
	}
	if st, _ := a.store.Read(); slices.ContainsFunc(st.Merges, func(m state.Merge) bool { return m.PR == 0 }) {
		t.Errorf("the unpinned promotion kept a place: %+v", st.Merges)
	}
	if d := lastEventOf(t, a, "merge.queued"); strings.Contains(d, "run of its own") {
		t.Errorf("handed to a run of its own: %q", d)
	}
}

// A newer candidate cut while a pinned promotion is queued refuses it at its
// turn, so a stable release is never cut at an older release candidate; with
// its pinned candidate still newest, devctl runs with the pin.
func TestAPinnedPromotionRefusesWhenANewerCandidateAppears(t *testing.T) {
	noSystemd(t)
	stubGitHub(t, github.Open, "")
	args := filepath.Join(t.TempDir(), "args")
	fakeDevctl(t, `echo "$@" > `+args+`; echo '{"repositories":[{"candidate":"`+candidateA+`","state":"dispatched"}]}'`)
	newest := candidateB
	asked := stubCandidate(t, &newest)

	a, g := promotePlace(t)
	g.argv, g.pinned = promoteArgv("--version", candidateA), true
	why, err := g.step()
	if Code(err) != ExitGateRefused || why != "" {
		t.Fatalf("step: %q, exit %d (%v), want a refusal", why, Code(err), err)
	}
	if *asked == 0 {
		t.Errorf("the pinned promotion was not judged against the newest candidate")
	}
	want := "o/r promote: o/r promote was queued for candidate " + candidateA + ", the newest is candidate " + candidateB + " now: nothing promoted"
	if d := lastEventOf(t, a, "merge.refused"); !strings.HasPrefix(d, want) {
		t.Errorf("refusal %q, want %q…", d, want)
	}
	if st, _ := a.store.Read(); len(st.Merges) != 0 {
		t.Errorf("the refused promotion kept a place: %+v", st.Merges)
	}
	if _, err := os.Stat(args); err == nil {
		t.Errorf("devctl ran for a stale pinned candidate")
	}

	a, g = promotePlace(t)
	g.argv, g.pinned = promoteArgv("--version", candidateA), true
	newest = candidateA
	if why, err := g.step(); err != nil || why != "" {
		t.Fatalf("step with current candidate: %q, %v", why, err)
	}
	if raw, _ := os.ReadFile(args); !strings.Contains(string(raw), "--version "+candidateA) { //nolint:gosec // the test's file
		t.Errorf("devctl ran %q, want the pin", raw)
	}
	if d := lastEventOf(t, a, verbMerged); !strings.HasPrefix(d, "o/r promote exit 0, release v1.2.4") {
		t.Errorf("merged event %q", d)
	}
}

// A repository hold that waits on the repository's own release lets its
// promote run, the step that cuts the release it waits on, and still
// refuses a merge into the repository. A hold whose probe waits on
// something else keeps the promote waiting.
func TestAHoldWaitingOnTheReleaseLetsItsOwnPromoteThrough(t *testing.T) {
	noSystemd(t)
	stubGitHub(t, github.Open, "")
	fakeDevctl(t, `echo '{"repositories":[{"candidate":"`+candidateA+`","state":"dispatched"}]}'`)
	newest := candidateA
	stubCandidate(t, &newest)
	hold := func(probe string) state.Hold {
		return state.Hold{Target: scratchRepo, Reason: "promote v1.2.4 first", By: state.Party{Name: aheadName}, At: time.Now(), LiftWhen: probe}
	}

	a, g := promotePlace(t)
	setHold(t, a, hold("devctl pr wait "+scratchRepo+" 6"))
	if why, err := g.step(); err != nil || !strings.HasPrefix(why, "held in lane o/r") {
		t.Fatalf("under a hold waiting on no release: %q, %v, want held", why, err)
	}

	a, g = promotePlace(t)
	setHold(t, a, hold("gh release view v1.2.4 --repo "+scratchRepo))
	if err := a.gate(context.Background(), mergeArgv(scratchRepo), 0, 0, false); Code(err) != ExitGateRefused {
		t.Errorf("a merge into the held repository: exit %d (%v), want %d", Code(err), err, ExitGateRefused)
	}
	if why, err := g.step(); err != nil || why != "" {
		t.Fatalf("its own promote: %q, %v", why, err)
	}
	if d := lastEventOf(t, a, verbMerged); !strings.HasPrefix(d, "o/r promote exit 0, release v1.2.4") {
		t.Errorf("merged event %q", d)
	}
}

// A promotion whose turn comes when another candidate is newest promotes
// nothing: it is refused, its place leaves the lane, and the refusal names
// both candidates. With its own candidate still newest, it runs.
func TestAPromotionQueuedForOneCandidateNeverPromotesAnother(t *testing.T) {
	noSystemd(t)
	stubGitHub(t, github.Open, "")
	fakeDevctl(t, `echo '{"repositories":[{"candidate":"`+candidateB+`","state":"dispatched"}]}'`)

	a, g := promotePlace(t)
	newest := candidateB
	stubCandidate(t, &newest)
	why, err := g.step()
	if Code(err) != ExitGateRefused || why != "" {
		t.Fatalf("step: %q, exit %d (%v), want a refusal", why, Code(err), err)
	}
	want := "o/r promote: o/r promote was queued for candidate " + candidateA + ", the newest is candidate " + candidateB + " now: nothing promoted"
	if d := lastEventOf(t, a, "merge.refused"); !strings.HasPrefix(d, want) {
		t.Errorf("refusal %q, want %q…", d, want)
	}
	if st, _ := a.store.Read(); len(st.Merges) != 0 {
		t.Errorf("the refused promotion kept a place: %+v", st.Merges)
	}
	if d := lastEventOf(t, a, verbMerged); d != "" {
		t.Errorf("promoted: %q", d)
	}

	a, g = promotePlace(t)
	newest = candidateA
	if why, err := g.step(); err != nil || why != "" {
		t.Fatalf("its own candidate: %q, %v", why, err)
	}
	if d := lastEventOf(t, a, verbMerged); !strings.HasPrefix(d, "o/r promote exit 0, release v1.2.4") {
		t.Errorf("merged event %q", d)
	}
}

// lanes drop takes a waiting promotion out of its lane, and the gate that
// waited for the place refuses instead of joining again.
func TestLanesDropTakesAPromotionOut(t *testing.T) {
	stubGitHub(t, github.Open, "")
	newest := candidateA
	stubCandidate(t, &newest)
	a, g := promotePlace(t)
	// A merge ahead holds the lane, so the promotion waits.
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = append([]state.Merge{{Repo: scratchRepo, PR: 6, Lane: scratchRepo, By: state.Party{Name: aheadName}, PID: sleeper(t),
			Phase: state.Waiting, Joined: time.Now().Add(-time.Minute), Seen: time.Now()}}, st.Merges...)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if why, err := g.step(); err != nil || !strings.Contains(why, "behind o/r#6") {
		t.Fatalf("step before the drop: %q, %v", why, err)
	}

	var out bytes.Buffer
	a.out = &out
	c := a.lanesCmd()
	c.SetArgs([]string{"drop", scratchRepo, promoteArg})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "dropped o/r promote from its lane\n" {
		t.Errorf("drop said %q", out.String())
	}
	st, _ := a.store.Read()
	if slices.ContainsFunc(st.Merges, func(m state.Merge) bool { return m.PR == 0 }) {
		t.Fatalf("the promotion kept its place: %+v", st.Merges)
	}

	why, err := g.step()
	if Code(err) != ExitGateRefused || why != "" {
		t.Fatalf("step after the drop: %q, exit %d (%v), want a refusal", why, Code(err), err)
	}
	if d := lastEventOf(t, a, "merge.refused"); !strings.Contains(d, "o/r promote was taken out of lane o/r (beekeeper lanes drop): nothing ran") {
		t.Errorf("refusal %q", d)
	}
	if st, _ := a.store.Read(); slices.ContainsFunc(st.Merges, func(m state.Merge) bool { return m.PR == 0 }) {
		t.Errorf("the gate joined the lane again: %+v", st.Merges)
	}
}

func TestDropArg(t *testing.T) {
	for arg, want := range map[string]int{promoteArg: 0, "7": 7} {
		if got, err := dropArg(arg); err != nil || got != want {
			t.Errorf("%s: %d, %v; want %d", arg, got, err, want)
		}
	}
	for _, arg := range []string{"0", "-1", "promotion", ""} {
		if _, err := dropArg(arg); err == nil {
			t.Errorf("%q: accepted", arg)
		}
	}
}

// The gate names the command whose turn came: a promotion is "promoting
// o/r" and a refused one says "nothing promoted" with devctl's state, never
// "merging"; a merge stays "merging o/r#7".
func TestTheGateNamesTheCommandWhoseTurnCame(t *testing.T) {
	noSystemd(t)
	stubGitHub(t, github.Open, "")
	newest := candidateA
	stubCandidate(t, &newest)

	fakeDevctl(t, `echo '{"repositories":[{"candidate":"`+candidateA+`","state":"not_built"}]}'; exit 1`)
	a, g := promotePlace(t)
	var err error
	stderr := gateStderr(t, func() { _, err = g.step() })
	if Code(err) != 1 {
		t.Fatalf("refused promotion: exit %d (%v), want 1", Code(err), err)
	}
	if d := lastEventOf(t, a, verbPromoting); d != "o/r in lane o/r" {
		t.Errorf("promoting event %q", d)
	}
	if d := lastEventOf(t, a, verbMerging); d != "" {
		t.Errorf("a promotion logged merging: %q", d)
	}
	if d := lastEventOf(t, a, "merge.failed"); !strings.Contains(d, "nothing promoted (not_built)") {
		t.Errorf("failed event %q", d)
	}
	if !strings.Contains(stderr, GatePrefix+"nothing promoted (not_built) (exit 1); o/r promote left lane o/r") || strings.Contains(stderr, "merg") {
		t.Errorf("stderr %q, want the promotion's outcome and no merge", stderr)
	}

	fakeDevctl(t, `echo '{"repositories":[{"candidate":"`+candidateA+`","state":"dispatched"}]}'`)
	a, g = promotePlace(t)
	stderr = gateStderr(t, func() { _, err = g.step() })
	if err != nil {
		t.Fatalf("dispatched promotion: %v", err)
	}
	if !strings.Contains(stderr, GatePrefix+"promoted o/r: release v1.2.4 dispatched") || strings.Contains(stderr, "merg") {
		t.Errorf("stderr %q, want the dispatched promotion", stderr)
	}
	if d := lastEventOf(t, a, verbMerging); d != "" {
		t.Errorf("a promotion logged merging: %q", d)
	}

	fakeDevctl(t, `echo '`+mergedDoc+`'`)
	stubBaseRelease(t, github.BaseRelease{Base: "main", Auto: true}, nil)
	a, g = promotePlace(t)
	g.pr, g.argv, g.candidate = 7, mergeArgv(scratchRepo), ""
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges[0].PR, st.Merges[0].Candidate = 7, ""
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	stderr = gateStderr(t, func() { _, err = g.step() })
	if err != nil {
		t.Fatalf("merge: exit %d (%v), stderr %q", Code(err), err, stderr)
	}
	if d := lastEventOf(t, a, verbMerging); d != "o/r#7 in lane o/r" {
		t.Errorf("merging event %q", d)
	}
	if d := lastEventOf(t, a, verbPromoting); d != "" || strings.Contains(stderr, "promot") {
		t.Errorf("a merge said promoting: event %q, stderr %q", d, stderr)
	}
}

// A lane hold that admits the line's promoter lets that promoter's own
// promote through; every other session's promote and merge on the line is
// refused (exit 77) with a gate line naming the session the hold admits.
func TestAHoldAdmittingThePromoterLetsItsPromoteThrough(t *testing.T) {
	noSystemd(t)
	stubGitHub(t, github.Open, "")
	fakeDevctl(t, `echo '{"repositories":[{"candidate":"`+candidateA+`","state":"dispatched"}]}'`)
	newest := candidateA
	stubCandidate(t, &newest)
	hold := state.Hold{Target: merge.LanePrefix + scratchRepo, Reason: "promoting v1.2.4", By: state.Party{Name: aheadName}, At: time.Now(),
		ExceptSession: ownerName}
	admitted := `session "` + ownerName + `"`

	a, g := promotePlace(t)
	setHold(t, a, hold)
	other := &gateRun{app: a, ctx: context.Background(), argv: promoteArgv(), repo: scratchRepo, lane: g.lane,
		me: state.Party{Session: "s2", Name: aheadName}, pid: os.Getpid()}
	var err error
	stderr := gateStderr(t, func() { _, err = other.step() })
	if Code(err) != ExitGateRefused || !strings.Contains(stderr, GatePrefix+"refused") || !strings.Contains(stderr, admitted) {
		t.Errorf("another session's promote: exit %d (%v), stderr %q, want %d naming %s", Code(err), err, stderr, ExitGateRefused, admitted)
	}
	promoter := a.as
	a.as = aheadName
	stderr = gateStderr(t, func() { err = a.gate(context.Background(), mergeArgv(scratchRepo), 0, 0, false) })
	a.as = promoter
	if Code(err) != ExitGateRefused || !strings.Contains(stderr, admitted) {
		t.Errorf("another session's merge: exit %d (%v), stderr %q, want %d naming %s", Code(err), err, stderr, ExitGateRefused, admitted)
	}
	if why, err := g.step(); err != nil || why != "" {
		t.Fatalf("the admitted promoter's promote: %q, %v", why, err)
	}
	if d := lastEventOf(t, a, verbMerged); !strings.HasPrefix(d, "o/r promote exit 0, release v1.2.4") {
		t.Errorf("merged event %q", d)
	}
}

// .github/release-gate.custom.sh refuses cutting a stable release when a newer
// release candidate for the same version is not an ancestor of HEAD or when
// HEAD is an ancestor of an existing release or candidate tag, and passes when
// promoting the newest candidate or a new patch commit.
func TestReleaseGateCustomScriptRefusesOlderCandidate(t *testing.T) {
	script, err := filepath.Abs(filepath.Join("..", ".github", "release-gate.custom.sh"))
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	gitRun := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...) //nolint:gosec // test helper
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGate := func(tag string) (string, error) {
		t.Helper()
		cmd := exec.Command("bash", script, tag) //nolint:gosec // test script in repository
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	gitRun("init")
	gitRun("commit", "--allow-empty", "-m", "feat: first candidate")
	gitRun("tag", "v0.171.0-rc.110")
	gitRun("commit", "--allow-empty", "-m", "feat: second candidate")
	gitRun("tag", "v0.171.0-rc.111")

	if out, err := runGate("v0.171.0"); err != nil {
		t.Fatalf("promoting newest candidate v0.171.0-rc.111 failed: %v\n%s", err, out)
	}

	gitRun("checkout", "--detach", "v0.171.0-rc.110")
	if out, err := runGate("v0.171.0"); err == nil || !strings.Contains(out, "v0.171.0-rc.111") {
		t.Fatalf("promoting older candidate v0.171.0-rc.110: err=%v out=%q; want refusal naming v0.171.0-rc.111", err, out)
	}
	if out, err := runGate("v0.170.1"); err == nil || !strings.Contains(out, "HEAD is an ancestor of existing release tag") {
		t.Fatalf("cutting stable release at ancestor of v0.171.0-rc.111: err=%v out=%q; want ancestor refusal", err, out)
	}

	gitRun("checkout", "--detach", "v0.171.0-rc.111")
	gitRun("tag", "v0.171.0")
	gitRun("commit", "--allow-empty", "-m", "fix: patch after stable")
	if out, err := runGate("v0.171.1"); err != nil {
		t.Fatalf("patch release at new HEAD failed: %v\n%s", err, out)
	}
}
