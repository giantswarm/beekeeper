// Package install puts beekeeper in place on a machine and takes it away
// again: the Claude Code hooks, the platform's standby service and memory
// guard, and a starter config. A manifest in the state directory records
// what install wrote, so uninstall removes exactly that; what install finds
// in place and did not write, it keeps as it is.
package install

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/docs/examples"
	"github.com/giantswarm/beekeeper/internal/platform"
)

// Env is the machine install and uninstall act on.
type Env struct {
	// Exe is the absolute path of the binary the hooks and the standby
	// service run.
	Exe string
	// Settings is Claude Code's user settings file, Config beekeeper's
	// config file, StateDir its state directory (the manifest's).
	Settings, Config, StateDir string
	Setup                      platform.Setup
	Spec                       platform.SetupSpec
	// Run runs one service manager command.
	Run func(ctx context.Context, argv []string) error
	Out io.Writer
	// DryRun prints every step and takes none.
	DryRun bool
}

// Install writes the hooks, the standby service, the memory guard and a
// starter config where they are missing, and updates what an earlier
// install wrote; a second run changes nothing.
func Install(ctx context.Context, e Env) error {
	m, err := readManifest(e.manifest())
	if err != nil {
		return err
	}
	before := m.encode()
	p := &plan{home: e.Spec.Home}
	if err := e.planHooks(p, &m); err != nil {
		return err
	}
	e.planConfig(p, &m)
	e.planSetup(ctx, p, &m)
	m.addDirs(e.StateDir)
	if after := m.encode(); !bytes.Equal(before, after) {
		p.first("record", e.manifest(), "what install writes, for uninstall", func() error {
			return writeFile(e.manifest(), after)
		})
	}
	return p.run(e.Out, e.DryRun)
}

// Uninstall removes what install wrote and nothing else: a file changed
// since is kept. Config and state stay unless purge.
func Uninstall(ctx context.Context, e Env, purge bool) error {
	m, err := readManifest(e.manifest())
	if err != nil {
		return err
	}
	p := &plan{home: e.Spec.Home}
	if e.Setup.Available() {
		for _, svc := range m.Services {
			if e.Setup.Started(ctx, svc) {
				p.command(ctx, e, e.Setup.Stop(svc))
			}
		}
	}
	removed := map[string]bool{}
	for _, path := range slices.Sorted(maps.Keys(m.Files)) {
		cur, err := os.ReadFile(filepath.Clean(path))
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			p.add("keep", path, err.Error(), nil)
		case !m.owns(path, cur):
			p.add("keep", path, "changed since beekeeper install wrote it", nil)
		default:
			removed[path] = true
			p.add("remove", path, "", func() error { return os.Remove(path) })
		}
	}
	unitsGone := len(removed) > 0
	if err := e.planUnhooks(p, m, removed); err != nil {
		return err
	}
	remove := func(path, why string, rm func(string) error) {
		if exists(path) {
			removed[path] = true
			p.add("remove", path, why, func() error { return rm(path) })
		}
	}
	if purge {
		remove(e.Config, "--purge", os.Remove)
		remove(e.StateDir, "--purge: the state", os.RemoveAll)
	} else {
		remove(e.manifest(), "", os.Remove)
	}
	dirs := slices.Clone(m.Dirs)
	slices.SortFunc(dirs, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	for _, dir := range dirs {
		if !removed[dir] && empty(dir, removed) {
			remove(dir, "an empty directory install created", os.Remove)
		}
	}
	if unitsGone && e.Setup.Available() {
		p.command(ctx, e, e.Setup.Reload())
	}
	if len(p.steps) == 0 {
		p.add("ok", "nothing installed", "", nil)
	}
	return p.run(e.Out, e.DryRun)
}

func (e Env) manifest() string { return filepath.Join(e.StateDir, "install.json") }

// planConfig writes the starter config when no config exists.
func (e Env) planConfig(p *plan, m *Manifest) {
	if _, err := os.Stat(e.Config); !errors.Is(err, fs.ErrNotExist) {
		p.add("ok", e.Config, "a config exists", nil)
		return
	}
	m.addDirs(filepath.Dir(e.Config))
	content := Starter()
	p.add("write", e.Config, "the example config, every key commented out", func() error {
		return writeFile(e.Config, content)
	})
}

// Starter is the example config with every key commented out: the defaults
// apply until a key is set.
func Starter() []byte {
	var b strings.Builder
	b.WriteString("# Written by beekeeper install from the example configuration with every key\n" +
		"# commented out, so the defaults apply: uncomment and edit what the desk needs.\n#\n")
	for line := range strings.Lines(examples.Config) {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "#") {
			b.WriteString("# ")
		}
		b.WriteString(line)
	}
	return []byte(b.String())
}

// fileState is what install found at a file's path.
type fileState int

const (
	fileNew     fileState = iota // missing: written
	fileUpdated                  // written by an earlier install, now different: rewritten
	fileSame                     // already what install writes
	fileKept                     // different and not install's: kept
)

// planFile writes f where it is missing or install's own: written by an
// earlier install, or a unit that runs this binary (a copy of the shipped
// one, an install that predates the manifest).
func (e Env) planFile(p *plan, m *Manifest, f platform.File) fileState {
	cur, err := os.ReadFile(filepath.Clean(f.Path))
	state := fileNew
	switch {
	case err == nil && bytes.Equal(cur, f.Content):
		p.add("ok", f.Path, "", nil)
		return fileSame
	case err == nil && m.owns(f.Path, cur):
		state = fileUpdated
		p.add("update", f.Path, "", nil)
	case err == nil && bytes.Contains(cur, []byte(e.Exe)):
		state = fileUpdated
		p.add("update", f.Path, "an earlier unit of this binary", nil)
	case err == nil:
		p.add("keep", f.Path, "differs from what install writes, and install did not write it", nil)
		return fileKept
	case !errors.Is(err, fs.ErrNotExist):
		p.add("keep", f.Path, err.Error(), nil)
		return fileKept
	default:
		m.addDirs(filepath.Dir(f.Path))
		p.add("write", f.Path, "", nil)
	}
	m.Files[f.Path] = sum(f.Content)
	p.steps[len(p.steps)-1].do = func() error { return writeFile(f.Path, f.Content) }
	return state
}

// planSetup writes the standby service's, the memory guard's and the
// keeper's files and starts their units.
func (e Env) planSetup(ctx context.Context, p *plan, m *Manifest) {
	if !e.Setup.Available() {
		p.add("skip", platform.Unavailable("standby service"), "", nil)
		return
	}
	files, skipped := e.Setup.Files(e.Spec)
	written := false
	var units []platform.File
	states := map[string]fileState{}
	for _, f := range files {
		s := e.planFile(p, m, f)
		written = written || s == fileNew || s == fileUpdated
		if f.Service {
			units, states[f.Path] = append(units, f), s
		}
	}
	for _, s := range skipped {
		p.add("skip", s, "", nil)
	}
	if written {
		p.command(ctx, e, e.Setup.Reload())
	}
	for _, u := range units {
		e.planStart(ctx, p, m, u.Path, states[u.Path])
	}
}

// planStart starts the unit defined by the file at path, which install
// found in state, and restarts it when install updated it.
func (e Env) planStart(ctx context.Context, p *plan, m *Manifest, path string, state fileState) {
	name := filepath.Base(path)
	switch {
	case state == fileKept:
		p.add("keep", name, "its definition is not beekeeper install's", nil)
	case state != fileNew && e.Setup.Started(ctx, path):
		if state == fileUpdated {
			p.command(ctx, e, e.Setup.Stop(path))
			p.command(ctx, e, e.Setup.Start(path))
		} else {
			p.add("ok", name, "running", nil)
		}
	default:
		if !slices.Contains(m.Services, path) {
			m.Services = append(m.Services, path)
		}
		p.command(ctx, e, e.Setup.Start(path))
	}
}

// plan is the steps install or uninstall takes, printed as they run.
type plan struct {
	home  string
	steps []step
}

type step struct {
	verb, what, why string
	do              func() error
}

func (p *plan) add(verb, what, why string, do func() error) {
	p.steps = append(p.steps, step{verb, what, why, do})
}

func (p *plan) first(verb, what, why string, do func() error) {
	p.steps = slices.Insert(p.steps, 0, step{verb, what, why, do})
}

// command runs argv; nil is no command.
func (p *plan) command(ctx context.Context, e Env, argv []string) {
	if argv != nil {
		p.add("run", strings.Join(argv, " "), "", func() error { return e.Run(ctx, argv) })
	}
}

func (p *plan) run(out io.Writer, dry bool) error {
	if dry {
		_, _ = fmt.Fprintln(out, "dry run: nothing changes")
	}
	for _, s := range p.steps {
		line := fmt.Sprintf("%-7s %s", s.verb, p.tilde(s.what))
		if s.why != "" {
			line += ": " + s.why
		}
		_, _ = fmt.Fprintln(out, line)
		if !dry && s.do != nil {
			if err := s.do(); err != nil {
				return fmt.Errorf("%s %s: %w", s.verb, s.what, err)
			}
		}
	}
	return nil
}

// tilde shortens paths under the home.
func (p *plan) tilde(s string) string {
	if p.home == "" {
		return s
	}
	return strings.ReplaceAll(s, p.home+string(filepath.Separator), "~"+string(filepath.Separator))
}

// Manifest is what install wrote, read by the next install and uninstall.
type Manifest struct {
	// Files are the files install wrote and the SHA-256 of their content.
	Files map[string]string `json:"files,omitempty"`
	// Dirs are the directories install created.
	Dirs []string `json:"dirs,omitempty"`
	// Services are the definitions of the units install started: the
	// standby service, the keeper's timer.
	Services []string `json:"services,omitempty"`
	// Service is the standby service's definition as earlier installs
	// recorded it, read into Services.
	Service string `json:"service,omitempty"`
	// Hooks are the settings entries install added.
	Hooks []Hook `json:"hooks,omitempty"`
	// SettingsKeys are the settings keys install created ("hooks",
	// "hooks.PreToolUse"); SettingsCreated says it created the file.
	SettingsKeys    []string `json:"settingsKeys,omitempty"`
	SettingsCreated bool     `json:"settingsCreated,omitempty"`
}

func readManifest(path string) (Manifest, error) {
	m := Manifest{}
	raw, err := os.ReadFile(filepath.Clean(path))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return m, err
	default:
		if err := json.Unmarshal(raw, &m); err != nil {
			return m, fmt.Errorf("%s: %w", path, err)
		}
	}
	if m.Files == nil {
		m.Files = map[string]string{}
	}
	if m.Service != "" && !slices.Contains(m.Services, m.Service) {
		m.Services = append(m.Services, m.Service)
	}
	m.Service = ""
	return m, nil
}

func (m Manifest) encode() []byte {
	raw, _ := json.MarshalIndent(m, "", "  ")
	return append(raw, '\n')
}

// addDirs records the directories a write into dir creates.
func (m *Manifest) addDirs(dir string) {
	for _, d := range missingDirs(dir) {
		if !slices.Contains(m.Dirs, d) {
			m.Dirs = append(m.Dirs, d)
		}
	}
}

// owns reports whether install wrote the file at path with content cur.
func (m Manifest) owns(path string, cur []byte) bool {
	h, ok := m.Files[path]
	return ok && h == sum(cur)
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// missingDirs are the directories a write into dir creates, outermost first.
func missingDirs(dir string) []string {
	var out []string
	for !exists(dir) {
		out = append([]string{dir}, out...)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return out
}

// empty reports whether dir holds nothing once the paths in gone are gone.
func empty(dir string, gone map[string]bool) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, en := range entries {
		if !gone[filepath.Join(dir, en.Name())] {
			return false
		}
	}
	return true
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// writeFile replaces the file at path, through a symlink to its target,
// keeping its mode; a new file is 0644.
func writeFile(path string, content []byte) error {
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
	mode := fs.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
