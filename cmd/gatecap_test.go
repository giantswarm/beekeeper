//go:build unix

package cmd

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
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
	hostDevctl(t, slices.Concat(devctlArgv(3, "pr", "wait"), devctlArgv(3, "release", "wait"), devctlArgv(3, "rollout", "wait", "gazelle"))...)
	if why := g.capWait(st, me); why != "" {
		t.Errorf("cap wait %q with nine waits and no merge running, want none", why)
	}
}

// Five merges and promotes still fill a cap of 5, the waits beside them
// counted for nothing.
func TestMergeCapCountsMergesAndPromotes(t *testing.T) {
	g, st, me := capGate(t)
	hostDevctl(t, slices.Concat(devctlArgv(3, "pr", "merge"), devctlArgv(2, "release", "promote"), devctlArgv(4, "pr", "wait"))...)
	if why := g.capWait(st, me); !strings.Contains(why, "5 merges and promotes run machine-wide (cap 5)") {
		t.Errorf("cap wait %q, want the five merges and promotes to fill the cap", why)
	}
	// A promotion's dry run and a help call merge nothing.
	hostDevctl(t, slices.Concat(devctlArgv(4, "pr", "merge"), [][]string{
		{"devctl", "release", "promote", "o/y", "--dry-run"}, {"devctl", "pr", "merge", "--help"}})...)
	if why := g.capWait(st, me); why != "" {
		t.Errorf("cap wait %q with four merges, a dry run and a help call, want none", why)
	}
}
