package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/update"
	"github.com/giantswarm/beekeeper/pkg/project"
)

const (
	selfUpdateCheckFlag    = "--check"
	selfUpdateRollbackFlag = "--rollback"
	selfUpdateReasonFlag   = "--rollback-reason"
	selfUpdateReason       = "candidate unhealthy"
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

func TestSelfUpdateCanaryChecksLiveConfigAndSkipsFailingRelease(t *testing.T) {
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
	if len(evs) != 1 || !strings.Contains(evs[0].Detail, canaryFailedVersion) || !strings.Contains(evs[0].Detail, "budget --gate") {
		t.Fatalf("canary events = %+v", evs)
	}

	// Subsequent --check skips the failed release.
	root = New()
	root.SetArgs([]string{cfgSwitch, cfgPath, selfUpdateCmdName, selfUpdateCheckFlag})
	root.SetOut(&out)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil {
		t.Fatalf("self-update --check after skip = %v", err)
	}

	// Verify all four canary checks run with --config when healthy.
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
	}
	if !slices.EqualFunc(ranChecks, wantChecks, slices.Equal) {
		t.Errorf("canary checks = %v, want %v", ranChecks, wantChecks)
	}
}

func TestSelfUpdateRecoveryFlagsAreExclusiveAndRequireReason(t *testing.T) {
	for _, args := range [][]string{
		{selfUpdateRollbackFlag},
		{selfUpdateReasonFlag, selfUpdateReason},
		{selfUpdateCheckFlag, "--complete-probation"},
		{"--complete-probation", selfUpdateRollbackFlag, selfUpdateReasonFlag, selfUpdateReason},
		{selfUpdateCheckFlag, selfUpdateRollbackFlag, selfUpdateReasonFlag, selfUpdateReason},
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
	root.SetArgs([]string{selfUpdateCmdName, selfUpdateCheckFlag, "--version", tag})
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
