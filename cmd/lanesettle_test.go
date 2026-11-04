//go:build unix

package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

func TestLaneSettleWaitsForTermedGate(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	gate := exec.Command("sh", "-c", `trap 'sleep 0.2; exit 0' TERM; touch "$1"; while :; do sleep 0.05; done`, "sh", ready) //nolint:gosec // a test fixture
	if err := gate.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = gate.Wait(); close(done) }()
	t.Cleanup(func() { _ = gate.Process.Kill(); <-done })
	if !eventually(5*time.Second, func() bool { _, err := os.Stat(ready); return err == nil }) {
		t.Fatal("gate not ready")
	}
	a := queueApp(t, state.Merge{Repo: repo, PR: 1, Lane: repo, PID: gate.Process.Pid, Phase: state.Running})
	a.as = litOperator
	var out bytes.Buffer
	a.out = &out
	bin := fakeGH(t, `'{"state":"MERGED","mergedAt":"2026-01-01T00:00:00Z"}'`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := gate.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	c := a.settleCmd()
	c.SetContext(context.Background())
	if err := c.RunE(c, []string{"o/r", "1"}); err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Merges) != 1 || st.Merges[0].Phase != state.Settling || !st.Merges[0].Outside || st.Merges[0].PID != 0 {
		t.Fatalf("not settled outside gate: %+v", st.Merges)
	}
	if !strings.Contains(out.String(), "outside the gate") {
		t.Fatalf("output: %s", out.String())
	}
}

func TestLaneSettleWaitBoundAndCancellation(t *testing.T) {
	for _, child := range []bool{false, true} {
		t.Run(strconv.FormatBool(child), func(t *testing.T) {
			m := state.Merge{Repo: "o/r", PR: 1, Phase: state.Running, PID: os.Getpid()}
			if child {
				m.PID, m.Child = 0, os.Getpid()
			}
			a := queueApp(t, m)
			start := time.Now()
			err := a.waitSettleRun(context.Background(), "o/r", 1, 20*time.Millisecond)
			for _, want := range []string{refOne, strconv.Itoa(os.Getpid()), "still exiting", "20ms", "retry: beekeeper lanes settle o/r 1"} {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("want %q in %v", want, err)
				}
			}
			if time.Since(start) > time.Second {
				t.Fatal("wait exceeded bound")
			}
			st, err := a.store.Read()
			if err != nil || st.Merges[0].Phase != state.Running {
				t.Fatalf("wait changed state: %+v, %v", st, err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := a.waitSettleRun(ctx, "o/r", 1, time.Minute); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation: %v", err)
			}
		})
	}
}

// A gate can finish and remove its entry while settle waits for it.
func TestLaneSettleWaitRereadsState(t *testing.T) {
	a := queueApp(t, state.Merge{Repo: "o/r", PR: 1, Phase: state.Running, PID: os.Getpid()})
	done := make(chan error, 1)
	go func() { done <- a.waitSettleRun(context.Background(), "o/r", 1, time.Second) }()
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = nil
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
