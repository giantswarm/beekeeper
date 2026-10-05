// Package sandbox is the agent sandbox: the policy beekeeper renders into
// Claude Code's managed settings, which Anthropic's sandbox runtime enforces
// on every command of every session on the machine, and the allow lists
// beekeeper's PreToolUse hook holds the harness's own file tools to.
//
// The home directory is denied and only what a session needs is
// re-allowed, so a credential path nobody listed stays unreadable. Egress
// is an allow list, and the GitHub token is masked: commands see a
// placeholder, the sandbox proxy puts the real token into requests to
// GitHub only.
package sandbox

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/config"
)

// Env is set in every session the policy holds, for the hook to know it.
const Env = "BEEKEEPER_SANDBOX"

// Brokered is set, with Env, in a beekeeper secret call the broker runs on
// the host for a sandboxed session: the call holds its files to the
// policy's lists and asks no broker itself.
const Brokered = "BEEKEEPER_SANDBOX_BROKERED"

// DropIn is the policy's file in Claude Code's managed settings directory.
const DropIn = "beekeeper-sandbox.json"

// FileTools are the harness's tools that read or write files outside the
// sandbox, whose paths the hook holds to the policy.
var FileTools = []string{"Read", "Grep", "Glob", "Edit", "MultiEdit", "Write", "NotebookEdit"}

// ManagedDir is Claude Code's managed settings directory on this system.
func ManagedDir() string {
	if runtime.GOOS == "darwin" {
		return "/Library/Application Support/ClaudeCode"
	}
	return "/etc/claude-code"
}

// Path is where the policy is installed: a drop-in of the managed settings.
func Path() string { return filepath.Join(ManagedDir(), "managed-settings.d", DropIn) }

// Policy is what an agent session's commands and file tools may read, write
// and reach. Paths are absolute and clean.
type Policy struct {
	// Home is denied for reading, apart from Read and Write.
	Home string
	// Read are the paths under Home that may be read.
	Read []string
	// Write are the paths that may be written, and read.
	Write []string
	// Deny are paths inside Write that may be neither read nor written:
	// the value scanner's key and index in beekeeper's state.
	Deny []string
	// Harness is the harness's config directory, which commands never
	// write: Claude Code mounts it writable in the sandbox, so a command
	// could leave a file there the harness reads outside it. The file
	// tools write its memory and plans through ToolWrite.
	Harness string
	// ToolWrite are the paths only the harness's file tools may write:
	// the sessions' memory and plans, never a command.
	ToolWrite []string
	// Domains are the hosts commands reach.
	Domains []string
	// Mask are the environment variables commands see as a placeholder.
	Mask []config.SandboxMask
	// Exe is the beekeeper binary the policy's hook runs.
	Exe string
	// Vars are the environment variables the policy sets besides Env: a
	// symlinked config's target, which the sandbox mounts without the link.
	Vars map[string]string
}

// Paths are where beekeeper and its configuration live, and the build
// slots beekeeper run takes.
type Paths struct {
	Home, ConfigFile, StateDir, LeaseDir, SlotDir, Exe string
	// ScanDir is the value scanner's key and index, which no session reads.
	ScanDir string
}

// New is the policy for cfg: beekeeper's own paths and GitHub, and the
// configured ones.
func New(cfg config.Sandbox, e Paths) Policy {
	claude := os.Getenv("CLAUDE_CONFIG_DIR")
	if claude == "" {
		claude = filepath.Join(e.Home, ".claude")
	}
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		xdg = filepath.Join(e.Home, ".config")
	}
	p := Policy{
		Home: e.Home,
		Read: append([]string{
			filepath.Dir(e.Exe), filepath.Dir(e.ConfigFile),
			// the harness's state a session reads back: its transcripts and
			// tool results, plans, skills and plugins; never its credentials
			filepath.Join(claude, "projects"), filepath.Join(claude, "plans"),
			filepath.Join(claude, "skills"), filepath.Join(claude, "plugins"),
			filepath.Join(claude, "CLAUDE.md"),
			filepath.Join(e.Home, ".gitconfig"), filepath.Join(xdg, "git"),
		}, cfg.AllowRead...),
		Write:     append([]string{e.StateDir, e.LeaseDir, e.SlotDir}, cfg.AllowWrite...),
		Deny:      []string{e.ScanDir},
		Harness:   filepath.Clean(claude),
		ToolWrite: []string{filepath.Join(claude, "projects", "*", "memory"), filepath.Join(claude, "plans")},
		Domains:   append([]string{"github.com", "*.github.com", "*.githubusercontent.com"}, cfg.Domains...),
		Mask:      cfg.Mask,
		Exe:       e.Exe,
	}
	// the home directory is empty inside the sandbox but for the paths it
	// mounts, at their targets: a symlinked config is named by its target,
	// a symlinked config directory (git's ignore and attributes) as well
	p.Vars = map[string]string{Env: "1"}
	for name, path := range map[string]string{"BEEKEEPER_CONFIG": e.ConfigFile, "GIT_CONFIG_GLOBAL": filepath.Join(e.Home, ".gitconfig"), "XDG_CONFIG_HOME": xdg} {
		if r, err := filepath.EvalSymlinks(path); err == nil && r != filepath.Clean(path) {
			p.Vars[name] = r
		}
	}
	for _, ps := range []*[]string{&p.Read, &p.Write, &p.Deny, &p.ToolWrite} {
		*ps = slices.DeleteFunc(*ps, func(q string) bool { return q == "" })
		for i, q := range *ps {
			(*ps)[i] = filepath.Clean(q)
			// the sandbox mounts what a path resolves to: a symlinked
			// config or checkout is listed at its target too
			if r, err := filepath.EvalSymlinks(q); err == nil && r != (*ps)[i] {
				*ps = append(*ps, r)
			}
		}
		slices.Sort(*ps)
		*ps = slices.Compact(*ps)
	}
	return p
}

// Settings is the policy as Claude Code managed settings.
func (p Policy) Settings() map[string]any {
	envVars := make([]map[string]any, 0, len(p.Mask))
	for _, m := range p.Mask {
		envVars = append(envVars, map[string]any{"name": m.Name, "mode": "mask", "injectHosts": m.Hosts})
	}
	return map[string]any{
		"env": p.Vars,
		"sandbox": map[string]any{
			"enabled":                  true,
			"failIfUnavailable":        true,
			"allowUnsandboxedCommands": false,
			"autoAllowBashIfSandboxed": true,
			"filesystem": map[string]any{
				"denyRead":                  append([]string{p.setting(p.Home)}, p.settings(p.Deny)...),
				"denyWrite":                 append(p.settings(p.Deny), p.setting(p.Harness)),
				"allowRead":                 p.settings(p.Read),
				"allowWrite":                p.settings(p.Write),
				"allowManagedReadPathsOnly": true,
			},
			"network": map[string]any{
				"allowedDomains":          p.Domains,
				"allowManagedDomainsOnly": true,
				"strictAllowlist":         true,
				"tlsTerminate":            map[string]any{},
			},
			"credentials": map[string]any{"envVars": envVars},
		},
		"hooks": map[string]any{
			"PreToolUse": []map[string]any{{
				"matcher": strings.Join(FileTools, "|"),
				"hooks":   []map[string]any{{"type": "command", "command": p.Exe + " hook pretooluse", "timeout": 10}},
			}},
		},
	}
}

// JSON is Settings, indented, with a final newline.
func (p Policy) JSON() ([]byte, error) {
	b, err := json.MarshalIndent(p.Settings(), "", "  ")
	return append(b, '\n'), err
}

// setting writes a path in the settings' prefix syntax: ~/ under the home
// directory, // for an absolute one (a single / is relative to the
// settings file).
func (p Policy) setting(path string) string {
	if path == p.Home {
		return "~/"
	}
	if rel, ok := strings.CutPrefix(path, p.Home+string(filepath.Separator)); ok {
		return "~/" + rel
	}
	return "/" + path
}

func (p Policy) settings(paths []string) []string {
	out := make([]string, len(paths))
	for i, q := range paths {
		out[i] = p.setting(q)
	}
	return out
}

// Readable reports whether a file tool may read path: outside the home
// directory, or under a path of Read or Write, or writable.
func (p Policy) Readable(path, cwd string) bool {
	path = resolve(path, cwd)
	return !p.denied(path) && (p.listed(path) || p.Writable(path, cwd))
}

// Writable reports whether a file tool may write path: under a path of
// Write or ToolWrite, the temporary directory, or the session's working
// directory when that is itself readable (a checkout, never the home
// directory or a credential's).
func (p Policy) Writable(path, cwd string) bool {
	path = resolve(path, cwd)
	if p.denied(path) {
		return false
	}
	if under(path, resolve(os.TempDir(), "")) || slices.ContainsFunc(p.Write, func(w string) bool { return under(path, resolve(w, "")) }) {
		return true
	}
	if cwd != "" {
		if dir := resolve(cwd, ""); under(path, dir) && p.listed(dir) {
			return true
		}
	}
	return slices.ContainsFunc(p.ToolWrite, func(w string) bool { return underGlob(path, w) })
}

// denied reports whether the resolved path lies under a path of Deny,
// which holds inside every allow.
func (p Policy) denied(path string) bool {
	return slices.ContainsFunc(p.Deny, func(d string) bool { return under(path, resolve(d, "")) })
}

// listed reports whether the resolved path lies outside the home directory
// or under a path of Read or Write.
func (p Policy) listed(path string) bool {
	return !under(path, resolve(p.Home, "")) ||
		slices.ContainsFunc(append(slices.Clone(p.Read), p.Write...), func(r string) bool { return under(path, resolve(r, "")) })
}

// resolve is path made absolute against cwd with its symlinks resolved; a
// path that does not exist yet resolves through its nearest existing parent.
func resolve(path, cwd string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = home + path[1:]
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	path = filepath.Clean(path)
	var rest []string
	for d := path; ; d = filepath.Dir(d) {
		r, err := filepath.EvalSymlinks(d)
		if err == nil {
			return filepath.Join(append([]string{r}, rest...)...)
		}
		if !errors.Is(err, fs.ErrNotExist) || d == filepath.Dir(d) {
			return path
		}
		rest = append([]string{filepath.Base(d)}, rest...)
	}
}

// under reports whether path is dir or lies below it.
func under(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, strings.TrimSuffix(dir, string(filepath.Separator))+string(filepath.Separator))
}

// underGlob is under for a dir whose components may be globs.
func underGlob(path, dir string) bool {
	parts := strings.Split(dir, string(filepath.Separator))
	got := strings.Split(path, string(filepath.Separator))
	if len(got) < len(parts) {
		return false
	}
	ok, err := filepath.Match(dir, strings.Join(got[:len(parts)], string(filepath.Separator)))
	return ok && err == nil
}
