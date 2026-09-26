//go:build unix

package cmd

import (
	"context"
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
	"github.com/giantswarm/beekeeper/internal/guard"
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

// runningMerge is a gate run of repo#pr in its lane that devctl's turn has
// come for, with the devctl release window open when repo is devctl's.
func runningMerge(t *testing.T, repo string, lane config.Lane) *gateRun {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Lanes: []config.Lane{lane}, Merge: config.Merge{SeedTTL: config.Duration{Duration: time.Hour}}}
	me := state.Party{Name: "worker"}
	g := &gateRun{app: &app{cfg: cfg, store: store, now: relayNow}, ctx: context.Background(), repo: repo, pr: 7, lane: lane, me: me, pid: os.Getpid(),
		argv: []string{"devctl", "pr", "merge", repo, "7"}}
	err = store.Update(func(st *state.State) ([]state.Event, error) {
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
			if systemd && !guard.UserSystemd() {
				t.Skip("no user service manager")
			}
			was := userSystemd
			userSystemd = func() bool { return systemd }
			t.Cleanup(func() { userSystemd = was })
			where := filepath.Join(t.TempDir(), "where")
			fakeDevctl(t, `echo merging >&2; { ps -o sid= -p $$; echo $$; cat /proc/$$/cgroup; } >`+where+`; sleep 1; echo "waiting for the release" >&2; echo '`+mergedDoc+`'`)
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
	gazelleLane := config.Lane{Name: "serving", Installation: "gazelle", Repositories: []string{scratchRepo}}
	toolLane := config.Lane{Name: merge.ToolRepo, Repositories: []string{merge.ToolRepo}}
	for _, c := range []struct {
		name, repo, pull string
		lane             config.Lane
		check            func(t *testing.T, g *gateRun, st *state.State)
	}{
		{"merged, a lane to roll", scratchRepo, github.Merged, gazelleLane, func(t *testing.T, g *gateRun, st *state.State) {
			if len(st.Merges) != 1 || st.Merges[0].Phase != state.Settling || st.Merges[0].Retrying() {
				t.Errorf("want one settling merge, no retry place: %+v", st.Merges)
			}
			if d := lastEvent(t, g, "merged"); !strings.Contains(d, "exit 143, release unconfirmed (merged per GitHub)") {
				t.Errorf("merged event: %q", d)
			}
			if lastEvent(t, g, "merge.failed") != "" {
				t.Error("logged merge.failed")
			}
		}},
		{"unmerged keeps the retry place", scratchRepo, github.Open, gazelleLane, func(t *testing.T, g *gateRun, st *state.State) {
			if len(st.Merges) != 1 || !st.Merges[0].Retrying() {
				t.Errorf("want the retry place: %+v", st.Merges)
			}
			if d := lastEvent(t, g, "merge.failed"); !strings.Contains(d, "exit 143, nothing merged") {
				t.Errorf("merge.failed event: %q", d)
			}
		}},
		{"merged devctl keeps its window for the update", merge.ToolRepo, github.Merged, toolLane, func(t *testing.T, _ *gateRun, st *state.State) {
			if len(st.Merges) != 0 {
				t.Errorf("merges left: %+v", st.Merges)
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
		{"devctl updated", github.Merged, "v8.1.0", true, false},
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
