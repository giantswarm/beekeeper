//go:build unix

package cmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
)

// localLane is a lane without an installation: nothing rolls.
var localLane = config.Lane{Name: scratchRepo, Repositories: []string{scratchRepo}}

// A merge in a lane without an installation ends devctl at the merge and
// confirms its release in its gate, its place releasing meanwhile: the
// lane's next merge may start beside the release, never merge before it is
// known; a confirmed release frees the lane.
func TestAMergeInALaneWithoutAnInstallationFreesItsLaneForTheNextCIWhileItsReleaseIsConfirmed(t *testing.T) {
	noSystemd(t)
	fakeDevctl(t, handCutDevctl)
	stubGitHub(t, github.Merged, "")
	stubBaseRelease(t, github.BaseRelease{Base: mainBranch, Auto: true}, nil)
	g := runningMerge(t, scratchRepo, localLane)
	var during state.State
	var starts, merges string
	asked := new([]string)
	was := releaseWait
	t.Cleanup(func() { releaseWait = was })
	releaseWait = func(_ context.Context, repo string, target ...string) []byte {
		*asked = append(*asked, repo+" "+strings.Join(target, " "))
		during = *gateState(t, g)
		q := merge.Queue(&during, localLane.Name)
		starts = q.Unsettled(time.Now(), time.Hour, true, func(int) bool { return true })
		merges = g.lanePending()
		return []byte(`{"verdict":"available","tag":"v1.2.4"}`)
	}
	var err error
	out := gateStderr(t, func() { err = startMerge(t, g) })
	if err != nil {
		t.Fatalf("exit %d, want 0\n%s", Code(err), out)
	}
	if len(*asked) != 1 || (*asked)[0] != scratchRepo+" --pr 7" {
		t.Errorf("release waits: %q, want one of %s --pr 7", *asked, scratchRepo)
	}
	if len(during.Merges) != 1 || during.Merges[0].Phase != state.Settling || !during.Merges[0].Releasing || during.Merges[0].PID != os.Getpid() {
		t.Errorf("while the release is confirmed, want the merge releasing in its gate: %+v", during.Merges)
	}
	if starts != "" {
		t.Errorf("the next merge may not start beside the release: %q", starts)
	}
	if !strings.Contains(merges, "o/r#7's release is being confirmed") {
		t.Errorf("the next merge may land before the release is known: %q", merges)
	}
	if st := gateState(t, g); len(st.Merges) != 0 {
		t.Errorf("the lane is not free once the release is known: %+v", st.Merges)
	}
	if d := lastEvent(t, g, "merge.release"); !strings.Contains(d, "o/r#7: release v1.2.4, lane o/r is free") {
		t.Errorf("release event: %q", d)
	}
	if !strings.Contains(out, "merged and released v1.2.4") {
		t.Errorf("gate lines:\n%s", out)
	}
}

// A failed release in a lane without an installation is the merge's exit
// 6, as devctl pr merge's own release wait ends, and the lane's next merge
// waits for that verdict; an unconfirmed one settles the lane by the
// settle rule.
func TestAFailedReleaseInALaneWithoutAnInstallationStopsTheNextMergeUntilItsVerdict(t *testing.T) {
	t.Run("failed", func(t *testing.T) {
		noSystemd(t)
		fakeDevctl(t, handCutDevctl)
		stubGitHub(t, github.Merged, "")
		stubBaseRelease(t, github.BaseRelease{Base: mainBranch, Auto: true}, nil)
		g := runningMerge(t, scratchRepo, localLane)
		var merges string
		was := releaseWait
		t.Cleanup(func() { releaseWait = was })
		releaseWait = func(context.Context, string, ...string) []byte {
			merges = g.lanePending()
			return []byte(`{"verdict":"ci_failed","reason":"push-to-registries failed","tag":"v1.2.4"}`)
		}
		var err error
		out := gateStderr(t, func() { err = startMerge(t, g) })
		if Code(err) != devctlReleaseFailed {
			t.Fatalf("exit %d, want %d\n%s", Code(err), devctlReleaseFailed, out)
		}
		if merges == "" {
			t.Error("the next merge could land while the release ran")
		}
		if !strings.Contains(out, "its release v1.2.4 failed (push-to-registries failed)") {
			t.Errorf("gate lines:\n%s", out)
		}
		if d := lastEvent(t, g, "merge.release"); !strings.Contains(d, "release v1.2.4 failed") {
			t.Errorf("release event: %q", d)
		}
	})
	t.Run("unconfirmed", func(t *testing.T) {
		noSystemd(t)
		fakeDevctl(t, handCutDevctl)
		stubGitHub(t, github.Merged, "")
		stubBaseRelease(t, github.BaseRelease{Base: mainBranch, Auto: true}, nil)
		stubReleaseWait(t, `{"verdict":"timeout","reason":"no tag within 30m"}`)
		g := runningMerge(t, scratchRepo, localLane)
		g.cfg.Merge.Settle.Duration = time.Hour
		var err error
		_ = gateStderr(t, func() { err = startMerge(t, g) })
		if Code(err) != devctlUnconfirmed {
			t.Fatalf("exit %d, want %d", Code(err), devctlUnconfirmed)
		}
		st := gateState(t, g)
		if len(st.Merges) != 1 || st.Merges[0].Phase != state.Settling || st.Merges[0].Releasing {
			t.Fatalf("want the merge settling by the settle rule: %+v", st.Merges)
		}
		if why := g.lanePending(); !strings.Contains(why, "release is unknown") {
			t.Errorf("the lane does not settle: %q", why)
		}
	})
}

// A lane with an installation keeps devctl's own release wait: its merge
// settles until the release rolled, nothing overlaps.
func TestALaneWithAnInstallationDoesNotOverlap(t *testing.T) {
	g := runningMerge(t, scratchRepo, config.Lane{Name: serving, Installation: gazelle, Repositories: []string{scratchRepo}})
	if g.overlapLane() || g.overlaps() {
		t.Error("an installation's lane overlaps")
	}
	argv, err := g.mergeArgv()
	if err != nil || strings.Contains(strings.Join(argv, " "), "--no-release-wait") {
		t.Errorf("argv %q, %v: want devctl's release wait", argv, err)
	}
}

// nextBesideARelease is the running merge o/r#8 of localLane, its turn come
// while o/r#7's release is being confirmed in a live gate (this process),
// with its warm-up stubbed: warmed is called when the warm-up's CI wait
// runs, the state polled every few milliseconds.
func nextBesideARelease(t *testing.T, warmed func(g *gateRun)) *gateRun {
	t.Helper()
	stubGitHub(t, github.Open, "")
	g := runningMerge(t, scratchRepo, localLane)
	g.pr, g.argv = 8, append(mergeArgv(scratchRepo), "--update-branch")
	g.cfg.Merge.CITimeout.Duration = time.Minute
	_ = g.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = []state.Merge{
			{Repo: scratchRepo, PR: 7, Lane: localLane.Name, PID: os.Getpid(), Phase: state.Settling, Releasing: true, Finished: time.Now()},
			{Repo: scratchRepo, PR: 8, Lane: localLane.Name, By: g.me, PID: os.Getpid(), Phase: state.Running, Started: time.Now()},
		}
		return nil, nil
	})
	wasPoll, wasUpdate, wasWait := releasesPoll, updateBranch, prWarmWait
	t.Cleanup(func() { releasesPoll, updateBranch, prWarmWait = wasPoll, wasUpdate, wasWait })
	releasesPoll = 10 * time.Millisecond
	updateBranch = func(context.Context, string, int) error { return nil }
	prWarmWait = func(context.Context, string, int) { warmed(g) }
	return g
}

// releaseKnown takes o/r#7's releasing place out of the lane, as its gate
// does once the release is confirmed.
func releaseKnown(g *gateRun) {
	_ = g.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = slices.DeleteFunc(st.Merges, func(m state.Merge) bool { return m.PR == 7 })
		return nil, nil
	})
}

// The next merge, its turn come beside a release being confirmed, updates
// its branch and waits for its CI meanwhile, and goes on to merge only once
// that release is known.
func TestTheNextMergeWarmsUpBesideTheReleaseAndMergesAfterIt(t *testing.T) {
	var known, warmed atomic.Bool
	g := nextBesideARelease(t, func(g *gateRun) {
		warmed.Store(!known.Load())
		go func() {
			time.Sleep(50 * time.Millisecond)
			known.Store(true)
			releaseKnown(g)
		}()
	})
	var err error
	out := gateStderr(t, func() { err = g.awaitReleases() })
	if err != nil {
		t.Fatalf("exit %d (%v), want the merge to go on", Code(err), err)
	}
	if !known.Load() || !warmed.Load() {
		t.Errorf("release known %v, CI waited beside it %v:\n%s", known.Load(), warmed.Load(), out)
	}
	if !strings.Contains(out, "o/r#7's release is being confirmed; o/r#8 updates its branch and waits for its CI meanwhile") ||
		!strings.Contains(out, "reviews: nothing holds o/r#8") {
		t.Errorf("gate lines:\n%s", out)
	}
}

// A stop that reaches the merge while it waits for its lane's release ends
// the wait, nothing merged, its place out of the lane; a hangup or the
// caller's timeout cuts only the call, and the wait goes on.
func TestAStopDuringTheReleaseWaitEndsTheMergeUnmerged(t *testing.T) {
	sig := make(chan os.Signal, 2)
	g := nextBesideARelease(t, func(*gateRun) {})
	g.sig = sig
	was := timedCaller
	t.Cleanup(func() { timedCaller = was })
	timedCaller = func() bool { return false }
	sig <- syscall.SIGHUP
	sig <- syscall.SIGINT
	var err error
	out := gateStderr(t, func() { err = g.awaitReleases() })
	if Code(err) != 130 {
		t.Fatalf("exit %d (%v), want 130\n%s", Code(err), err, out)
	}
	if !strings.Contains(out, "hangup: the caller is going away; o/r#8 waits on") || !strings.Contains(out, "nothing merged, it left the lane") {
		t.Errorf("gate lines:\n%s", out)
	}
	st := gateState(t, g)
	if len(st.Merges) != 1 || st.Merges[0].PR != 7 {
		t.Errorf("want only o/r#7's release in the lane: %+v", st.Merges)
	}
	if d := lastEvent(t, g, "merge.stopped"); !strings.Contains(d, "o/r#8 in lane o/r: interrupt while it waited") {
		t.Errorf("stopped event: %q", d)
	}
	t.Run("context", func(t *testing.T) {
		g := nextBesideARelease(t, func(*gateRun) {})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		g.ctx = ctx
		var err error
		_ = gateStderr(t, func() { err = g.awaitReleases() })
		if Code(err) != 143 {
			t.Fatalf("exit %d (%v), want 143", Code(err), err)
		}
	})
	t.Run("taken out of the lane", func(t *testing.T) {
		g := nextBesideARelease(t, func(g *gateRun) {
			_ = g.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Merges = slices.DeleteFunc(st.Merges, func(m state.Merge) bool { return m.PR == 8 })
				return nil, nil
			})
		})
		var err error
		out := gateStderr(t, func() { err = g.awaitReleases() })
		if Code(err) != ExitGateRefused || !strings.Contains(out, "taken out of lane o/r") {
			t.Fatalf("exit %d, want a refusal:\n%s", Code(err), out)
		}
	})
}

// A hold set while the merge waits for its lane's release that lets another
// pull request through first puts the merge back to waiting in its place,
// before its devctl starts.
func TestAHoldDuringTheReleaseWaitPutsTheMergeBackToWaiting(t *testing.T) {
	g := nextBesideARelease(t, func(g *gateRun) {
		_ = g.store.Update(func(st *state.State) ([]state.Event, error) {
			st.Holds = append(st.Holds, state.Hold{Target: merge.LanePrefix + localLane.Name, Except: scratchRepo + "#9", By: state.Party{Name: supervisorName},
				At: time.Now().UTC(), Reason: "the fix first"})
			return nil, nil
		})
	})
	var err error
	_ = gateStderr(t, func() { err = g.awaitReleases() })
	if p := (*preemptedError)(nil); !errors.As(err, &p) {
		t.Fatalf("%v, want the merge preempted", err)
	}
	st := gateState(t, g)
	if i := slices.IndexFunc(st.Merges, func(m state.Merge) bool { return m.PR == 8 }); i < 0 || st.Merges[i].Phase != state.Waiting {
		t.Errorf("want o/r#8 waiting in its place: %+v", st.Merges)
	}
}

// A release whose verdict stays unknown past the wait's bound refuses the
// merge, its place out of the lane: it never merges ahead of a verdict.
func TestAReleaseUnknownPastTheWaitRefusesTheMerge(t *testing.T) {
	g := nextBesideARelease(t, func(*gateRun) {})
	was := releasesWait
	t.Cleanup(func() { releasesWait = was })
	releasesWait = func(time.Duration) time.Duration { return 30 * time.Millisecond }
	var err error
	out := gateStderr(t, func() { err = g.awaitReleases() })
	if Code(err) != ExitGateRefused || !strings.Contains(out, "still waits for a release's verdict") {
		t.Fatalf("exit %d, want a refusal:\n%s", Code(err), out)
	}
	if st := gateState(t, g); slices.ContainsFunc(st.Merges, func(m state.Merge) bool { return m.PR == 8 }) {
		t.Errorf("o/r#8 kept its place: %+v", st.Merges)
	}
}

// beekeeper gate stop reaches a running merge whose devctl has not started,
// its gate waiting for its lane's release: it interrupts that gate and says
// so, never that its devctl had ended.
func TestGateStopInterruptsAMergeWaitingForItsLanesRelease(t *testing.T) {
	gate := exec.Command("sleep", "30")
	if err := gate.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- gate.Wait() }()
	t.Cleanup(func() { _ = gate.Process.Kill() })
	stubGitHub(t, github.Open, "")
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	me := state.Party{Name: workerRole}
	_ = store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = []state.Merge{{Repo: scratchRepo, PR: 8, Lane: localLane.Name, By: me, PID: gate.Process.Pid, Phase: state.Running, Started: time.Now()}}
		return nil, nil
	})
	said, err := stopMerges(store, me, func(state.Merge) bool { return true }, false, func(state.Merge) string { return "TaskStop" })
	if err != nil || len(said) != 1 || !strings.Contains(said[0], "o/r#8 cancelled while it waited for lane o/r's release, before its devctl started") {
		t.Fatalf("%q, %v", said, err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Error("the waiting gate ended without the interrupt")
		}
	case <-time.After(5 * time.Second):
		t.Error("the waiting gate was not interrupted")
	}
}

// A merge whose lane confirms no release goes on at once, no warm-up.
func TestTheNextMergeOfAFreeLaneDoesNotWarmUp(t *testing.T) {
	g := runningMerge(t, scratchRepo, localLane)
	was := prWarmWait
	t.Cleanup(func() { prWarmWait = was })
	called := false
	prWarmWait = func(context.Context, string, int) { called = true }
	if err := g.awaitReleases(); err != nil {
		t.Fatalf("exit %d, want the merge to go on", Code(err))
	}
	if called {
		t.Error("a free lane's merge waited for its CI twice")
	}
}

// The lane's pace is the median of its latest places, start to merge, on
// to the settle where its lane rolled; drain is that times its places.
func TestLanePaceIsTheMedianOfItsLatestPlaces(t *testing.T) {
	at := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	ev := func(min int, verb, detail string) state.Event {
		return state.Event{At: at.Add(time.Duration(min) * time.Minute), Verb: verb, Detail: detail}
	}
	events := []state.Event{
		ev(0, verbMerging, "o/r#1 in lane o/r"),
		ev(10, verbMerged, "o/r#1 exit 0, release v1"),
		ev(10, verbMerging, "o/r#2 in lane o/r"),
		ev(22, verbMerged, "o/r#2 exit 0, release v2"),
		ev(30, verbLaneSettled, "o/r: o/r#2 rolled"),
		ev(30, verbMerging, "o/x#3 in lane other"),
		ev(31, verbMerged, "o/x#3 exit 0, release v1"),
		ev(31, verbMerging, "o/r#4 in lane o/r"),
		ev(46, verbMerged, "o/r#4 exit 0, release v4"),
	}
	pace, ok := lanePace(events, "o/r")
	if !ok || pace != 15*time.Minute {
		t.Errorf("pace %s, %v; want 15m (10m, 20m, 15m)", pace, ok)
	}
	if _, ok := lanePace(events, "none"); ok {
		t.Error("a lane without merges has a pace")
	}
}
