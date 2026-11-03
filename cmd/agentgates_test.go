//go:build unix

package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// stoppingWorkerLauncher models the gate process in a stopped worker unit.
// Its merge runner may be independent, or ended with the worker.
type stoppingWorkerLauncher struct {
	platform.Launcher
	stop func()
}

func (l stoppingWorkerLauncher) State(context.Context, string) string { return holdActive }
func (l stoppingWorkerLauncher) Stop(context.Context, string) error   { l.stop(); return nil }

func TestStoppingAWorkerPreservesItsGateOrSettlesItsPlaceImmediately(t *testing.T) {
	const workerID = "test-worker"
	for _, harness := range []string{codex.Harness, agy.Harness, omp.Harness} {
		for _, tc := range []struct {
			name             string
			survives         bool
			doc, pull, phase string
		}{
			{name: "independent runner", survives: true, phase: state.Running},
			{name: "finished release", doc: mergedDoc},
			{name: "merged awaiting release", pull: github.Merged, phase: state.Settling},
			{name: "unreadable outcome", phase: state.Settling},
			{name: "nothing merged", pull: github.Open},
		} {
			t.Run(harness+"/"+tc.name, func(t *testing.T) {
				a, _ := stubApp(t)
				a.now = time.Now()
				asked := stubGitHub(t, tc.pull, "")
				start := func() (*exec.Cmd, func()) {
					c := exec.Command("sleep", "30")
					if err := c.Start(); err != nil {
						t.Skip("no sleep:", err)
					}
					stopped := false
					stop := func() {
						if !stopped {
							_ = c.Process.Kill()
							_ = c.Wait()
							stopped = true
						}
					}
					t.Cleanup(stop)
					return c, stop
				}
				gate, stopGate := start()
				runner, stopRunner := start()
				m := state.Merge{Repo: scratchRepo, PR: 7, Lane: scratchRepo, By: four, PID: gate.Process.Pid, Child: runner.Process.Pid, Phase: state.Running, Started: a.now, Joined: a.now, Seen: a.now}
				if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
					st.Merges = []state.Merge{m, {Repo: scratchRepo, PR: 8, Lane: scratchRepo, Phase: state.Waiting, PID: os.Getpid(), Joined: a.now, Seen: a.now}}
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
				base := mergeBase(a.store.Dir(), m.Repo, m.PR)
				if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(base+".pid", []byte(strconv.Itoa(m.Child)), 0o600); err != nil {
					t.Fatal(err)
				}
				if tc.doc != "" {
					if err := os.WriteFile(base+".json", []byte(tc.doc), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(base+".rc", []byte("0"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				old := plat.Launcher
				plat.Launcher = stoppingWorkerLauncher{Launcher: old, stop: func() {
					stopGate()
					if !tc.survives {
						stopRunner()
					}
				}}
				t.Cleanup(func() { plat.Launcher = old })
				var said string
				switch harness {
				case codex.Harness:
					said = a.endCodex(context.Background(), workerID)
				case agy.Harness:
					said = a.endAgy(context.Background(), workerID)
				case omp.Harness:
					said = a.endOmp(context.Background(), workerID)
				}
				if !strings.Contains(said, "stopped its "+harness+" unit") {
					t.Fatalf("worker stop: %s", said)
				}
				st, err := a.store.Read()
				if err != nil {
					t.Fatal(err)
				}
				found, queued := false, false
				for _, got := range st.Merges {
					if got.PR == 8 {
						queued = true
						continue
					}
					if got.PR == 7 {
						found = true
						if tc.phase == "" || got.Phase != tc.phase {
							t.Fatalf("place after worker stop: %+v", got)
						}
					}
				}
				if !queued || found != (tc.phase != "") {
					t.Fatalf("places after worker stop: %+v", st.Merges)
				}
				if tc.survives {
					if !proc.Alive(m.Child) || *asked != 0 {
						t.Fatalf("independent runner disturbed: alive=%v GitHub reads=%d", proc.Alive(m.Child), *asked)
					}
				} else if tc.phase == state.Settling {
					// No hung-run grace, watch pass or manual lane drop was needed.
					evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == verbMerged || e.Verb == "merge.unknown" })
					if err != nil || len(evs) != 1 {
						t.Fatalf("no synchronous settlement: %+v %v", evs, err)
					}
				}
			})
		}
	}
}
