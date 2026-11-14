package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/loopworkflow"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/update"
	"github.com/giantswarm/beekeeper/pkg/project"
)

const (
	selfUpdateCompleteFlag  = "--complete-probation"
	selfUpdateJudgeFlag     = "--" + recoverOutdatedJudgeFlag
	selfUpdateInstalledPath = "/tmp/installed"
	selfUpdateCheckFlag     = "--check"
	selfUpdateRollbackFlag  = "--rollback"
	selfUpdateReasonFlag    = "--rollback-reason"
	selfUpdateReason        = "candidate unhealthy"
	selfUpdateStageFlag     = "--stage-canary"
	selfUpdateVersionArg    = "--" + hostVersionFlag
)

// An update names the beekeeper processes it leaves on the old binary, pid
// and command each, every watch with the command that re-arms it, and says
// nothing when there are none.
func TestLeftBehind(t *testing.T) {
	stale := []*proc.Process{
		{PID: 3, Comm: project.Name, Args: strings.Fields("beekeeper watch --standby")},
		{PID: 5, Comm: project.Name, Args: strings.Fields("beekeeper agents reopen")},
		{PID: 7, Comm: project.Name, Args: strings.Fields("beekeeper gate -- devctl pr merge o/r 1")},
		{PID: 9, Comm: project.Name, Args: strings.Fields("beekeeper merge-child /s/merges/o_r-1")},
		{PID: 11, Comm: project.Name, Args: strings.Fields("/home/u/.go/bin/beekeeper guide watch")},
		{PID: 13, Comm: project.Name, Args: strings.Fields("beekeeper alerts watch --interval 5m")},
	}
	want := "Left on v0.107.0 until restarted, their saves of the state refused once v0.107.1 writes it:\n" +
		"  pid 3: beekeeper watch --standby (a watch: stop it, then run `beekeeper watch --standby`)\n" +
		"  pid 5: beekeeper agents reopen\n" +
		"  pid 7: beekeeper gate -- (re-executes v0.107.1 at its next step)\n" +
		"  pid 9: beekeeper merge-child /s/merges/o_r-1 (re-executes v0.107.1 at its next step)\n" +
		"  pid 11: beekeeper guide watch (a watch: stop it, then run `beekeeper guide watch`)\n" +
		"  pid 13: beekeeper alerts watch --interval (a watch: stop it, then run `beekeeper alerts watch --interval` with its values)\n"
	if got := leftBehind(stale, "v0.107.0", "v0.107.1"); got != want {
		t.Errorf("left behind:\n%s\nwant:\n%s", got, want)
	}
	if got := leftBehind(nil, "v0.107.0", "v0.107.1"); got != "" {
		t.Errorf("none left behind said %q", got)
	}
}

func TestSelfUpdateCanaryChecksLiveConfigAndRetriesBudgetFailure(t *testing.T) {
	const (
		cfgSwitch            = "--" + configFlag
		canaryCurrentVersion = "v1.4.0"
		canaryFailedVersion  = "v1.5.0"
		stagedCanaryPath     = "/tmp/staged-beekeeper"
	)
	dir := t.TempDir()
	cfgPath := emptyTestConfig(t, filepath.Join(dir, "config.yaml"))
	t.Setenv("XDG_STATE_HOME", dir)

	prevUpdate, prevCanary := runUpdate, runCanaryCheck
	t.Cleanup(func() {
		runUpdate = prevUpdate
		runCanaryCheck = prevCanary
	})

	gateArgs := append([]string{cfgSwitch, cfgPath}, budgetGateArgs...)
	var ranChecks [][]string
	failGate := true
	runCanaryCheck = func(_ context.Context, path string, args ...string) (string, error) {
		if path != stagedCanaryPath {
			t.Fatalf("canary path = %q", path)
		}
		ranChecks = append(ranChecks, slices.Clone(args))
		if failGate && slices.Equal(args, gateArgs) {
			return "GitHub budget under the floor (app metadata unreadable)", errors.New("exit status 3")
		}
		return "ok", nil
	}

	installed := false
	runUpdate = func(ctx context.Context, _ io.Writer, check bool) (update.Result, error) {
		opts := update.OptionsFrom(ctx)
		if opts.Skip != nil && opts.Skip(canaryFailedVersion) {
			return update.Result{Current: canaryCurrentVersion, Latest: canaryCurrentVersion}, nil
		}
		if check {
			return update.Result{Current: canaryCurrentVersion, Latest: canaryFailedVersion, Newer: true}, update.ErrOutdated
		}
		if opts.Canary == nil {
			t.Fatal("self-update did not attach a canary to ctx")
		}
		if err := opts.Canary(ctx, stagedCanaryPath, canaryFailedVersion); err != nil {
			return update.Result{Current: canaryCurrentVersion, Latest: canaryFailedVersion, Newer: true}, err
		}
		installed = true
		return update.Result{Current: canaryCurrentVersion, Latest: canaryFailedVersion, Newer: true, Updated: true, Path: "/bin/beekeeper"}, nil
	}

	var out bytes.Buffer
	root := New()
	root.SetArgs([]string{cfgSwitch, cfgPath, selfUpdateCmdName})
	root.SetOut(&out)
	root.SetErr(io.Discard)
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "budget --gate") || installed {
		t.Fatalf("self-update on failing canary = %v, installed=%v", err, installed)
	}

	store, err := state.Open(filepath.Join(dir, "beekeeper"))
	if err != nil {
		t.Fatal(err)
	}
	evs, err := store.Events(0, func(e state.Event) bool { return e.Verb == verbSelfUpdateCanary })
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 0 {
		t.Fatalf("canary events = %+v", evs)
	}

	// Budget failures leave the candidate discoverable and retryable.
	root = New()
	root.SetArgs([]string{cfgSwitch, cfgPath, selfUpdateCmdName, selfUpdateCheckFlag})
	root.SetOut(&out)
	root.SetErr(io.Discard)
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), canaryFailedVersion+" is newer") {
		t.Fatalf("self-update --check after budget failure = %v", err)
	}

	// Verify the four live canary checks run with --config when healthy, and
	// the isolated workflow probe last, without the live configuration.
	ranChecks = nil
	failGate = false
	a := &app{cfgPath: cfgPath, store: store}
	if err := a.canaryRelease(context.Background(), &out, stagedCanaryPath, "v1.5.1"); err != nil {
		t.Fatalf("canaryRelease on healthy release = %v", err)
	}
	wantChecks := [][]string{
		{cfgSwitch, cfgPath, "version"},
		gateArgs,
		{cfgSwitch, cfgPath, supervisorName, executionStatus},
		{cfgSwitch, cfgPath, "capacity"},
		{selfUpdateCmdName, installationProbeArg},
	}
	if !slices.EqualFunc(ranChecks, wantChecks, slices.Equal) {
		t.Errorf("canary checks = %v, want %v", ranChecks, wantChecks)
	}
}

// The update hands the health check to the install, and a failed one is not a
// skipped release: the installed candidate is judged, not the one on offer.
func TestSelfUpdateChecksTheInstalledCandidate(t *testing.T) {
	prev, prevCheck := runUpdate, runCanaryCheck
	t.Cleanup(func() { runUpdate, runCanaryCheck = prev, prevCheck })
	runCanaryCheck = func(context.Context, string, ...string) (string, error) { return "no answer", errors.New("exit 1") }
	var health error
	runUpdate = func(ctx context.Context, _ io.Writer, _ bool) (update.Result, error) {
		health = update.OptionsFrom(ctx).Health(ctx, "/installed", "v1.2.3")
		return update.Result{}, nil
	}
	root := New()
	root.SetArgs([]string{selfUpdateCmdName, selfUpdateCheckFlag})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if health == nil || !strings.Contains(health.Error(), "no answer") {
		t.Fatalf("health = %v", health)
	}
}

func TestSelfUpdateRecoveryFlagsAreExclusiveAndRequireReason(t *testing.T) {
	for _, args := range [][]string{
		{selfUpdateRollbackFlag},
		{selfUpdateReasonFlag, selfUpdateReason},
		{selfUpdateCheckFlag, selfUpdateCompleteFlag},
		{selfUpdateJudgeFlag, selfUpdateInstalledPath, selfUpdateCheckFlag},
		{selfUpdateJudgeFlag, selfUpdateInstalledPath, selfUpdateCompleteFlag},
		{selfUpdateJudgeFlag, selfUpdateInstalledPath, selfUpdateVersionArg, "v0.4.0"},
		{selfUpdateCompleteFlag, selfUpdateRollbackFlag, selfUpdateReasonFlag, selfUpdateReason},
		{selfUpdateCheckFlag, selfUpdateRollbackFlag, selfUpdateReasonFlag, selfUpdateReason},
		{selfUpdateCheckFlag, selfUpdateStageFlag, "/tmp/candidate"},
		{selfUpdateVersionArg, "v0.4.0", selfUpdateStageFlag, "/tmp/candidate"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			root := New()
			root.SetArgs(append([]string{selfUpdateCmdName}, args...))
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			if err := root.Execute(); err == nil {
				t.Fatal("accepted unsafe flag combination")
			}
		})
	}
}

func TestSelfUpdateWarnsConfigCompatibility(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfg, []byte("watch:\n  interval: future-format\n"), 0600); err != nil {
		t.Fatal(err)
	}
	previous := runUpdate
	t.Cleanup(func() { runUpdate = previous })
	var out bytes.Buffer
	stopped := errors.New("stop before installation")
	called := false
	runUpdate = func(_ context.Context, _ io.Writer, _ bool) (update.Result, error) {
		called = true
		if !strings.Contains(out.String(), "installed binary cannot read") || !strings.Contains(out.String(), "watch.interval") {
			t.Fatalf("missing warning before update: %s", out.String())
		}
		return update.Result{}, stopped
	}
	root := New()
	root.SetArgs([]string{"--" + configFlag, cfg, selfUpdateCmdName})
	root.SetOut(&out)
	root.SetErr(io.Discard)
	if err := root.Execute(); !errors.Is(err, stopped) || !called {
		t.Fatalf("update callback not reached: %v", err)
	}
}

func TestSelfUpdateVersionFlag(t *testing.T) {
	prev := runUpdate
	t.Cleanup(func() { runUpdate = prev })
	const tag = "v0.125.0-rc.3"
	called := false
	runUpdate = func(ctx context.Context, _ io.Writer, check bool) (update.Result, error) {
		called = true
		if !check || update.OptionsFrom(ctx).Version != tag {
			t.Fatalf("check=%v options=%+v", check, update.OptionsFrom(ctx))
		}
		return update.Result{Current: tag, Latest: tag}, nil
	}
	root := New()
	root.SetArgs([]string{selfUpdateCmdName, selfUpdateCheckFlag, selfUpdateVersionArg, tag})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("update not called")
	}
}

// Global flag values and later arguments must not be mistaken for commands.
func TestReexecs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args string
		want bool
	}{
		{"no arguments", "", false},
		{"program only", "beekeeper", false},
		{"gate wrapper", "beekeeper gate -- devctl pr merge o/r 1", true},
		{"merge child", "beekeeper merge-child /state/merge", true},
		{"config gate", "beekeeper --config /config.yaml gate -- devctl pr merge o/r 1", true},
		{"config merge child", "beekeeper --config /config.yaml merge-child /state/merge", true},
		{"inline config", "beekeeper --config=/config.yaml gate", true},
		{"multiple globals", "beekeeper --json --as worker --config /config.yaml merge-child /state/merge", true},
		{"gate config value", "beekeeper --config gate version", false},
		{"merge child identity value", "beekeeper --as merge-child version", false},
		{"later gate", "beekeeper version gate", false},
		{"separator", "beekeeper -- gate", false},
		{"missing config value", "beekeeper --config", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := reexecs(&proc.Process{Args: strings.Fields(tc.args)}); got != tc.want {
				t.Errorf("reexecs(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// An older updater invokes the candidate's unchanged supervisor status command.
// A stopped loop must let the remaining checks run; broken state must not.
func TestSelfUpdateCanaryWithStoppedSupervisor(t *testing.T) {
	a, out := supervisorApp(t)
	prev := runCanaryCheck
	t.Cleanup(func() { runCanaryCheck = prev })
	for _, json := range []bool{false, true} {
		a.json = json
		out.Reset()
		capacityRan := false
		runCanaryCheck = func(ctx context.Context, _ string, args ...string) (string, error) {
			if slices.Equal(args, []string{supervisorName, executionStatus}) {
				err := a.supervisorStatus(ctx)
				return out.String(), err
			}
			if slices.Equal(args, []string{loop.LoopCapacity}) {
				capacityRan = true
			}
			return "ok", nil
		}
		if why := canaryFailure(context.Background(), "", "candidate"); why != "" || !capacityRan {
			t.Fatalf("stopped loop (json=%v): failure=%q capacity ran=%v", json, why, capacityRan)
		}
		want := "the supervisor loop does not run"
		if json {
			want = `"loop": false`
		}
		if !strings.Contains(out.String(), want) {
			t.Fatalf("status output = %q, want %q", out.String(), want)
		}
	}
	if err := a.store.WriteFile(loop.LoopPIDFile, "invalid PID"); err != nil {
		t.Fatal(err)
	}
	if why := canaryFailure(context.Background(), "", "candidate"); !strings.Contains(why, "canary supervisor status failed") {
		t.Fatalf("broken supervisor status passed canary: %q", why)
	}
}

func TestSelfUpdateStageCanaryFlag(t *testing.T) {
	prev := runStageCanary
	t.Cleanup(func() { runStageCanary = prev })
	dir := t.TempDir()
	outPath := filepath.Join(dir, "candidate.canary.json")
	var gotCfg loopworkflow.CanaryConfig
	runStageCanary = func(_ context.Context, cfg loopworkflow.CanaryConfig) (update.CanaryEvidence, error) {
		gotCfg = cfg
		return update.CanaryEvidence{
			Schema:      1,
			Version:     "v0.99.0-rc.1",
			Commit:      "0123456789abcdef0123456789abcdef01234567",
			SHA256:      strings.Repeat("a", 64),
			OS:          "canary-os",
			Arch:        "canary-arch",
			Status:      update.CanaryStatusSuccess,
			Run:         cfg.Run,
			MergeCommit: strings.Repeat("b", 40),
		}, nil
	}
	var out bytes.Buffer
	root := New()
	root.SetArgs([]string{
		selfUpdateCmdName,
		selfUpdateStageFlag, "/staged/beekeeper",
		"--canary-recorded",
		"--canary-run", "run-123",
		"--canary-output", outPath,
	})
	root.SetOut(&out)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if gotCfg.Candidate != "/staged/beekeeper" || !gotCfg.Recorded || gotCfg.Run != "run-123" {
		t.Fatalf("StageCanary config = %+v", gotCfg)
	}
	written, err := os.ReadFile(outPath) //nolint:gosec // test output path
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), `"status": "success"`) || !strings.Contains(out.String(), `"run": "run-123"`) {
		t.Fatalf("unexpected canary output:\nfile=%s\nstdout=%s", written, out.String())
	}
}

// Live checks can be unavailable without diagnosing a defective candidate.
func TestProbationHealthClassifiesCanaryFailures(t *testing.T) {
	prev := runCanaryCheck
	t.Cleanup(func() { runCanaryCheck = prev })
	for _, check := range canaryChecks {
		name := strings.Join(check, " ")
		t.Run(name, func(t *testing.T) {
			runCanaryCheck = func(_ context.Context, _ string, args ...string) (string, error) {
				if slices.Equal(args, check) {
					return "check unavailable", errors.New("exit status 1")
				}
				return "ok", nil
			}
			err := probationHealth(context.Background(), "", "candidate")
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("lost failed check: %v", err)
			}
			if got, want := errors.Is(err, update.ErrHealthUnproven), name != hostVersionFlag; got != want {
				t.Fatalf("unproven = %v, want %v: %v", got, want, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runCanaryCheck = func(ctx context.Context, _ string, _ ...string) (string, error) {
		return "", ctx.Err()
	}
	if err := probationHealth(ctx, "", "candidate"); !errors.Is(err, update.ErrHealthUnproven) {
		t.Fatalf("cancellation diagnosed a broken candidate: %v", err)
	}
	runCanaryCheck = func(context.Context, string, ...string) (string, error) { return "ok", nil }
	if err := probationHealth(context.Background(), "", "candidate"); err != nil {
		t.Fatalf("healthy candidate: %v", err)
	}
}

// A loaded host slows every binary: a check of the candidate that runs out of
// its minute is run by the running release, within five minutes, and then by
// the candidate again, within twice the running release's time and a minute
// at least. The candidate passes and the update says why it waited; a fixed
// minute refused it (https://github.com/giantswarm/beekeeper/issues/3852).
func TestCanaryRunsACheckThatRanOutOfTimeAgain(t *testing.T) {
	running, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	prev := runCanaryCheck
	t.Cleanup(func() { runCanaryCheck = prev })
	status := []string{supervisorName, executionStatus}
	var runs []string
	var within []time.Duration
	runCanaryCheck = func(ctx context.Context, path string, args ...string) (string, error) {
		if !slices.Equal(args, status) {
			return "ok", nil
		}
		runs = append(runs, path)
		if at, ok := ctx.Deadline(); ok {
			within = append(within, time.Until(at))
		}
		if len(runs) == 1 {
			return "", fmt.Errorf("%w: signal: killed", context.DeadlineExceeded)
		}
		return "ok", nil
	}
	var out bytes.Buffer
	if err := (&app{}).canaryRelease(context.Background(), &out, testCandidateBinary, "v1.5.0"); err != nil {
		t.Fatalf("a candidate that ran out of time on a slow check was refused: %v\n%s", err, out.String())
	}
	if want := []string{testCandidateBinary, running, testCandidateBinary}; !slices.Equal(runs, want) {
		t.Fatalf("supervisor status ran on %v, want %v", runs, want)
	}
	near := func(got, want time.Duration) bool { return got > want-5*time.Second && got <= want }
	if len(within) != 3 || !near(within[0], time.Minute) || !near(within[1], 5*time.Minute) || !near(within[2], time.Minute) {
		t.Fatalf("deadlines %v, want a minute, five minutes and a minute", within)
	}
	if want := "Canary supervisor status ran out of its 1m0s deadline; the running release's same check took "; !strings.Contains(out.String(), want) {
		t.Fatalf("output %q, want %q", out.String(), want)
	}
}

// A version probe that runs out of its minute while the running release's own
// probe does not pass either judges the host, not the candidate: probation
// stays pending and the release is not skipped. A candidate that runs out of
// time again where the running release answered fails the probe as a broken
// one did before: it is skipped and its probation fails.
func TestCanaryTimeoutOnABusyHostJudgesTheHost(t *testing.T) {
	running, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &app{store: store}
	prev := runCanaryCheck
	t.Cleanup(func() { runCanaryCheck = prev })
	runningAnswers := false
	runCanaryCheck = func(_ context.Context, path string, args ...string) (string, error) {
		if !slices.Equal(args, []string{hostVersionFlag}) {
			return "ok", nil
		}
		if path == running && runningAnswers {
			return "beekeeper v1.4.0", nil
		}
		return "", fmt.Errorf("%w: signal: killed", context.DeadlineExceeded)
	}

	err = probationHealth(context.Background(), "", testCandidateBinary)
	if !errors.Is(err, update.ErrHealthUnproven) || !strings.Contains(err.Error(), "the host is too busy") {
		t.Fatalf("busy host: probation = %v, want unproven, the host too busy", err)
	}
	if err := a.canaryRelease(context.Background(), io.Discard, testCandidateBinary, "v1.5.0"); err == nil {
		t.Fatal("busy host: the candidate passed a check nothing passed")
	}
	if a.isSkippedRelease("v1.5.0") {
		t.Fatal("busy host: the release was skipped")
	}

	runningAnswers = true
	err = probationHealth(context.Background(), "", testCandidateBinary)
	if err == nil || errors.Is(err, update.ErrHealthUnproven) || !strings.Contains(err.Error(), "on its second run") {
		t.Fatalf("hung candidate: probation = %v, want a failed version probe", err)
	}
	if err := a.canaryRelease(context.Background(), io.Discard, testCandidateBinary, "v1.5.1"); err == nil || !a.isSkippedRelease("v1.5.1") {
		t.Fatalf("hung candidate: canary = %v, skipped = %v", err, a.isSkippedRelease("v1.5.1"))
	}
}
