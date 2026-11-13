//go:build unix

package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/gate"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestQueuedFollowerOnlyReadsItsPlace(t *testing.T) {
	for _, tc := range []struct {
		phase   string
		handoff bool
	}{{state.Waiting, false}, {state.Running, false}, {state.Waiting, true}, {state.Running, true}} {
		t.Run(fmt.Sprintf("%s/handoff=%v", tc.phase, tc.handoff), func(t *testing.T) {
			asked := stubGitHub(t, github.Open, "")
			a := busyLaneApp(t)
			pid := os.Getpid()
			if tc.handoff {
				pid = sleeper(t)
			}
			err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Merges[0].Phase = tc.phase
				st.Merges = append(st.Merges, state.Merge{Repo: scratchRepo, PR: 7, Lane: scratchRepo, PID: pid, Phase: state.Waiting, Joined: time.Now(), Seen: time.Now()})
				// Either of these would make the old housekeeping read GitHub,
				// even though another live gate holds this follower's lane.
				st.Holds = append(st.Holds, state.Hold{Tool: merge.Tool, ToolPR: 123, Target: "o/other"})
				st.Merges = append(st.Merges, state.Merge{Repo: "o/other", PR: 123, Lane: "unrelated-lane", Phase: state.Waiting, Outside: true, Seen: time.Now()})
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			g := &gate.GateRun{Host: a, Ctx: context.Background(), Argv: mergeArgv(scratchRepo), Repo: scratchRepo, Pr: 7, Lane: a.cfg.LaneOf(scratchRepo), Pid: os.Getpid(), Placed: true}
			if tc.handoff {
				g.From, g.Queued = pid, true
				if why, err := g.Step(); err != nil || why == "" || *asked != 0 {
					t.Fatalf("handoff read GitHub or failed: %q, %v, reads %d", why, err, *asked)
				}
			}
			path := filepath.Join(a.store.Dir(), "state.json")
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			for range 3 {
				if why, err := g.Step(); err != nil || why == "" || !g.Passive {
					t.Fatalf("follower: why %q, err %v, passive %v", why, err, g.Passive)
				}
			}
			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if *asked != 0 || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
				t.Fatalf("follower made %d GitHub reads or rewrote state", *asked)
			}
		})
	}
}

func TestQueuedFollowerResumesNormalChecksAtItsTurn(t *testing.T) {
	stubGitHub(t, github.Open, "")
	a := busyLaneApp(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = append(st.Merges, state.Merge{Repo: scratchRepo, PR: 7, Lane: scratchRepo, PID: os.Getpid(), Phase: state.Waiting, Joined: time.Now(), Seen: time.Now()})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	g := &gate.GateRun{Host: a, Ctx: context.Background(), Argv: mergeArgv(scratchRepo), Repo: scratchRepo, Pr: 7, Lane: a.cfg.LaneOf(scratchRepo), Pid: os.Getpid(), Placed: true}
	if why, err := g.Step(); err != nil || why == "" || !g.Passive {
		t.Fatalf("not waiting behind predecessor: %q, %v", why, err)
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = st.Merges[1:]
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	was := readBudget
	t.Cleanup(func() { readBudget = was })
	reads := 0
	readBudget = func(*app, context.Context) (github.Budget, error) {
		reads++
		return github.Budget{}, fmt.Errorf("no budget in this test")
	}
	if _, err := g.Step(); Code(err) != gate.ExitGateRefused || g.Passive || reads != 1 {
		t.Fatalf("head skipped normal checks: %v, passive %v, budget reads %d", err, g.Passive, reads)
	}
}

func TestQueueBackoffIsBoundedAndResetsAtTheHead(t *testing.T) {
	pause := time.Duration(0)
	for _, want := range []time.Duration{5, 10, 20, 40, 60, 60} {
		pause = gateQueuePause(pause, true)
		if pause != want*time.Second {
			t.Fatalf("pause %s, want %s", pause, want*time.Second)
		}
	}
	if pause = gateQueuePause(pause, false); pause != 5*time.Second {
		t.Fatalf("head pause %s", pause)
	}
}

func TestQueuedFollowerCancellationDoesNotWaitForItsSleep(t *testing.T) {
	asked := stubGitHub(t, github.Open, "")
	a := busyLaneApp(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = append(st.Merges, state.Merge{Repo: scratchRepo, PR: 7, Lane: scratchRepo, PID: os.Getpid(), Phase: state.Waiting, Joined: time.Now(), Seen: time.Now()})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := a.gate(ctx, mergeArgv(scratchRepo), time.Hour, 0, true)
	if err != context.DeadlineExceeded || time.Since(started) > time.Second || *asked != 0 {
		t.Fatalf("cancellation: %v after %s, %d GitHub reads", err, time.Since(started), *asked)
	}
}

func TestPassiveHandoffKeepsAnExpiredPlaceAfterItsParentExits(t *testing.T) {
	t.Setenv(gate.GateFromEnv, "")
	for _, seeded := range []bool{false, true} {
		t.Run(fmt.Sprintf("seeded=%v", seeded), func(t *testing.T) {
			asked := stubGitHub(t, github.Open, "")
			_, launched := stubSelf(t)
			parent := deadPID(t)
			a := busyLaneApp(t)
			joined := time.Now().Add(-2 * time.Hour)
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Merges[0].Joined = joined.Add(-time.Hour)
				st.Merges = append(st.Merges, state.Merge{Repo: scratchRepo, PR: 7, Lane: scratchRepo, PID: parent,
					Phase: state.Waiting, Joined: joined, Seen: joined, Seeded: seeded})
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			g := &gate.GateRun{Host: a, Ctx: context.Background(), Argv: mergeArgv(scratchRepo), Repo: scratchRepo, Pr: 7,
				Lane: a.cfg.LaneOf(scratchRepo), Pid: parent, Placed: true}
			before, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			merge.Prune(before, time.Now(), a.cfg.Merge.QueueTTL.Duration, a.cfg.Merge.SeedTTL.Duration, proc.Alive)
			if g.Mine(before, state.Waiting) >= 0 {
				t.Fatal("fixture does not reproduce an expired place whose parent is gone")
			}
			handoff := time.Now()
			if err := g.Enqueue("a long passive wait"); Code(err) != gate.ExitGateQueued {
				t.Fatalf("handoff: %v", err)
			}
			launched()
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			seen := st.Merges[g.Mine(st, state.Waiting)].Seen
			g.Pid, g.From, g.Queued = os.Getpid(), parent, true
			if why, err := g.Step(); err != nil || why == "" || !g.Passive {
				t.Fatalf("expired handoff was lost: why %q, err %v", why, err)
			}
			st, err = a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			m := st.Merges[g.Mine(st, state.Waiting)]
			if seen.Before(handoff) || !m.Joined.Equal(joined) || m.PID != g.Pid || m.Seeded != seeded || *asked != 0 {
				t.Fatalf("handoff changed order, missed its refresh or read GitHub: %+v, seen %s, reads %d", m, seen, *asked)
			}
		})
	}
}

func TestQueuedFollowerReloadsLanesOnPoll(t *testing.T) {
	a := busyLaneApp(t)
	a.cfg.Lanes[0].Name = litShared
	a.loadedCfg = filepath.Join(t.TempDir(), "config.yaml")
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges[0].Lane = litShared
		st.Merges = append(st.Merges, state.Merge{Repo: scratchRepo, PR: 7, Lane: "shared", PID: os.Getpid(), Phase: state.Waiting, Joined: time.Now(), Seen: time.Now()})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	g := &gate.GateRun{Host: a, Ctx: context.Background(), Repo: scratchRepo, Pr: 7, Lane: a.cfg.LaneOf(scratchRepo), Pid: os.Getpid(), Placed: true, Queued: true}
	for _, cap := range []int{5, 12} {
		raw := fmt.Sprintf("merge:\n  cap: %d\nlanes:\n  - name: %s\n    repositories: [%s, o/other]\n", cap, "shared", scratchRepo)
		if err := os.WriteFile(a.loadedCfg, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if why, err := g.Step(); err != nil || why == "" || !g.Passive {
			t.Fatalf("poll: %q, %v", why, err)
		}
		if gateApp(g.Host).cfg.Merge.Cap != cap || len(g.Lane.Repositories) != 2 {
			t.Fatalf("stale settings: cap %d, lane %+v", gateApp(g.Host).cfg.Merge.Cap, g.Lane)
		}
	}
}
