//go:build unix

package cmd

import (
	"bufio"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/gate"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestStopInterruptsMergeAwaitingRelease(t *testing.T) {
	a, _, _ := stopDesk(t)
	// This isolated process stands in for the live gate. Its merge has
	// entered the lane, but no merge-child exists while a release is pending.
	gate := exec.Command("sleep", "60")
	if err := gate.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = gate.Wait(); close(done) }()
	t.Cleanup(func() { _ = gate.Process.Kill(); <-done })
	seed(t, a, func(st *state.State) {
		st.Merges = []state.Merge{{Repo: scratchRepo, PR: 8, Lane: scratchRepo, Phase: state.Running, PID: gate.Process.Pid}}
	})
	if err := a.stop(context.Background(), "fixture operator stop"); err != nil {
		t.Fatal(err)
	}
	if proc.Alive(gate.Process.Pid) {
		t.Fatal("stop reported success but left the release-waiting gate running")
	}
	out := a.out.(interface{ String() string }).String()
	if !strings.Contains(out, "cancelled while it waited for lane") || !strings.Contains(out, "before its devctl started") {
		t.Fatalf("stop did not report cancellation before the merge started: %s", out)
	}
}

// control: none: the gate extraction only changes names and access paths; assertions are unchanged.
// The merge base has no internal/gate package, so these tests cannot build there.
func TestStopReportsReleaseWaiterThatIgnoresInterrupt(t *testing.T) {
	a, _, _ := stopDesk(t)
	was := gate.StopWait
	gate.StopWait = 20 * time.Millisecond
	t.Cleanup(func() { gate.StopWait = was })
	gate := exec.Command("sh", "-c", "trap '' INT; echo interrupt-handler-ready; exec sleep 60")
	ready, err := gate.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = gate.Wait(); close(done) }()
	t.Cleanup(func() { _ = gate.Process.Kill(); <-done })
	if line, err := bufio.NewReader(ready).ReadString('\n'); err != nil || line != "interrupt-handler-ready\n" {
		t.Fatalf("fixture did not ignore interrupts: %q, %v", line, err)
	}
	seed(t, a, func(st *state.State) {
		st.Merges = []state.Merge{{Repo: scratchRepo, PR: 8, Lane: scratchRepo, Phase: state.Running, PID: gate.Process.Pid}}
	})
	if err := a.stop(context.Background(), "fixture stop with unresponsive gate"); err == nil {
		t.Fatal("stop reported success despite the gate ignoring its interrupt")
	}
	out := a.out.(interface{ String() string }).String()
	if !strings.Contains(out, "FAILED the gate of "+scratchRepo+"#8") {
		t.Fatalf("stop did not name the surviving gate: %s", out)
	}
}
