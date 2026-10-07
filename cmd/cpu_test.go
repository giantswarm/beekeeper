package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
)

func table(ps ...*proc.Process) *proc.Table {
	t := &proc.Table{ByPID: map[int]*proc.Process{}}
	for _, p := range ps {
		t.ByPID[p.PID] = p
	}
	return t
}

// The top consumers are the CPU time burned since the last read, grouped by
// command name; a reused PID counts as new.
func TestTopCPUGroupsTheSpanByCommand(t *testing.T) {
	prev := table(
		&proc.Process{PID: 1, Comm: "go", StartTicks: 5, CPU: 100 * time.Second},
		&proc.Process{PID: 2, Comm: "go", StartTicks: 5, CPU: 10 * time.Second},
		&proc.Process{PID: 3, Comm: "vim", StartTicks: 5, CPU: 50 * time.Second},
		&proc.Process{PID: 4, Comm: "old", StartTicks: 5, CPU: 90 * time.Second},
	)
	cur := table(
		&proc.Process{PID: 1, Comm: "go", StartTicks: 5, CPU: 130 * time.Second},
		&proc.Process{PID: 2, Comm: "go", StartTicks: 5, CPU: 40 * time.Second},
		&proc.Process{PID: 3, Comm: "vim", StartTicks: 5, CPU: 50 * time.Second},
		&proc.Process{PID: 4, Comm: "cc1", StartTicks: 9, CPU: 15 * time.Second},
		&proc.Process{PID: 5, Comm: "bash", StartTicks: 9, CPU: 3 * time.Second},
	)
	got := topCPU(prev, cur, 30*time.Second, 2)
	if len(got) != 2 || got[0] != (cpuUse{"go", 2, 2}) || got[1] != (cpuUse{"cc1", 0.5, 1}) {
		t.Fatalf("topCPU = %+v", got)
	}
	line := topCPULine(got, 30*time.Second)
	if line != "; top CPU over 30s: go 2.0 cores (2), cc1 0.5 cores (1)" {
		t.Errorf("line = %q", line)
	}
	if topCPU(nil, cur, 30*time.Second, 5) != nil || topCPULine(nil, time.Second) != "" {
		t.Error("a first read names consumers")
	}
}

// LOAD RISING is a steep climb past one per core, not a high steady load.
func TestLoadRising(t *testing.T) {
	for _, c := range []struct {
		load [3]float64
		want bool
	}{
		{[3]float64{30, 10, 4}, true},
		{[3]float64{20, 5, 2}, false},   // under one per core
		{[3]float64{40, 30, 20}, false}, // high, not climbing
	} {
		if got := loadRising(c.load, 24); got != c.want {
			t.Errorf("loadRising(%v, 24) = %v", c.load, got)
		}
	}
}

// CPU PRESSURE waits for a second sample over watch.cpuPSIMax;
// it ends with one ENDED line.
func TestWatchSaysCPUPressureOnTheSecondSample(t *testing.T) {
	w, out := loopWatcher(t, "250ms", ", loadMax: 1000000, cpuPSIMax: -1", nil)
	now := time.Now()
	w.sampleCPU(now, nil)
	if strings.Contains(out.String(), "CPU PRESSURE") {
		t.Fatalf("CPU PRESSURE on the first sample:\n%s", out)
	}
	w.sampleCPU(now.Add(time.Second), nil)
	if !strings.Contains(out.String(), "CPU PRESSURE: some avg10") {
		t.Fatalf("no CPU PRESSURE with its consumers on the second sample:\n%s", out)
	}
	w.cfg.Watch.CPUPSIMax = 1000
	w.sampleCPU(now.Add(2*time.Second), nil)
	if !strings.Contains(out.String(), "ENDED CPU PRESSURE") {
		t.Errorf("no ENDED CPU PRESSURE:\n%s", out)
	}
}

// A swapoff is read from the sample's process table.
func TestSwapoffRuns(t *testing.T) {
	if swapoffRuns(nil) {
		t.Error("swapoff without a process table")
	}
	if swapoffRuns(table(&proc.Process{PID: 1, Comm: "init"})) {
		t.Error("swapoff with none running")
	}
	if !swapoffRuns(table(&proc.Process{PID: 1, Comm: "init"}, &proc.Process{PID: 2, Comm: "swapoff"})) {
		t.Error("a running swapoff missed")
	}
}
