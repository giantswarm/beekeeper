package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/fatal"
)

// fatalHarnessConfig runs in a timeout-bounded child of the sentinel. It uses
// startup's parsers/preparation without sending a prompt or contacting a model.
// The child emits only a generic failure, never configuration contents.
func (a *app) fatalHarnessConfig(ctx context.Context, harness string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	switch harness {
	case config.CodexHarness:
		// Exercise the same generated home and config merge as startCodexAgent,
		// without changing the home of any running worker or reading credentials.
		scratch, err := os.MkdirTemp(a.cfg.StateDir, "fatal-codex-preflight-")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(scratch) }()
		workerHome := filepath.Join(scratch, config.CodexHarness, "home")
		if err := codex.EnsureHome(workerHome, a.cfg.Codex.AuthFile, a.cfg.Codex.ConfigFile, "beekeeper", ""); err != nil {
			return err
		}
		// This command loads Codex's own config schema but never starts a turn.
		// Environment changes are confined to this disposable preflight child.
		previous, present := os.LookupEnv("CODEX_HOME")
		if err := os.Setenv("CODEX_HOME", workerHome); err != nil {
			return err
		}
		defer func() {
			if present {
				_ = os.Setenv("CODEX_HOME", previous)
			} else {
				_ = os.Unsetenv("CODEX_HOME")
			}
		}()
		return fatalNonModel(ctx, config.CodexHarness, "features", "list")
	case config.ClaudeHarness:
		dir := os.Getenv("CLAUDE_CONFIG_DIR")
		if dir == "" {
			dir = filepath.Join(home, ".claude")
		}
		for _, name := range []string{"settings.json", "settings.local.json"} {
			if err := fatalJSONConfig(filepath.Join(dir, name)); err != nil {
				return err
			}
		}
		return fatalNonModel(ctx, config.ClaudeHarness, "auth", "status")
	case config.AgyHarness:
		if err := fatalJSONConfig(filepath.Join(home, ".gemini", "config", "hooks.json")); err != nil {
			return err
		}
		if err := refuseUnguardedAgy(); err != nil {
			return err
		}
		_, err := agyModels(ctx, config.AgyHarness)
		return err
	case config.OmpHarness:
		// models performs the harness's own startup configuration loading, without
		// inference. Parse the configured models file as well (it can differ from
		// the CLI default); key values are neither logged nor handed to a model.
		raw, err := os.ReadFile(filepath.Clean(a.cfg.Omp.ModelsFile))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		var models map[string]any
		if err := yaml.Unmarshal(raw, &models); err != nil {
			return err
		}
		_, err = a.ompListed(ctx)
		return err
	default:
		return errors.New("unsupported repair harness")
	}
}

func fatalJSONConfig(path string) error {
	raw, err := os.ReadFile(filepath.Clean(path)) //nolint:gosec // operator-owned harness settings path, never a worker-supplied path
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		return err
	}
	if settings == nil {
		return errors.New("settings must be an object")
	}
	return nil
}

// All command output stays inside fatalProbe and is discarded.
func fatalNonModel(ctx context.Context, argv ...string) error {
	code, err := fatalProbe(ctx, argv...)
	if err != nil {
		return err
	}
	if code != 0 {
		return errors.New("non-model startup command failed")
	}
	return nil
}

// A successful version string alone cannot verify startup configuration.
func fatalHarness(ctx context.Context, path, harness string) fatal.Observation {
	code, err := fatalProbe(ctx, harness, "--version")
	if err == nil && code == 0 {
		var bin string
		bin, err = os.Executable()
		if err == nil {
			code, err = fatalProbe(ctx, bin, workerGitHubConfigOption, path, "fatal", "preflight", harness)
		}
	}
	o := fatal.Observation{ID: "harness:" + harness, Failed: err != nil || code != 0, Evidence: "configured harness fails non-model startup preflight"}
	if ctx.Err() != nil {
		o.Failed, o.Unknown = false, true
	}
	return o
}
