package install

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestInstallRetriesFailedServiceChanges(t *testing.T) {
	const (
		refusedRestart = "refusal"
		canceledRun    = "cancellation"
	)
	for _, failure := range []string{reload, restart, refusedRestart, canceledRun} {
		t.Run(failure, func(t *testing.T) {
			f := &fakeSetup{}
			e, _ := env(t, t.TempDir(), f, f.run)
			if err := Install(context.Background(), e); err != nil {
				t.Fatal(err)
			}
			e.Exe, e.Spec.Exe = replacementExe, replacementExe
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e.Run = func(ctx context.Context, argv []string) error {
				if failure == canceledRun {
					cancel()
					return ctx.Err()
				}
				if argv[0] == failure {
					return errors.New("synthetic service manager failure")
				}
				return f.run(ctx, argv)
			}
			if failure == refusedRestart {
				e.BeforeRestart = func(context.Context, string) error { return errors.New("synthetic refusal") }
			}
			err := Install(ctx, e)
			if (failure == refusedRestart) != (err == nil) {
				t.Fatalf("unexpected first attempt result: %v", err)
			}
			e.Run, e.BeforeRestart, f.ran = f.run, nil, nil
			if err := Install(context.Background(), e); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(f.ran, ","), "restart notify.service") {
				t.Errorf("retry never applied the changed service: %v", f.ran)
			}
			if (failure == reload || failure == canceledRun) && !strings.Contains(strings.Join(f.ran, ","), reload) {
				t.Error("retry never reloaded the changed definitions")
			}
			f.ran = nil
			if err := Install(context.Background(), e); err != nil {
				t.Fatal(err)
			}
			if len(f.ran) != 0 {
				t.Errorf("successful recovery was not cleared: %v", f.ran)
			}
		})
	}
}

// A launchd restart unloads the old agent before bootstrapping the new one.
type splitRestartSetup struct{ fakeSetup }

func (f *splitRestartSetup) Restart(path string) [][]string {
	return [][]string{f.Halt(path), f.Start(path)}
}

func TestInstallRetriesPartiallyCompletedRestart(t *testing.T) {
	f := &splitRestartSetup{}
	e, _ := env(t, t.TempDir(), f, f.run)
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	e.Exe, e.Spec.Exe = replacementExe, replacementExe
	e.Run = func(ctx context.Context, argv []string) error {
		if argv[0] == start {
			return errors.New("synthetic bootstrap failure")
		}
		return f.run(ctx, argv)
	}
	if err := Install(context.Background(), e); err == nil {
		t.Fatal("bootstrap failure ignored")
	}
	if f.started {
		t.Fatal("fixture did not unload the old agent")
	}
	e.Run, f.ran = f.run, nil
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.ran, "start notify.service") {
		t.Fatalf("retry did not bootstrap the agent: %v", f.ran)
	}
	m, err := readManifest(e.manifest())
	if err != nil {
		t.Fatal(err)
	}
	if m.ReloadPending || len(m.RestartsPending) != 0 {
		t.Error("successful bootstrap left a pending action")
	}
}
