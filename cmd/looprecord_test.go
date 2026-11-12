package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

func TestDryRunRecordsBeyondOneSweepBatch(t *testing.T) {
	r := newLoopRig(t)
	var open []string
	for n := range loop.SweepReadsPerPass + 10 {
		ref := sweepRef(100 + n)
		open = append(open, ref)
		r.pulls[ref] = superloop.Pull{Ref: ref, URL: fmt.Sprintf("https://github.com/o/r/pull/%d", 100+n), State: superloop.Open, Head: loopHead, Green: true, Author: loopDesk}
	}
	listed := loop.ReadOpenPulls
	t.Cleanup(func() { loop.ReadOpenPulls = listed })
	loop.ReadOpenPulls = func(context.Context, func() ([]string, error), []string) ([]string, error) { return open, nil }
	dir := t.TempDir()
	if err := r.a.dryRunSupervisorLoop(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	var recorded loop.LoopRecording
	readReplayJSON(t, filepath.Join(dir, loop.LoopRecordInputFile), &recorded)
	if len(recorded.Planner.Pulls) != len(open) {
		t.Fatalf("recorded %d pulls, want %d", len(recorded.Planner.Pulls), len(open))
	}
	if len(recorded.Deliveries) != len(open) {
		t.Fatalf("recorded %d derived deliveries, want %d", len(recorded.Deliveries), len(open))
	}
}

func TestLoopRecordAnonymizesAndDoesNotOverwrite(t *testing.T) {
	const privateRef = "private-owner/private-repo#9876"
	input := loop.LoopRecording{
		Planner: superloop.Input{Workers: []superloop.Worker{{Name: "private-worker", ParkedOn: privateRef}}, Pulls: map[string]superloop.Pull{privateRef: {Ref: privateRef, URL: "https://github.com/private-owner/private-repo/pull/9876", Body: "private-body", Head: "private-head"}}},
		Agents:  []state.Agent{{Party: state.Party{Name: "private-worker", Session: "private-session"}, Task: "private-task", Park: &state.Park{On: privateRef, Continuation: []string{"private-event"}}}},
	}
	input.Planner.Open = func(ref string) bool { return ref == privateRef }
	input.Capture(input.Planner)
	_ = input.Planner.Open(privateRef)
	dir := t.TempDir()
	if err := loop.WriteLoopRecording(dir, input); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, loop.LoopRecordInputFile)) //nolint:gosec // test TempDir fixture
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-") || strings.Contains(string(data), "9876") {
		t.Fatal("recording leaked identity or free text")
	}
	var replay loop.LoopRecording
	if err := json.Unmarshal(data, &replay); err != nil {
		t.Fatal(err)
	}
	worker := replay.Planner.Workers[0]
	if worker.Name == "" || worker.Name != replay.Agents[0].Name || worker.ParkedOn != replay.Agents[0].Park.On || replay.Planner.Pulls[worker.ParkedOn].Ref != worker.ParkedOn {
		t.Fatal("inconsistent placeholders")
	}
	if !replay.ReplayInput().Open(worker.ParkedOn) {
		t.Fatal("recorded query fact lost")
	}
	if err := loop.WriteLoopRecording(dir, input); err == nil {
		t.Fatal("overwrote evidence")
	}
}

// The same replay runner covers pure decisions, reservation admission and
// fresh continuations; start admission does not live in superloop.Plan.
func TestStandstillReplay(t *testing.T) {
	paths, err := filepath.Glob("../internal/superloop/testdata/standstills/*/input.json")
	if err != nil || len(paths) < 2 {
		t.Fatalf("fixtures missing: %v", err)
	}
	for _, path := range paths {
		t.Run(filepath.Base(filepath.Dir(path)), func(t *testing.T) {
			var in loop.LoopRecording
			readReplayJSON(t, path, &in)
			var want struct {
				Starts        int
				Continuations []string
				Actions       map[string]int
			}
			readReplayJSON(t, filepath.Join(filepath.Dir(path), "want.json"), &want)
			a, out := supervisorApp(t)
			a.now = in.Planner.Now
			l := &loop.Loop{Host: a, DryRun: true, Said: map[string]bool{}}
			st := &state.State{Agents: in.Agents, Deliveries: in.Deliveries}
			if got := l.DeliveryRoom(in.Room, st); got < want.Starts {
				t.Fatalf("standstill: starts %d, need %d", got, want.Starts)
			}
			if err := l.ContinueParks(t.Context(), st); err != nil {
				t.Fatal(err)
			}
			for _, name := range want.Continuations {
				if !strings.Contains(out.String(), "SUPERVISOR WOULD CONTINUE "+name+" ") {
					t.Fatalf("standstill: continuation %s missing: %s", name, out)
				}
			}
			counts := map[string]int{}
			for _, action := range superloop.Plan(in.ReplayInput()) {
				counts[action.Kind]++
			}
			for kind, minimum := range want.Actions {
				if counts[kind] < minimum {
					t.Fatalf("standstill: %s=%d, need %d", kind, counts[kind], minimum)
				}
			}
		})
	}
}

func readReplayJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // testdata replay fixture
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		t.Fatal(err)
	}
}

func TestDryRunWritesRecordingWithoutActions(t *testing.T) {
	r := newLoopRig(t)
	r.ownedBy("team-a")
	r.park(t, loopPR, time.Now())
	before, err := r.a.store.Events(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := r.a.dryRunSupervisorLoop(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	var recorded loop.LoopRecording
	readReplayJSON(t, filepath.Join(dir, loop.LoopRecordInputFile), &recorded)
	if len(recorded.Planner.Workers) == 0 {
		t.Fatal("no planner workers recorded")
	}
	after, err := r.a.store.Events(0, nil)
	if err != nil || len(after) != len(before) {
		t.Fatalf("record mutated events: %v", err)
	}
	for _, call := range r.calls {
		if call[0] == agentsName {
			t.Fatalf("record ran an action: %v", call)
		}
	}
}
