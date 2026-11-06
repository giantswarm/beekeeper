package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/proc"
)

func batchWatcher(t *testing.T) (*watcher, *strings.Builder, *fakeClock) {
	t.Helper()
	w, out := onceWatch(t, nil)
	clk := newFakeClock(t)
	w.clock = clk
	w.batchEvery = 5 * time.Minute
	w.cfg.Alerts.Team = ourTeam
	return w, out, clk
}

func TestWatchBatchKeepsActionsImmediate(t *testing.T) {
	w, out, _ := batchWatcher(t)
	w.emitNow("doctor", "DOCTOR archived a finished worker")
	w.emitNow("sessions", "SESSIONS started: transient")
	w.emitNow("central-feed", "CENTRAL agents.register (worker): registered [person on host]")
	if out.Len() != 0 {
		t.Fatal(out.String())
	}
	actions := []string{
		"KERNEL OOM: test", "LOW RAM: test", "OOMD IMMINENT: test",
		"PAGE UNOWNED: test", "RELAY DUE: test", "HANDOVER DUE: test", "TIMER: test",
		"WORKER REPORT: test", "PROBLEM FOUND: test", "LANE STALLED: test", "RUNAWAY: test",
		"LEASE: test", "GRANT: test", "DOCTOR FAULT test", "DOCTOR FAILED test",
		"ALERT NEW alpha PAGE other PageAlert ns/pod", "ALERT NEW alpha notify BUMBLEBEE OurAlert ns/pod",
		"PARKED ON A PERSON: test", "AGENT RESUME FAILED: test", "NEW UNKNOWN ACTION: test",
	}
	for _, line := range actions {
		before := strings.Count(out.String(), "\n")
		w.emitNow("pending", "%s", line)
		if got := strings.Count(out.String(), "\n"); got != before+1 || !strings.Contains(out.String(), line) {
			t.Fatalf("action delayed: %s", line)
		}
	}
	if strings.Contains(out.String(), "BATCH:") {
		t.Fatal("action flushed routine items")
	}
	evs, err := w.store.Events(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) < len(actions)+3 {
		t.Fatal("routine events were not durable before the tick")
	}
	w.flushBatch()
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	_, raw, ok := strings.Cut(lines[len(lines)-1], " BATCH: ")
	var items []string
	if !ok || json.Unmarshal([]byte(raw), &items) != nil || len(items) != 3 {
		t.Fatalf("batch = %s", raw)
	}
}

func TestWatchBatchQuietHalfHour(t *testing.T) {
	w, out, clk := batchWatcher(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.batchLoop(ctx) }()
	defer func() { cancel(); <-done }()
	// Twenty routine events per five-minute interval still produce one
	// physical stdout event, while an empty interval produces none.
	for interval := 0; interval < 6; interval++ {
		clk.settle(1)
		for i := 0; i < 20; i++ {
			w.emitNow("doctor", "DOCTOR archived worker %d", i)
		}
		clk.advance(5 * time.Minute)
		clk.settle(1)
		if got := strings.Count(out.String(), "\n"); got != interval+1 {
			t.Fatalf("%d events at interval %d", got, interval)
		}
	}
	clk.advance(5 * time.Minute)
	clk.settle(1)
	if got := strings.Count(out.String(), "\n"); got != 6 {
		t.Fatalf("empty tick printed an event: %d", got)
	}
}

func TestWatchBatchDisabledAndEmbeddedNewlines(t *testing.T) {
	w, out, _ := batchWatcher(t)
	w.batchEvery = 0 // --once never enables the buffer
	w.emitNow("sessions", "SESSIONS started: once")
	if !strings.Contains(out.String(), "once") {
		t.Fatal("--once buffered an event")
	}
	w.batchEvery = -time.Second
	w.emitNow("doctor", "DOCTOR disabled")
	if !strings.Contains(out.String(), "disabled") {
		t.Fatal("disabled batching buffered an event")
	}
	out.Reset()
	w.batchEvery = time.Minute
	w.emitNow("doctor", "DOCTOR multiline\nsecond line")
	w.flushBatch()
	if strings.Count(out.String(), "\n") != 1 || !strings.Contains(out.String(), `\nsecond line`) {
		t.Fatal(out.String())
	}
}

func TestWatchBatchOtherTeamNotifyAndRecovery(t *testing.T) {
	w, out, _ := batchWatcher(t)
	for _, line := range []string{"ALERT NEW alpha notify other Alert ns/pod", "ENDED HIGH LOAD (since 12:00)", "ENDED CPU PRESSURE (since 12:00)", "ENDED READS SLOWED (since 12:00)"} {
		w.emitNow("alerts", "%s", line)
	}
	if out.Len() != 0 {
		t.Fatal(out.String())
	}
	w.emitNow("ended", "ENDED LOW RAM (since 12:00)")
	if !strings.Contains(out.String(), "ENDED LOW RAM") {
		t.Fatal("memory recovery delayed")
	}
	w.cfg.Alerts.Team = ""
	w.emitNow("alerts", "ALERT NEW alpha notify unknown Alert ns/pod")
	if !strings.Contains(out.String(), "unknown") {
		t.Fatal("unknown team delayed")
	}
}

// A sanitized hour of recorded desk output supplies the same events to
// both policies; every action must remain at exactly its original time.
func TestWatchBatchRecordedHour(t *testing.T) {
	raw, err := os.ReadFile("testdata/watch-hour.txt")
	if err != nil {
		t.Fatal(err)
	}
	w, out, clk := batchWatcher(t)
	start, _ := time.Parse("15:04:05", "04:00:00")
	clk.mu.Lock()
	clk.now = start
	clk.mu.Unlock()
	next := start.Add(w.batchEvery)
	before, actions := 0, 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		at, err := time.Parse("15:04:05", line[:8])
		if err != nil {
			t.Fatal(err)
		}
		for !at.Before(next) {
			clk.mu.Lock()
			clk.now = next
			clk.mu.Unlock()
			w.flushBatch()
			next = next.Add(w.batchEvery)
		}
		clk.mu.Lock()
		clk.now = at
		clk.mu.Unlock()
		text := line[9:]
		routine := w.routineLine(text)
		w.outputEvent(text)
		before++
		if !routine {
			actions++
			if !strings.HasSuffix(out.String(), line+"\n") {
				t.Fatalf("action moved: %s", line)
			}
		}
	}
	w.flushBatch()
	after := strings.Count(out.String(), "\n")
	if after >= before {
		t.Fatalf("no reduction: %d -> %d", before, after)
	}
	t.Logf("recorded hour: %d stdout events -> %d (%d immediate actions); %.1f%% fewer wakes", before, after, actions, 100*float64(before-after)/float64(before))
}

func TestWatchTickRejectsNonpositiveDuration(t *testing.T) {
	for _, value := range []string{"0s", "-1s"} {
		t.Run(value, func(t *testing.T) {
			c := (&app{cfg: &config.Config{}}).watchCmd()
			// Avoid creating a host scope; validation precedes every watch read.
			t.Setenv(restartedEnv, "1")
			c.SetArgs([]string{"--tick", value})
			if err := c.Execute(); err == nil || !strings.Contains(fmt.Sprint(err), "--tick must be positive") {
				t.Fatalf("tick %s: %v", value, err)
			}
		})
	}
}

func TestWatchBatchLoopRunsBetweenSlowPolls(t *testing.T) {
	needsPlatform(t)
	w, out := loopWatcher(t, "1h", "", nil)
	clk := newFakeClock(t)
	w.clock = clk
	w.batchEvery = time.Minute // --tick overrides the disabled configuration
	runWatch(t, w)
	clk.settle(loopTimers + 1)
	w.emitNow("sessions", "SESSIONS started: batch-fixture")
	if strings.Contains(out.String(), "batch-fixture") {
		t.Fatal("routine item escaped before the tick")
	}
	clk.advance(time.Minute)
	clk.settle(loopTimers + 1)
	if got := strings.Count(out.String(), " BATCH: "); got != 1 || !strings.Contains(out.String(), "batch-fixture") {
		t.Fatalf("batch loop did not flush between polls:\n%s", out.String())
	}
	w.emitNow("pending", "WORKER REPORT: immediate-between-ticks")
	if !strings.Contains(out.String(), "immediate-between-ticks") {
		t.Fatal("action waited for a poll or batch tick")
	}
}

// These production doctor messages describe unresolved failures, even
// though their source also emits completed chores under the DOCTOR prefix.
func TestWatchBatchDoctorFailuresAreImmediate(t *testing.T) {
	for _, tc := range []struct {
		name, line string
		chore      bool
	}{
		{name: "stale state writer", line: staleLine(staleBinary{Process: &proc.Process{PID: 123, Args: []string{pluginName, agentsName, agentStartName}}})},
		{name: "failed unit reset", line: "the failed units beekeeper-worker.service stay failed: access denied", chore: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, out, _ := batchWatcher(t)
			if tc.chore {
				w.sayChores(doctorReport{chores: []string{tc.line}})
			} else {
				w.check("doctor-stale 123", true, "DOCTOR %s", tc.line)
			}
			if !strings.Contains(out.String(), "DOCTOR "+tc.line) {
				t.Fatalf("actionable doctor failure buffered before tick: %s", tc.line)
			}
			w.flushBatch()
			if strings.Contains(out.String(), " BATCH: ") {
				t.Fatal("failure also entered routine batch")
			}
		})
	}
}

func TestWatchBatchDoctorSuccessfulUnitResetIsRoutine(t *testing.T) {
	w, out, _ := batchWatcher(t)
	line := "reset the failed units beekeeper-worker.service: their outcomes are in the event log"
	w.sayChores(doctorReport{chores: []string{line}})
	if out.Len() != 0 {
		t.Fatal("completed reset woke the monitor before the tick")
	}
	w.flushBatch()
	if !strings.Contains(out.String(), line) {
		t.Fatal("completed reset missing from batch")
	}
}
