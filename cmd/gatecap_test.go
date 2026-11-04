//go:build unix

package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The devctl subcommands the cap tests run.
const (
	prMerge  = "pr merge"
	promote  = "release promote"
	prWait   = "pr wait"
	relWait  = "release wait"
	rollWait = "rollout wait gazelle"
)

// capGate is the waiting merge o/r#7 of a queue app with merge.cap 5.
func capGate(t *testing.T) (*gateRun, *state.State, state.Merge) {
	t.Helper()
	me := state.Merge{Repo: scratchRepo, PR: 7, Lane: scratchRepo, By: state.Party{Session: "s1", Name: ownerName}, PID: os.Getpid(),
		Phase: state.Waiting, Joined: time.Now(), Seen: time.Now()}
	a := queueApp(t, me)
	a.cfg.Merge.Cap = 5
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	return &gateRun{app: a, ctx: context.Background(), repo: scratchRepo, pr: 7, lane: a.cfg.LaneOf(scratchRepo), me: me.By, pid: os.Getpid()}, st, me
}

// merge.cap counts the merges and promotes only: nine waits running leave a
// queued merge free to start under a cap of 5.
func TestMergeCapDoesNotCountWaits(t *testing.T) {
	g, st, me := capGate(t)
	hostDevctl(t, slices.Concat(devctlArgv(3, prWait), devctlArgv(3, relWait), devctlArgv(3, rollWait))...)
	if why := g.capWait(st, me); why != "" {
		t.Errorf("cap wait %q with nine waits and no merge running, want none", why)
	}
}

// Five merges and promotes still fill a cap of 5, the waits beside them
// counted for nothing.
func TestMergeCapCountsMergesAndPromotes(t *testing.T) {
	g, st, me := capGate(t)
	hostDevctl(t, slices.Concat(devctlArgv(3, prMerge), devctlArgv(2, promote), devctlArgv(4, prWait))...)
	if why := g.capWait(st, me); !strings.Contains(why, "5 merges and promotes run machine-wide (cap 5)") {
		t.Errorf("cap wait %q, want the five merges and promotes to fill the cap", why)
	}
	// A promotion's dry run and a help call merge nothing.
	hostDevctl(t, slices.Concat(devctlArgv(4, prMerge), [][]string{
		append(devctlArgv(1, promote)[0], "--dry-run"), append(devctlArgv(1, prMerge)[0], "--help")})...)
	if why := g.capWait(st, me); why != "" {
		t.Errorf("cap wait %q with four merges, a dry run and a help call, want none", why)
	}
}

// A queued run sees an edited cap on its next poll, without a new binary.
func TestQueuedGateReloadsCap(t *testing.T) {
	g, st, me := capGate(t)
	g.queued = true
	g.loadedCfg = filepath.Join(t.TempDir(), "config.yaml")
	hostDevctl(t, devctlArgv(8, prMerge)...)
	for _, cap := range []int{5, 12, 5} {
		if err := os.WriteFile(g.loadedCfg, []byte(fmt.Sprintf("merge:\n  cap: %d\n", cap)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := g.reloadQueueConfig(); err != nil {
			t.Fatal(err)
		}
		why := g.capWait(st, me)
		if (why == "") != (cap == 12) {
			t.Fatalf("cap %d: wait %q", cap, why)
		}
	}
	if err := os.WriteFile(g.loadedCfg, []byte("merge: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := g.reloadQueueConfig(); err != nil {
		t.Fatal(err)
	}
	if g.cfg.Merge.Cap != 5 {
		t.Fatalf("invalid edit changed cap to %d", g.cfg.Merge.Cap)
	}
}

func TestQueuedGateStartsAfterCapRaise(t *testing.T) {
	noSystemd(t)
	fakeDevctl(t, "echo '"+mergedDoc+"'")
	stubGitHub(t, github.Open, "")
	stubBaseRelease(t, github.BaseRelease{Base: mainBranch, Auto: true}, nil)
	g, _, _ := capGate(t)
	g.argv, g.queued = mergeArgv(scratchRepo), true
	g.cfg.Merge.BudgetFresh = config.Duration{Duration: time.Hour}
	setBudget(t, g.app, 4000)
	hostDevctls(t, 8)
	g.loadedCfg = filepath.Join(t.TempDir(), "config.yaml")
	for _, cap := range []int{5, 12} {
		if err := os.WriteFile(g.loadedCfg, []byte(fmt.Sprintf("merge:\n  cap: %d\n", cap)), 0o600); err != nil {
			t.Fatal(err)
		}
		why, err := g.step()
		if err != nil {
			t.Fatalf("cap %d: %v", cap, err)
		}
		if cap == 5 && !strings.Contains(why, "cap 5") {
			t.Fatalf("expected cap wait, got %q", why)
		}
		if cap == 12 && why != "" {
			t.Fatalf("still waiting after raise: %q", why)
		}
	}
	if d := lastEventOf(t, g.app, verbMerged); !strings.Contains(d, "o/r#7 exit 0") {
		t.Fatalf("merge did not start: %q", d)
	}
}

func TestQueuedGateMovesToReloadedLane(t *testing.T) {
	g, _, _ := capGate(t)
	g.loadedCfg = filepath.Join(t.TempDir(), "config.yaml")
	raw := fmt.Sprintf("lanes:\n  - name: shared\n    repositories: [%s]\n", scratchRepo)
	if err := os.WriteFile(g.loadedCfg, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := g.reloadQueueConfig(); err != nil {
		t.Fatal(err)
	}
	st, err := g.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if g.lane.Name != "shared" || st.Merges[0].Lane != "shared" {
		t.Fatalf("lane not updated: %+v, %+v", g.lane, st.Merges)
	}
}
