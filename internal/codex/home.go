package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Harness is Session.Harness and Start.Harness for a Codex CLI session.
const Harness = "codex"

// Comm is the Codex CLI executable name.
const Comm = "codex"

// Environment variables beekeeper sets on a codex agent's unit, which codex
// passes down to every tool shell and hook invocation.
const (
	EnvAgent = "BEEKEEPER_CODEX_AGENT"
	EnvName  = "BEEKEEPER_AGENT_NAME"
)

// HostPrefix is the prefix of a codex agent's HostSession on the roster:
// codex_<start id>.
const HostPrefix = "codex_"

// DefaultProjectDocMaxBytes is the worker config.toml cap for AGENTS.md docs.
const DefaultProjectDocMaxBytes = 262144

// HomeDir is the isolated CODEX_HOME directory for beekeeper-started codex workers.
func HomeDir(stateDir string) string {
	return filepath.Join(stateDir, "codex", "home")
}

// HomeSessionsDir is the sessions directory inside the isolated CODEX_HOME.
func HomeSessionsDir(stateDir string) string {
	return filepath.Join(HomeDir(stateDir), "sessions")
}

// ZshDir is the ZDOTDIR inside the isolated CODEX_HOME.
func ZshDir(codexHome string) string {
	return filepath.Join(codexHome, "zsh")
}

// DefaultAuthFile is the default location of the user's Codex OAuth file.
func DefaultAuthFile(userHome string) string {
	return filepath.Join(userHome, ".codex", "auth.json")
}

// DefaultUserConfigFile is the default location of the user's Codex config.toml.
func DefaultUserConfigFile(userHome string) string {
	return filepath.Join(userHome, ".codex", "config.toml")
}

// DefaultUserSessionsDir is the default location of the user's Codex sessions directory.
func DefaultUserSessionsDir(userHome string) string {
	return filepath.Join(userHome, ".codex", "sessions")
}

type shellEnvironmentPolicy struct {
	Inherit string `toml:"inherit"`
}

type workerFeatures struct {
	Hooks           bool `toml:"hooks"`
	DaemonAutoStart bool `toml:"daemon_auto_start"`
	Plugins         bool `toml:"plugins"`
	Apps            bool `toml:"apps"`
	MultiAgent      bool `toml:"multi_agent"`
	MultiAgentV2    bool `toml:"multi_agent_v2"`
	UnifiedExecTTY  bool `toml:"unified_exec_tty"`
}

type workerConfig struct {
	ProjectDocMaxBytes     int                    `toml:"project_doc_max_bytes"`
	ShellEnvironmentPolicy shellEnvironmentPolicy `toml:"shell_environment_policy"`
	Features               workerFeatures         `toml:"features"`
	Projects               map[string]any         `toml:"projects,omitempty"`
}

// MergeConfigTOML parses existing user config.toml bytes structurally, retains
// only the [projects.*] trust tables, and returns a clean worker config.toml
// with deterministic worker settings and feature flags.
func MergeConfigTOML(existing []byte) ([]byte, error) {
	var projects map[string]any
	if len(bytes.TrimSpace(existing)) > 0 {
		var raw map[string]any
		if err := toml.Unmarshal(existing, &raw); err != nil {
			return nil, fmt.Errorf("parsing existing codex config.toml: %w", err)
		}
		if p, ok := raw["projects"].(map[string]any); ok && len(p) > 0 {
			projects = make(map[string]any, len(p))
			for k, v := range p {
				if m, ok := v.(map[string]any); ok {
					projects[k] = m
				}
			}
			if len(projects) == 0 {
				projects = nil
			}
		}
	}
	cfg := workerConfig{
		ProjectDocMaxBytes: DefaultProjectDocMaxBytes,
		ShellEnvironmentPolicy: shellEnvironmentPolicy{
			Inherit: "all",
		},
		Features: workerFeatures{
			Hooks:           true,
			DaemonAutoStart: false,
			Plugins:         false,
			Apps:            false,
			MultiAgent:      false,
			MultiAgentV2:    false,
			UnifiedExecTTY:  false,
		},
		Projects: projects,
	}
	out, err := toml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshaling worker codex config.toml: %w", err)
	}
	return out, nil
}

type hooksFile struct {
	Hooks map[string][]hookMatcher `json:"hooks"`
}

type hookMatcher struct {
	Matcher string      `json:"matcher"`
	Hooks   []hookEntry `json:"hooks"`
}

type hookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// HooksJSON returns the hooks.json content for the isolated CODEX_HOME.
func HooksJSON(exe string) ([]byte, error) {
	bin := strings.TrimSpace(exe)
	if bin == "" {
		bin = "beekeeper"
	}
	hf := hooksFile{
		Hooks: map[string][]hookMatcher{
			"PreToolUse": {
				{
					Matcher: "*",
					Hooks: []hookEntry{
						{
							Type:    keyCommand,
							Command: bin + " hook pretooluse --format codex",
							Timeout: 30,
						},
					},
				},
			},
		},
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(hf); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// EnsureHome prepares an isolated CODEX_HOME directory for a beekeeper codex
// worker: symlinks authFile to auth.json when present, generates config.toml
// keeping only [projects.*] from userConfigFile, writes hooks.json, and writes
// zsh/.zshenv and zsh/.zprofile for non-interactive tool shells.
func EnsureHome(codexHome, authFile, userConfigFile, exe, prelude string) error {
	zshDir := ZshDir(codexHome)
	for _, dir := range []string{codexHome, filepath.Join(codexHome, "sessions"), zshDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	if authFile != "" {
		if _, err := os.Lstat(authFile); err == nil {
			dstAuth := filepath.Join(codexHome, "auth.json")
			if target, err := os.Readlink(dstAuth); err != nil || target != authFile {
				_ = os.Remove(dstAuth)
				if err := os.Symlink(authFile, dstAuth); err != nil {
					return fmt.Errorf("linking codex auth.json: %w", err)
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat codex auth.json: %w", err)
		}
	}
	var existing []byte
	if userConfigFile != "" {
		b, err := os.ReadFile(filepath.Clean(userConfigFile))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("reading user codex config.toml: %w", err)
		}
		existing = b
	}
	cfgBytes, err := MergeConfigTOML(existing)
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(codexHome, "config.toml"), cfgBytes); err != nil {
		return err
	}
	hooksBytes, err := HooksJSON(exe)
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(codexHome, "hooks.json"), hooksBytes); err != nil {
		return err
	}
	body := "setopt nonomatch 2>/dev/null || true\n" +
		"unalias grep find ls cp mv rm 2>/dev/null || true\n"
	if strings.TrimSpace(prelude) != "" {
		body = prelude
		if !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
	}
	zshenv := "_bk_zdotdir=\"$ZDOTDIR\"\n" +
		"ZDOTDIR=\"$HOME\"\n" +
		"[[ -f \"$HOME/.zshenv\" ]] && source \"$HOME/.zshenv\"\n" +
		body +
		"[[ -o login ]] && ZDOTDIR=\"$_bk_zdotdir\"\n" +
		"unset _bk_zdotdir\n"
	if err := writeAtomic(filepath.Join(zshDir, ".zshenv"), []byte(zshenv)); err != nil {
		return err
	}
	zprofile := "ZDOTDIR=\"$HOME\"\n" +
		"[[ -f \"$HOME/.zprofile\" ]] && source \"$HOME/.zprofile\"\n" +
		body
	return writeAtomic(filepath.Join(zshDir, ".zprofile"), []byte(zprofile))
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}
