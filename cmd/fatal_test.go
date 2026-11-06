package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/fatal"
	"github.com/giantswarm/beekeeper/internal/state"
)

func fakeFatalRun(t *testing.T, run func([]string) (string, int)) {
	t.Helper()
	old := fatalRun
	fatalRun = func(_ context.Context, _ string, args []string) (string, string, int, error) {
		out, code := run(args)
		return out, "", code, nil
	}
	t.Cleanup(func() { fatalRun = old })
}

func TestFatalServiceHonorsDisabledIntent(t *testing.T) {
	for _, status := range []string{fatalDisabledState, "masked", "not-found", "enabled", "unavailable"} {
		t.Run(status, func(t *testing.T) {
			fakeFatalRun(t, func(args []string) (string, int) {
				if slices.Contains(args, "is-enabled") {
					return status, 0
				}
				return "", 3
			})
			o := fatalService(t.Context(), "beekeeper-supervisor-loop.service")
			if o.Disabled != (status == fatalDisabledState || status == "masked" || status == "not-found") || o.Failed != (status == "enabled") || o.Unknown != (status == "unavailable") {
				t.Fatalf("%+v", o)
			}
		})
	}
}

func TestFatalScanOwnsOneRepairAndRespectsStop(t *testing.T) {
	for _, hold := range []string{ghNone, "stop", "starts"} {
		t.Run(hold, func(t *testing.T) {
			a, _ := stubApp(t)
			dir := a.store.Dir()
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(fmt.Sprintf("stateDir: %q\nfatal:\n  checks:\n    - name: startup\n      probe: [critical-probe]\n", dir)), 0600); err != nil {
				t.Fatal(err)
			}
			a.cfgPath = path
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				if hold == "stop" {
					st.Stop = &state.Stop{Reason: "person stopped", At: time.Now()}
				}
				if hold == "starts" {
					st.Holds = []state.Hold{{Target: holdStarts, Reason: "person held", At: time.Now()}}
				}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			failed := true
			fakeFatalRun(t, func(args []string) (string, int) {
				if slices.Contains(args, "is-enabled") {
					return fatalDisabledState, 1
				}
				if args[0] == "critical-probe" && failed {
					return "discard sensitive output", 1
				}
				return "", 0
			})
			old := beekeeperRun
			starts := 0
			beekeeperRun = func(_ context.Context, args ...string) (string, int, error) {
				starts++
				if slices.Contains(args, "--force") {
					t.Fatal("forced start")
				}
				if !slices.Contains(args, "--branch") || !slices.Contains(args, "--dir") {
					t.Fatal("repair must use a managed repository worktree")
				}
				q, err := fatal.Read(dir)
				if err != nil || len(q.Incidents) != 1 || q.Incidents[0].Owner == "" {
					t.Fatal("launch before durable owner", err)
				}
				return "", 0, nil
			}
			t.Cleanup(func() { beekeeperRun = old })
			for range 2 {
				if err := a.fatalScan(t.Context(), dir); err != nil {
					t.Fatal(err)
				}
			}
			want := 0
			if hold == ghNone {
				want = 1
			}
			if starts != want {
				t.Fatalf("starts %d want %d", starts, want)
			}
			q, err := fatal.Read(dir)
			if err != nil || q.Barrier("ordinary") == nil {
				t.Fatal("missing outage barrier", err)
			}
			failed = false
			if err := a.fatalScan(t.Context(), dir); err != nil {
				t.Fatal(err)
			}
			q, err = fatal.Read(dir)
			if err != nil || q.Barrier("ordinary") != nil {
				t.Fatal("verified recovery did not clear", err)
			}
		})
	}
}

func TestFatalMalformedConfigStillDetectsWithoutSupervisor(t *testing.T) {
	a, _ := stubApp(t)
	dir := a.store.Dir()
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("fatal: ["), 0600); err != nil {
		t.Fatal(err)
	}
	a.cfgPath = path
	fakeFatalRun(t, func(args []string) (string, int) { return fatalDisabledState, 0 })
	if err := a.fatalScan(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	q, err := fatal.Read(dir)
	if err != nil || len(q.Incidents) != 1 || q.Incidents[0].ID != "configuration" {
		t.Fatalf("%+v %v", q, err)
	}
	var out bytes.Buffer
	c := New()
	c.SetOut(&out)
	c.SetArgs([]string{workerGitHubConfigOption, path, "fatal", "--state-dir", dir, executionStatus})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "configuration") {
		t.Fatal(out.String())
	}
}

func TestFatalAdmissionBlocksWorkersButAllowsRepairAndReview(t *testing.T) {
	a, _ := stubApp(t)
	q := fatal.Queue{}
	q.Observe(fatal.Observation{ID: "installed-guard-check", Failed: true}, a.now)
	x := q.Reserve(a.now, func(string) bool { return false })
	if err := q.Save(a.store.Dir()); err != nil {
		t.Fatal(err)
	}
	called := false
	start := func() error { called = true; return nil }
	if err := a.withKindAdmission(t.Context(), "ordinary", start); err == nil || called {
		t.Fatal("ordinary admitted")
	}
	if err := a.withKindAdmission(t.Context(), x.Owner, start); err != nil || !called {
		t.Fatal("repair denied", err)
	}
	a.selectionKind = taskKindReview
	if err := a.withKindAdmission(t.Context(), "review existing fix", start); err != nil {
		t.Fatal("review denied", err)
	}
}

func TestFatalMainPinsHeadAndDistinguishesRedPendingUnknown(t *testing.T) {
	for _, tc := range []struct {
		name, status, checks string
		red, unknown         bool
	}{
		{"all checks succeeded", `{"sha":"abc","state":"success","total_count":1}`, `{"check_runs":[{"conclusion":"success"}]}`, false, false},
		{"legacy red", `{"sha":"abc","state":"failure","total_count":1}`, `{"check_runs":[]}`, true, false},
		{"check red", `{"sha":"abc","state":"success","total_count":1}`, `{"check_runs":[{"conclusion":"failure","html_url":"https://example.test/run"}]}`, true, false},
		{executionPending, `{"sha":"abc","state":"pending","total_count":1}`, `{"check_runs":[{executionStatus:"in_progress"}]}`, false, true},
		{"missing commit metadata", `{}`, `{}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeFatalRun(t, func(args []string) (string, int) {
				if args[0] != "gh" {
					return "", 0
				}
				if strings.Contains(args[2], "/status?") {
					return tc.status, 0
				}
				if !strings.Contains(args[2], "/commits/abc/") {
					t.Fatal("check not pinned", args)
				}
				return tc.checks, 0
			})
			o := fatalMain(t.Context(), "example/tool", "/config.yaml")
			if o.Failed != tc.red || o.Unknown != tc.unknown {
				t.Fatalf("%+v", o)
			}
		})
	}
}

func TestFatalAdmissionRefusalDoesNotExhaustRepairBudget(t *testing.T) {
	a, _ := stubApp(t)
	dir := a.store.Dir()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("stateDir: %q\nfatal:\n  repositories: []\n  checks:\n    - name: startup\n      probe: [critical-probe]\n", dir)), 0600); err != nil {
		t.Fatal(err)
	}
	a.cfgPath = path
	fakeFatalRun(t, func(args []string) (string, int) {
		if slices.Contains(args, "is-enabled") {
			return fatalDisabledState, 1
		}
		if args[0] == "critical-probe" {
			return "", 1
		}
		return "", 0
	})
	old := beekeeperRun
	beekeeperRun = func(context.Context, ...string) (string, int, error) { return "", ExitRefused, nil }
	t.Cleanup(func() { beekeeperRun = old })
	if err := a.fatalScan(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	q, err := fatal.Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Incidents) != 1 || q.Incidents[0].Attempts != 0 || q.Incidents[0].Owner != "" {
		t.Fatalf("%+v", q)
	}
}

func TestFatalFailedServiceRestartImmediatelyGetsOneRepairOwner(t *testing.T) {
	a, _ := stubApp(t)
	dir := a.store.Dir()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("stateDir: %q\nfatal: {repositories: []}\n", dir)), 0600); err != nil {
		t.Fatal(err)
	}
	a.cfgPath = path
	restarts := 0
	starts := 0
	fakeFatalRun(t, func(args []string) (string, int) {
		if slices.Contains(args, "enable") {
			t.Fatal("re-enabled a component")
		}
		if args[0] != "systemctl" {
			return "", 0
		}
		if slices.Contains(args, "is-enabled") {
			return "enabled", 0
		}
		if slices.Contains(args, "start") {
			restarts++
			return "", 1
		}
		return "", 3
	})
	old := beekeeperRun
	beekeeperRun = func(_ context.Context, args ...string) (string, int, error) {
		starts++
		i := slices.Index(args, "--name")
		if i < 0 {
			t.Fatal("owner missing")
		}
		if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
			st.Agents = append(st.Agents, state.Agent{Party: state.Party{Name: args[i+1]}, Task: "repair"})
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		return "", 0, nil
	}
	t.Cleanup(func() { beekeeperRun = old })
	for range 2 {
		if err := a.fatalScan(t.Context(), dir); err != nil {
			t.Fatal(err)
		}
	}
	if restarts != 1 || starts != 1 {
		t.Fatalf("%d restarts %d workers", restarts, starts)
	}
}
