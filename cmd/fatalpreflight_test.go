package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/fatal"
)

func TestFatalCodexStartupFailureExcludesHarnessUntilVerified(t *testing.T) {
	a, _ := stubApp(t)
	dir := a.store.Dir()
	path := filepath.Join(t.TempDir(), "config.yaml")
	userConfig := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(userConfig, []byte("model = ["), 0600); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("stateDir: %q\ncodex:\n  configFile: %q\n  authFile: %q\ncapacity:\n  providers:\n    codex: {command: [usage-probe]}\n    claude: {command: [usage-probe]}\n  choices:\n    - {harness: codex, model: test-codex}\n    - {harness: claude, model: test-claude}\nfatal: {repositories: []}\n", dir, userConfig, filepath.Join(t.TempDir(), "absent-auth.json"))
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
	old := BeekeeperRun
	starts := 0
	BeekeeperRun = func(_ context.Context, args ...string) (string, int, error) {
		starts++
		i := slices.Index(args, fatalExcludeOption)
		if i < 0 || args[i+1] != config.CodexHarness {
			t.Fatalf("broken harness not excluded: %v", args)
		}
		return "", 0, nil
	}
	t.Cleanup(func() { BeekeeperRun = old })
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

// A preflight killed at its deadline exits -1 without an error from the
// command runner; that timeout is unknown health, never a harness outage.
func TestFatalHarnessTimeoutIsUnknownNotFailed(t *testing.T) {
	oldRun, oldTimeout := fatalRun, fatalProbeTimeout
	t.Cleanup(func() { fatalRun, fatalProbeTimeout = oldRun, oldTimeout })
	fatalProbeTimeout = 50 * time.Millisecond
	fatalRun = func(ctx context.Context, stdin string, _ []string) (string, string, int, error) {
		return RunArgv(ctx, stdin, []string{"sleep", "5"})
	}
	o := fatalHarness(t.Context(), filepath.Join(t.TempDir(), "config.yaml"), config.AgyHarness)
	if o.Failed || !o.Unknown {
		t.Fatalf("timed-out harness preflight = %+v, want unknown and not failed", o)
	}
	fatalRun = func(context.Context, string, []string) (string, string, int, error) { return "", "", 1, nil }
	if o := fatalHarness(t.Context(), filepath.Join(t.TempDir(), "config.yaml"), config.AgyHarness); !o.Failed || o.Unknown {
		t.Fatalf("failing harness preflight = %+v, want failed", o)
	}
}
