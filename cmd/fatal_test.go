package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/fatal"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	fatalUnavailable   = "unavailable"
	fatalRepairTask    = "repair"
	fatalCriticalProbe = "critical-probe"
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

func TestFatalRepairOwnerUsesProcessLiveness(t *testing.T) {
	for _, tc := range []struct {
		name                                     string
		live, unknown, parked, importing, repair bool
	}{
		{"dead process can retry", false, false, false, false, true},
		{"active process retains ownership", true, false, false, false, false},
		{"unreadable process table retains ownership", false, true, false, false, false},
		{"parked review retains ownership", false, false, true, false, false},
		{"pending import retains ownership", false, false, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := stubApp(t)
			dir := a.store.Dir()
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(fmt.Sprintf("stateDir: %q\nfatal:\n  repositories: []\n  checks:\n    - name: startup\n      probe: [critical-probe]\n", dir)), 0600); err != nil {
				t.Fatal(err)
			}
			a.cfgPath = path
			now := time.Now().UTC()
			q := fatal.Queue{}
			q.Observe(fatal.Observation{ID: "check:startup", Failed: true}, now.Add(-time.Hour))
			x := q.Reserve(now.Add(-20*time.Minute), func(string) bool { return false })
			owner := x.Owner
			if err := q.Save(dir); err != nil {
				t.Fatal(err)
			}
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				ag := state.Agent{Party: state.Party{Name: owner, Session: "synthetic-fatal-repair"}, Task: fatalRepairTask}
				if tc.parked {
					ag.Park = &state.Park{At: now}
				}
				if tc.importing {
					ag.Import = &state.ImportWait{Until: now.Add(time.Hour)}
				}
				st.Agents = append(st.Agents, ag)
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			fakeFatalRun(t, func(args []string) (string, int) {
				if slices.Contains(args, "is-enabled") {
					return fatalDisabledState, 1
				}
				if args[0] == fatalCriticalProbe {
					return "", 1
				}
				return "", 0
			})
			oldLive := fatalSessionAlive
			fatalSessionAlive = func(*app, state.Party) (bool, error) {
				if tc.unknown {
					return false, errors.New("process snapshot unavailable")
				}
				return tc.live, nil
			}
			t.Cleanup(func() { fatalSessionAlive = oldLive })
			oldRun := beekeeperRun
			starts := 0
			beekeeperRun = func(context.Context, ...string) (string, int, error) { starts++; return "", 0, nil }
			t.Cleanup(func() { beekeeperRun = oldRun })
			if err := a.fatalScan(t.Context(), dir); err != nil {
				t.Fatal(err)
			}
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if (starts == 1) != tc.repair || st.Agents[0].Done != tc.repair {
				t.Fatalf("starts %d; done %v", starts, st.Agents[0].Done)
			}
		})
	}
}

func TestFatalServiceHonorsDisabledIntent(t *testing.T) {
	for _, status := range []string{fatalDisabledState, "masked", "not-found", fatalEnabledState, fatalUnavailable} {
		t.Run(status, func(t *testing.T) {
			fakeFatalRun(t, func(args []string) (string, int) {
				if slices.Contains(args, "is-enabled") {
					return status, 0
				}
				return "", 3
			})
			o := fatalService(t.Context(), "beekeeper-supervisor-loop.service")
			if o.Disabled != (status == fatalDisabledState || status == "masked" || status == "not-found") || o.Failed != (status == fatalEnabledState) || o.Unknown != (status == fatalUnavailable) {
				t.Fatalf("%+v", o)
			}
		})
	}
}

func TestFatalScanOwnsOneRepairAndRespectsStop(t *testing.T) {
	const harnessOff = "harness intentionally disabled"
	for _, hold := range []string{ghNone, bgStopCmd, holdStarts, harnessOff} {
		t.Run(hold, func(t *testing.T) {
			a, _ := stubApp(t)
			dir := a.store.Dir()
			path := filepath.Join(t.TempDir(), "config.yaml")
			extra := ""
			if hold == harnessOff {
				extra = "    - name: disabled-harness\n      probe: [disabled-probe]\n      harness: claude\n"
			}
			if err := os.WriteFile(path, []byte(fmt.Sprintf("stateDir: %q\nfatal:\n  checks:\n    - name: startup\n      probe: [critical-probe]\n", dir)+extra), 0600); err != nil {
				t.Fatal(err)
			}
			a.cfgPath = path
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				if hold == bgStopCmd {
					st.Stop = &state.Stop{Reason: "person stopped", At: time.Now()}
				}
				if hold == holdStarts {
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
				if args[0] == "disabled-probe" {
					return "", 2
				}
				if args[0] == fatalCriticalProbe && failed {
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
				if !slices.Contains(args, fatalBranchOption) || !slices.Contains(args, fatalDirOption) {
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
		if args[0] == fatalCriticalProbe {
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
			return fatalEnabledState, 0
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
		i := slices.Index(args, fatalNameOption)
		if i < 0 {
			t.Fatal("owner missing")
		}
		if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
			st.Agents = append(st.Agents, state.Agent{Party: state.Party{Name: args[i+1]}, Task: fatalRepairTask})
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

// The executable version is healthy even when startup's Codex TOML parser
// fails. An existing incident must stay open and Codex must be excluded.
func TestFatalCodexStartupFailureExcludesHarnessUntilVerified(t *testing.T) {
	a, _ := stubApp(t)
	dir := a.store.Dir()
	path := filepath.Join(t.TempDir(), "config.yaml")
	userConfig := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(userConfig, []byte("model = ["), 0600); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("stateDir: %q\ncodex:\n  configFile: %q\ncapacity:\n  providers:\n    codex: {command: [usage-probe]}\n    claude: {command: [usage-probe]}\n  choices:\n    - {harness: codex, model: test-codex}\n    - {harness: claude, model: test-claude}\nfatal: {repositories: []}\n", dir, userConfig)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	a.cfgPath = path
	if _, err := config.Load(path); err != nil {
		t.Fatal(err)
	}
	q := fatal.Queue{}
	q.Observe(fatal.Observation{ID: "harness:codex", Failed: true}, time.Now().Add(-time.Hour))
	if err := q.Save(dir); err != nil {
		t.Fatal(err)
	}
	preflights := 0
	fakeFatalRun(t, func(args []string) (string, int) {
		if slices.Contains(args, "is-enabled") {
			return fatalDisabledState, 1
		}
		if slices.Contains(args, "preflight") && args[len(args)-1] == config.CodexHarness {
			preflights++
			if a.fatalHarnessConfig(t.Context(), config.CodexHarness) != nil {
				return "", 1
			}
		}
		return "version succeeds", 0
	})
	old := beekeeperRun
	starts := 0
	beekeeperRun = func(_ context.Context, args ...string) (string, int, error) {
		starts++
		i := slices.Index(args, fatalExcludeOption)
		if i < 0 || args[i+1] != config.CodexHarness {
			t.Fatalf("broken harness not excluded: %v", args)
		}
		return "", 0, nil
	}
	t.Cleanup(func() { beekeeperRun = old })
	if err := a.fatalScan(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	q, err := fatal.Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if starts != 1 || preflights != 1 || !q.Incidents[0].Resolved.IsZero() {
		t.Fatalf("starts=%d preflights=%d queue=%+v", starts, preflights, q)
	}
	if err := os.WriteFile(userConfig, []byte("model = 'valid'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.fatalScan(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	q, err = fatal.Read(dir)
	if err != nil || q.Barrier("ordinary") != nil {
		t.Fatalf("verified recovery: %+v %v", q, err)
	}
}

func TestFatalMalformedConfigurationAdmitsOneRecoveryOwner(t *testing.T) {
	for _, hold := range []string{ghNone, bgStopCmd, holdStarts, holdGitHub, fatalCoreRepository, loopCapacity} {
		t.Run(hold, func(t *testing.T) {
			a, _ := stubApp(t)
			dir := a.store.Dir()
			path := filepath.Join(t.TempDir(), "config.yaml")
			ceiling := 1
			if hold == loopCapacity {
				ceiling = 0
			}
			valid := []byte(fmt.Sprintf("stateDir: %q\ncapacity: {ceiling: %d}\nfatal: {repositories: []}\n", dir, ceiling))
			if err := os.WriteFile(path, valid, 0600); err != nil {
				t.Fatal(err)
			}
			a.cfgPath = path
			fakeFatalRun(t, func(args []string) (string, int) {
				if slices.Contains(args, "is-enabled") {
					return fatalDisabledState, 1
				}
				return "", 0
			})
			// One healthy pass establishes the validated recovery settings.
			if err := a.fatalScan(t.Context(), dir); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("fatal: ["), 0600); err != nil {
				t.Fatal(err)
			}
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				if hold == bgStopCmd {
					st.Stop = &state.Stop{Reason: "configured stop", At: time.Now()}
				} else if hold != ghNone && hold != loopCapacity {
					st.Holds = []state.Hold{{Target: hold, Reason: "deliberate hold", At: time.Now()}}
				}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			starts := 0
			old := beekeeperRun
			beekeeperRun = func(ctx context.Context, args ...string) (string, int, error) {
				i := slices.Index(args, workerGitHubConfigOption)
				if i < 0 || args[i+1] == path {
					t.Fatalf("repair depends on malformed config: %v", args)
				}
				// The child loads the real snapshot through the normal config parser.
				child, _ := stubApp(t)
				child.cfgPath = args[i+1]
				if err := child.loadConfig(); err != nil {
					t.Fatal(err)
				}
				child.store = a.store
				if child.cfg.Capacity.Ceiling != ceiling {
					t.Fatal("capacity settings lost")
				}
				owner := args[slices.Index(args, fatalNameOption)+1]
				plat = platform.Stub()
				plat.Machine = roomyMachine{Machine: plat.Machine}
				if _, _, err := child.startGateWithOptions(ctx, owner, config.ClaudeHarness, "", false, false, nil, forWorker); err != nil {
					if hold != loopCapacity || Code(err) != ExitRefused {
						t.Fatalf("unexpected admission failure: %v", err)
					}
					return "", Code(err), nil
				}
				err := child.withKindAdmission(ctx, owner, func() error {
					starts++
					return child.store.Update(func(st *state.State) ([]state.Event, error) {
						st.Agents = append(st.Agents, state.Agent{Party: state.Party{Name: owner}, Task: fatalRepairTask})
						return nil, nil
					})
				})
				return "", 0, err
			}
			t.Cleanup(func() { beekeeperRun = old })
			for range 2 {
				if err := a.fatalScan(t.Context(), dir); err != nil {
					t.Fatal(err)
				}
			}
			// A repair worker inherits the snapshot path. Its scan must still
			// fail the original file, and must not launch a second owner.
			a.cfgPath = filepath.Join(dir, "fatal-recovery.yaml")
			if err := a.fatalScan(t.Context(), dir); err != nil {
				t.Fatal(err)
			}
			a.cfgPath = path
			want := 0
			if hold == ghNone {
				want = 1
			}
			if starts != want {
				t.Fatalf("started %d owners, want %d", starts, want)
			}
			q, err := fatal.Read(dir)
			if err != nil || q.Barrier("ordinary") == nil {
				t.Fatalf("configuration outage lost: %+v %v", q, err)
			}
			if err := os.WriteFile(path, valid, 0600); err != nil {
				t.Fatal(err)
			}
			if err := a.fatalScan(t.Context(), dir); err != nil {
				t.Fatal(err)
			}
			q, err = fatal.Read(dir)
			if err != nil || q.Barrier("ordinary") != nil {
				t.Fatalf("verified original config recovery did not clear: %+v %v", q, err)
			}
		})
	}
}

func TestFatalPreflightRejectsMalformedHarnessSettings(t *testing.T) {
	for _, harness := range []string{config.ClaudeHarness, config.AgyHarness, config.OmpHarness} {
		t.Run(harness, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
			a, _ := stubApp(t)
			path := filepath.Join(home, ".claude", "settings.json")
			if harness == config.AgyHarness {
				path = filepath.Join(home, ".gemini", "config", "hooks.json")
			}
			if harness == config.OmpHarness {
				path = filepath.Join(home, "models.yml")
				a.cfg.Omp.ModelsFile = path
			}
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("[malformed"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := a.fatalHarnessConfig(t.Context(), harness); err == nil {
				t.Fatal("malformed startup settings accepted")
			}
		})
	}
}
