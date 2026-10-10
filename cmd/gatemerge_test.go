//go:build unix

package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
)

const mergedDoc = `{"mergeCommitSha":"abc","release":{"verdict":"available","tag":"v1.2.4"}}`

// fakeDevctl puts a devctl on PATH that runs script.
func fakeDevctl(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "devctl"), []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// labRepo is a repository devctl does not serve.
const labRepo = "teemow/lab"

// devctlTo is the release a devctl window's merge produced.
const devctlTo = "v8.1.0"

// mergeArgv is devctl pr merge repo 7 with extra flags.
func mergeArgv(repo string, extra ...string) []string {
	return append([]string{merge.Tool, "pr", "merge", repo, "7"}, extra...)
}

// runningMerge is a gate run of repo#pr in its lane that devctl's turn has
// come for, with the devctl release window open when repo is devctl's.
func runningMerge(t *testing.T, repo string, lane config.Lane) *gateRun {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return runningMergeIn(t, store, repo, lane)
}

// runningMergeIn is runningMerge in store.
func runningMergeIn(t *testing.T, store *state.FileStore, repo string, lane config.Lane) *gateRun {
	t.Helper()
	cfg := &config.Config{Lanes: []config.Lane{lane}, Merge: config.Merge{SeedTTL: config.Duration{Duration: time.Hour}, DevctlOwners: []string{"o", "giantswarm"}}}
	me := state.Party{Name: "worker"}
	g := &gateRun{app: &app{cfg: cfg, store: store, now: relayNow}, ctx: context.Background(), repo: repo, pr: 7, lane: lane, me: me, pid: os.Getpid(),
		argv: mergeArgv(repo)}
	err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = []state.Merge{{Repo: repo, PR: 7, Lane: lane.Name, By: me, PID: g.pid, Phase: state.Running, Joined: relayNow, Started: relayNow}}
		if repo == merge.ToolRepo {
			st.Holds = []state.Hold{{Target: merge.AllMerges, Except: repo, By: me, Tool: merge.Tool, ToolFrom: devctlFrom, ToolPR: 7}}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func gateState(t *testing.T, g *gateRun) *state.State {
	t.Helper()
	st, err := g.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func lastEvent(t *testing.T, g *gateRun, verb string) string {
	t.Helper()
	evs, err := g.store.Events(0, func(e state.Event) bool { return e.Verb == verb })
	if err != nil || len(evs) == 0 {
		return ""
	}
	return evs[len(evs)-1].Detail
}

// ancestors are pid's parents up to init.
func ancestors(pid int) []int {
	var up []int
	for pid > 1 {
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			break
		}
		// pid (comm) state ppid ...
		f := strings.Fields(string(raw[strings.LastIndexByte(string(raw), ')')+1:]))
		if len(f) < 2 {
			break
		}
		pid, _ = strconv.Atoi(f[1])
		up = append(up, pid)
	}
	return up
}

func TestAGatedMergeOutlivesItsCaller(t *testing.T) {
	for _, systemd := range []bool{false, true} {
		t.Run(fmt.Sprintf("user systemd %v", systemd), func(t *testing.T) {
			if systemd && !plat.Launcher.Available() {
				t.Skip("no user service manager")
			}
			was := userSystemd
			userSystemd = func() bool { return systemd }
			t.Cleanup(func() { userSystemd = was })
			where := filepath.Join(t.TempDir(), "where")
			fakeDevctl(t, `echo merging >&2; { cut -d" " -f6 /proc/$$/stat; echo $$; cat /proc/$$/cgroup; } >`+where+`; sleep 1; echo "waiting for the release" >&2; echo '`+mergedDoc+`'`)
			stubGitHub(t, "", "")
			lane := config.Lane{Name: scratchRepo, Repositories: []string{scratchRepo}}
			g := runningMerge(t, scratchRepo, lane)
			// The caller's stdout pipe is gone with its session.
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			_ = r.Close()
			stdout := os.Stdout
			os.Stdout = w
			t.Cleanup(func() { os.Stdout = stdout; _ = w.Close() })
			go func() {
				time.Sleep(600 * time.Millisecond)
				// The caller's session ends: its process group gets SIGHUP and SIGTERM.
				_ = syscall.Kill(os.Getpid(), syscall.SIGHUP)
				_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
			}()
			if err := g.runMerge(); err != nil {
				t.Fatalf("the merge did not finish: %v", err)
			}
			raw, err := os.ReadFile(where) //nolint:gosec // the test's file
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.SplitN(string(raw), "\n", 3)
			sid, _ := strconv.Atoi(strings.TrimSpace(lines[0]))
			if mine, _ := unix.Getsid(0); sid == mine {
				t.Errorf("devctl ran in its caller's session %d", sid)
			}
			if systemd {
				// A harness kills its command's process tree, a unit its cgroup.
				pid, _ := strconv.Atoi(strings.TrimSpace(lines[1]))
				if slices.Contains(ancestors(pid), os.Getpid()) {
					t.Errorf("devctl (pid %d) is a descendant of its caller: %v", pid, ancestors(pid))
				}
				mine, _ := os.ReadFile("/proc/self/cgroup")
				if strings.TrimSpace(lines[2]) == strings.TrimSpace(string(mine)) {
					t.Errorf("devctl ran in its caller's cgroup %s", mine)
				}
			}
			if d := lastEvent(t, g, "merged"); !strings.Contains(d, "o/r#7 exit 0, release v1.2.4") {
				t.Errorf("merged event: %q", d)
			}
			if st := gateState(t, g); len(st.Merges) != 0 {
				t.Errorf("merges left: %+v", st.Merges)
			}
		})
	}
}

func TestAKilledMergeChildIsJudgedByGitHub(t *testing.T) {
	noSystemd(t)
	pidFile := filepath.Join(t.TempDir(), "runner")
	// devctl kills the merge's runner (its parent), as a person or an OOM would.
	fakeDevctl(t, `echo $PPID >`+pidFile+`; kill -KILL $PPID; sleep 1`)
	stubGitHub(t, github.Merged, "")
	g := runningMerge(t, scratchRepo, config.Lane{Name: scratchRepo, Repositories: []string{scratchRepo}})
	if err := g.runMerge(); Code(err) != 137 {
		t.Fatalf("exit %d, want 137", Code(err))
	}
	if d := lastEvent(t, g, "merged"); !strings.Contains(d, "exit 137, release unconfirmed") {
		t.Errorf("merged event: %q", d)
	}
}

// noSystemd runs merge-child in a session of its own, as without a user
// service manager.
func noSystemd(t *testing.T) {
	t.Helper()
	was := userSystemd
	userSystemd = func() bool { return false }
	t.Cleanup(func() { userSystemd = was })
}

func TestASignalExitWithoutADocumentIsJudgedByGitHub(t *testing.T) {
	gazelleLane := config.Lane{Name: serving, Installation: gazelle, Repositories: []string{scratchRepo}}
	toolLane := config.Lane{Name: merge.ToolRepo, Repositories: []string{merge.ToolRepo}}
	for _, c := range []struct {
		name, repo, pull string
		lane             config.Lane
		check            func(t *testing.T, g *gateRun, st *state.State)
	}{
		{"merged, a lane to roll", scratchRepo, github.Merged, gazelleLane, func(t *testing.T, g *gateRun, st *state.State) {
			if len(st.Merges) != 1 || st.Merges[0].Phase != state.Settling {
				t.Errorf("want one settling merge: %+v", st.Merges)
			}
			if d := lastEvent(t, g, "merged"); !strings.Contains(d, "exit 143, release unconfirmed (merged per GitHub)") {
				t.Errorf("merged event: %q", d)
			}
			if lastEvent(t, g, "merge.failed") != "" {
				t.Error("logged merge.failed")
			}
		}},
		{"unmerged leaves the lane", scratchRepo, github.Open, gazelleLane, func(t *testing.T, g *gateRun, st *state.State) {
			if len(st.Merges) != 0 {
				t.Errorf("a run with nothing merged keeps a place: %+v", st.Merges)
			}
			if d := lastEvent(t, g, "merge.failed"); !strings.Contains(d, "exit 143, nothing merged, it left lane "+serving) {
				t.Errorf("merge.failed event: %q", d)
			}
		}},
		{"merged devctl keeps its window for the update", merge.ToolRepo, github.Merged, toolLane, func(t *testing.T, _ *gateRun, st *state.State) {
			// Its release unconfirmed, it settles its lane by the settle rule.
			if len(st.Merges) != 1 || st.Merges[0].Phase != state.Settling || st.Merges[0].Release != "" {
				t.Errorf("want one settling merge, its release unknown: %+v", st.Merges)
			}
			if len(st.Holds) != 1 || !st.Holds[0].ToolMerged {
				t.Errorf("want the window, merged: %+v", st.Holds)
			}
		}},
		{"unmerged devctl lifts its window", merge.ToolRepo, github.Open, toolLane, func(t *testing.T, g *gateRun, st *state.State) {
			if len(st.Holds) != 0 {
				t.Errorf("window left: %+v", st.Holds)
			}
			if d := lastEvent(t, g, "hold.lift"); !strings.Contains(d, "merged nothing") {
				t.Errorf("hold.lift event: %q", d)
			}
		}},
		{"GitHub unanswered settles by the rule", scratchRepo, "", gazelleLane, func(t *testing.T, g *gateRun, st *state.State) {
			if len(st.Merges) != 1 || st.Merges[0].Phase != state.Settling || st.Merges[0].Exit != 143 {
				t.Errorf("want one settling merge: %+v", st.Merges)
			}
			if d := lastEvent(t, g, "merge.unknown"); !strings.Contains(d, "no network") {
				t.Errorf("merge.unknown event: %q", d)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			noSystemd(t)
			fakeDevctl(t, `echo merged >&2; kill -TERM $$`)
			asked := stubGitHub(t, c.pull, devctlFrom)
			g := runningMerge(t, c.repo, c.lane)
			if err := g.runMerge(); Code(err) != 143 {
				t.Fatalf("exit %d, want 143", Code(err))
			}
			if *asked == 0 {
				t.Error("GitHub not asked")
			}
			c.check(t, g, gateState(t, g))
		})
	}
}

func TestADeadMergesToolWindowCloses(t *testing.T) {
	for _, c := range []struct {
		name, pull, version string
		lifted, merged      bool
	}{
		{"not merged", github.Open, devctlFrom, true, false},
		{"closed", github.Closed, "", true, false},
		{"merged, devctl not updated", github.Merged, devctlFrom, false, true},
		{"devctl updated", github.Merged, devctlTo, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			stubGitHub(t, c.pull, c.version)
			g := runningMerge(t, merge.ToolRepo, config.Lane{Name: merge.ToolRepo})
			// Its gate and devctl are gone.
			err := g.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Merges[0].PID, st.Merges[0].Child = 0, 0
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			g.closeToolWindow(context.Background(), watchParty)
			st := gateState(t, g)
			if lifted := len(st.Holds) == 0; lifted != c.lifted {
				t.Fatalf("lifted %v, want %v: %+v", lifted, c.lifted, st.Holds)
			}
			if !c.lifted && st.Holds[0].ToolMerged != c.merged {
				t.Errorf("ToolMerged %v, want %v", st.Holds[0].ToolMerged, c.merged)
			}
		})
	}
	// A live devctl keeps the window, whatever GitHub says.
	asked := stubGitHub(t, github.Open, devctlFrom)
	g := runningMerge(t, merge.ToolRepo, config.Lane{Name: merge.ToolRepo})
	_ = g.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges[0].PID, st.Merges[0].Child = 0, os.Getpid()
		return nil, nil
	})
	g.closeToolWindow(context.Background(), watchParty)
	if st := gateState(t, g); len(st.Holds) != 1 || *asked != 0 {
		t.Errorf("a running devctl's window: %+v, GitHub asked %d times", st.Holds, *asked)
	}
}

// A merge-child unit fails only where a person must act; devctl's verdicts,
// the gate's and a stop as asked are its caller's and end the unit
// successfully.
func TestMergeChildUnitFailsOnlyForAPerson(t *testing.T) {
	for rc, want := range map[int]int{
		0: 0, 1: 0, 2: 0, 3: 0, 4: 0, 5: 0, 6: 0, 7: 0, 9: 0, ExitGateQueued: 0, ExitGateRefused: 0, 143: 0,
		8: 8, 75: 75, 127: 127, 130: 130, 137: 137,
	} {
		if got := unitExit(rc); got != want {
			t.Errorf("devctl exit %d: unit exit %d, want %d", rc, got, want)
		}
	}
}

// The plain squash merge route carries the caller's --timeout, in either
// spelling, and nothing else of devctl's flags.
func TestSquashArgv(t *testing.T) {
	for want, argv := range map[string][]string{
		"bk squash-merge teemow/lab 7":                mergeArgv(labRepo, "--progress"),
		"bk squash-merge teemow/lab 7 --timeout 10m":  mergeArgv(labRepo, "--timeout", "10m"),
		"bk squash-merge teemow/lab 7 --timeout 1h0m": mergeArgv(labRepo, "--timeout=1h0m"),
	} {
		if got := strings.Join(squashArgv("bk", labRepo, 7, argv), " "); got != want {
			t.Errorf("%v: %q, want %q", argv, got, want)
		}
	}
}

// A finished run's stderr and document are kept for its owner outside the
// run's own files; kept outputs past keptFor are pruned.
func TestKeepOutput(t *testing.T) {
	dir := t.TempDir()
	base := mergeBase(dir, labRepo, 7)
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base+".log", []byte("not found error\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, keptOutputs, "old.log")
	if err := os.MkdirAll(filepath.Dir(old), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(old, now.Add(-keptFor-time.Hour), now.Add(-keptFor-time.Hour)); err != nil {
		t.Fatal(err)
	}
	path := keepOutput(base, []byte(`{"exitCode":7}`), now)
	raw, err := os.ReadFile(path) //nolint:gosec // the test's file
	if err != nil || !strings.Contains(string(raw), "not found error") || !strings.Contains(string(raw), `"exitCode":7`) {
		t.Fatalf("kept %q: %q, %v", path, raw, err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("the old output is not pruned: %v", err)
	}
	removeMergeFiles(base)
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the kept output went with the run's files: %v", err)
	}
}

// A repository devctl does not serve takes the plain squash merge in the
// merge's child, not devctl, and the event names the route and the output.
func TestAnUnservedRepositoryTakesThePlainSquashMerge(t *testing.T) {
	userSystemd = func() bool { return false }
	t.Cleanup(func() { userSystemd = plat.Launcher.Available })
	fakeDevctl(t, `echo devctl ran >&2; exit 7`)
	dir := t.TempDir()
	args := filepath.Join(dir, "args")
	self := filepath.Join(dir, "beekeeper")
	// Stands in for this binary: merge-child runs the squash-merge it names.
	script := "#!/bin/sh\nif [ \"$1\" = merge-child ]; then exec " + os.Args[0] + " -test.run=TestHelperMergeChild -- \"$2\"; fi\n" +
		"echo \"$@\" >" + args + "; echo green >&2; echo '{\"mergeCommitSha\":\"abc\",\"release\":null}'\n"
	if err := os.WriteFile(self, []byte(script), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	was := selfExe
	selfExe = func() (string, error) { return self, nil }
	t.Cleanup(func() { selfExe = was })
	stubGitHub(t, "", "")
	g := runningMerge(t, labRepo, config.Lane{Name: labRepo})
	if err := g.runMerge(); err != nil {
		t.Fatalf("the plain squash merge: %v", err)
	}
	raw, _ := os.ReadFile(args) //nolint:gosec // the test's file
	if got := strings.TrimSpace(string(raw)); got != "squash-merge teemow/lab 7" {
		t.Errorf("ran %q", got)
	}
	d := lastEvent(t, g, "merged")
	if !strings.Contains(d, "teemow/lab#7 exit 0") || !strings.Contains(d, github.SquashRoute) || !strings.Contains(d, "output in ") {
		t.Errorf("merged event: %q", d)
	}
}

// TestHelperMergeChild is merge-child for the fake binary above.
func TestHelperMergeChild(t *testing.T) {
	i := slices.Index(os.Args, "--")
	if i < 0 {
		t.Skip("run by TestAnUnservedRepositoryTakesThePlainSquashMerge")
	}
	os.Exit(mergeChild(os.Args[i+1]).rc)
}

// A fix window lets its pull request through a lane whose installation
// cannot be read (or is not Ready) and logs it; without it the lane refuses.
func TestAFixWindowWaivesTheLanesReadiness(t *testing.T) {
	lane := config.Lane{Name: "portal-tools", Installation: "nowhere", Context: "no-such-context"}
	g := runningMerge(t, "o/r", lane)
	g.now = time.Now()
	if _, why, err := g.laneReady(merge.Lane{}); err == nil && why == "" {
		t.Fatal("an unreadable installation is ready without a fix window")
	}
	_ = g.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Holds = append(st.Holds, state.Hold{Target: merge.LanePrefix + lane.Name, Except: "o/r#7", By: state.Party{Name: "supervisor"}, Reason: "the outage fix"})
		return nil, nil
	})
	if _, why, err := g.laneReady(merge.Lane{}); err != nil || why != "" {
		t.Fatalf("the fix window's merge waits: %q, %v", why, err)
	}
	if d := lastEvent(t, g, "merge.window"); !strings.Contains(d, "o/r#7 in lane portal-tools") || !strings.Contains(d, "the outage fix") {
		t.Errorf("merge.window event: %q", d)
	}
}

// A merged window installs the release itself: the update runs once per
// toolUpdateEvery, a failed one is logged, and the window lifts once devctl
// reports the release.
func TestAMergedToolWindowUpdatesDevctl(t *testing.T) {
	stubGitHub(t, github.Merged, devctlFrom)
	version, updates, fail := devctlFrom, 0, error(nil)
	devctlVersion = func(context.Context) string { return version }
	devctlUpdate = func(context.Context) error {
		updates++
		if fail != nil {
			return fail
		}
		version = devctlTo
		return nil
	}
	g := runningMerge(t, merge.ToolRepo, config.Lane{Name: merge.ToolRepo})
	_ = g.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = nil
		st.Holds[0].ToolMerged, st.Holds[0].ToolRelease = true, devctlTo
		return nil, nil
	})

	fail = errors.New("no release asset yet")
	g.closeToolWindow(context.Background(), watchParty)
	g.closeToolWindow(context.Background(), watchParty)
	if st := gateState(t, g); len(st.Holds) != 1 || updates != 1 {
		t.Fatalf("after a failed update: %d updates, holds %+v", updates, st.Holds)
	}
	if d := lastEvent(t, g, "hold.update"); !strings.Contains(d, "no release asset yet") {
		t.Errorf("hold.update event: %q", d)
	}

	fail = nil
	_ = g.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Holds[0].ToolUpdated = time.Now().Add(-toolUpdateEvery)
		return nil, nil
	})
	g.closeToolWindow(context.Background(), watchParty)
	if st := gateState(t, g); len(st.Holds) != 0 || updates != 2 {
		t.Fatalf("after the update: %d updates, holds %+v", updates, st.Holds)
	}
	if d := lastEvent(t, g, "hold.lift"); !strings.Contains(d, "devctl now reports "+devctlTo) {
		t.Errorf("hold.lift event: %q", d)
	}
}

// A devctl merge that warrants no release lifts its window: no update is
// coming.
func TestANoReleaseToolMergeLiftsItsWindow(t *testing.T) {
	stubGitHub(t, github.Merged, devctlFrom)
	g := runningMerge(t, merge.ToolRepo, config.Lane{Name: merge.ToolRepo})
	_ = g.store.Update(func(st *state.State) ([]state.Event, error) {
		return recordRun(st, 0, g.lane, g.me, runOutcome{out: merge.Outcome{Merged: true, NoRelease: true}}, time.Now(), ""), nil
	})
	if st := gateState(t, g); len(st.Holds) != 0 {
		t.Fatalf("window left: %+v", st.Holds)
	}
	if d := lastEvent(t, g, "hold.lift"); !strings.Contains(d, "warranted no release") {
		t.Errorf("hold.lift event: %q", d)
	}
}

// A devctl merge queued behind a devctl release starts on that release: the
// gate runs devctl's update before it starts, and the merge waits while the
// window of the previous release is open, as devctl would otherwise run on
// the version that release replaced and refuse (exit 7).
func TestADevctlMergeWaitsForThePreviousRelease(t *testing.T) {
	stubGitHub(t, github.Merged, devctlFrom)
	updates := 0
	devctlUpdate = func(context.Context) error { updates++; return nil }
	g := runningMerge(t, merge.ToolRepo, config.Lane{Name: merge.ToolRepo})
	g.cfg.Merge.Cap = 10
	_ = g.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges[0].Phase, st.Merges[0].Started = state.Waiting, time.Time{}
		st.Holds[0].ToolMerged, st.Holds[0].ToolRelease, st.Holds[0].ToolPR = true, devctlTo, 6
		return nil, nil
	})
	why, err := g.start("", nil)
	if err != nil || !strings.Contains(why, "waiting for devctl to report "+devctlTo) || !strings.Contains(why, merge.ToolRepo+"#6") {
		t.Fatalf("start: %q, %v", why, err)
	}
	if updates != 1 {
		t.Errorf("%d updates before the start, want 1", updates)
	}
	st := gateState(t, g)
	if st.Merges[0].Phase != state.Waiting || len(st.Holds) != 1 || st.Holds[0].ToolPR != 6 {
		t.Errorf("the merge started or the window changed: %+v, %+v", st.Merges, st.Holds)
	}
}

// A merged window stays until devctl reports its release: another version
// than the window's opening one is not enough.
func TestAMergedToolWindowWaitsForItsRelease(t *testing.T) {
	stubGitHub(t, github.Merged, "v8.0.5")
	g := runningMerge(t, merge.ToolRepo, config.Lane{Name: merge.ToolRepo})
	_ = g.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = nil
		st.Holds[0].ToolMerged, st.Holds[0].ToolRelease = true, devctlTo
		return nil, nil
	})
	g.closeToolWindow(context.Background(), watchParty)
	if st := gateState(t, g); len(st.Holds) != 1 {
		t.Fatalf("lifted on v8.0.5 before %s: %+v", devctlTo, st.Holds)
	}
}

// The gate gives devctl pr merge merge.ciTimeout as its --timeout, which a
// CI restarted by --update-branch fits into; a --timeout the command names
// is the caller's and stays the only one.
func TestTheGateBoundsDevctlsCIWaitByItsOwnTimeout(t *testing.T) {
	for _, c := range []struct {
		name  string
		extra []string
		want  string
	}{
		{"none named", []string{"--update-branch"}, "--timeout 1h0m0s"},
		{"named", []string{"--timeout", "9m"}, "--timeout 9m"},
	} {
		t.Run(c.name, func(t *testing.T) {
			noSystemd(t)
			args := filepath.Join(t.TempDir(), "args")
			fakeDevctl(t, `echo "$*" >`+args+`; case "$*" in *"`+c.want+`"*) echo '`+mergedDoc+`' ;;
*) echo '{"verdict":"timeout","reason":"timeout after 30m0s"}'; exit 2 ;; esac`)
			stubGitHub(t, "", "")
			g := runningMerge(t, scratchRepo, config.Lane{Name: scratchRepo, Repositories: []string{scratchRepo}})
			g.cfg.Merge.CITimeout = config.Duration{Duration: time.Hour}
			g.argv = mergeArgv(scratchRepo, c.extra...)
			if err := g.runMerge(); err != nil {
				t.Fatalf("exit %d, want 0", Code(err))
			}
			raw, err := os.ReadFile(args) //nolint:gosec // the test's file
			if err != nil {
				t.Fatal(err)
			}
			if n := strings.Count(string(raw), "--timeout"); n != 1 {
				t.Errorf("devctl ran with %d --timeout: %s", n, raw)
			}
		})
	}
}
