package execution

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/google/uuid"
)

func newID() string         { return uuid.NewString() }
func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func atomicFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".execution-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// Sync the rename as well as its contents. Windows cannot sync a
	// directory handle; execution hosts require a user-scoped supervisor.
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

func readRegular(path string, limit int64) ([]byte, error) {
	fi, err := os.Lstat(path) //nolint:gosec // path is derived from an owned execution or operator configuration
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > limit {
		return nil, errors.New("invalid execution file")
	}
	f, err := os.Open(filepath.Clean(path)) //nolint:gosec // validated owned execution file
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if len(b) > int(limit) {
		return nil, errors.New("execution file exceeds limit")
	}
	return b, err
}

func readJSON(path string, v any) error {
	b, err := readRegular(path, MaxWire)
	if err != nil {
		return err
	}
	return Decode(b, v)
}

func within(root, target string) bool {
	r, err := filepath.Rel(root, target)
	return err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) && !filepath.IsAbs(r)
}

// ownedDirectory rejects symlinked parents before creating any managed
// directory. Provisioning must not write through an operator path alias.
func ownedDirectory(p string) error {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == string(filepath.Separator) {
		return errors.New("managed directory must be an absolute clean non-root path")
	}
	ancestor := p
	for {
		real, err := filepath.EvalSymlinks(ancestor)
		if err == nil {
			if real != ancestor {
				return errors.New("managed directory resolves through a symlink")
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(ancestor) == ancestor {
			return errors.New("managed directory parent is unavailable")
		}
		ancestor = filepath.Dir(ancestor)
	}
	return os.MkdirAll(p, 0o700)
}

// managedBinary is how the embedded worker contract invokes the component
// (agents note, park and idle): by bare name through the harness PATH.
const managedBinary = "beekeeper"

// cleanEnvironment retains user-manager plumbing and local authentication
// location only. It never inherits a caller's harness identity/provider keys.
// The component argument is the policy-bound beekeeper executable: the
// service manager's PATH cannot be assumed to carry a per-user installation,
// so its directory is placed first, which lets the owned harness resolve the
// bare worker commands to exactly the executable the host policy pins.
func cleanEnvironment(configDir, component string) []string {
	dir := ""
	if component != "" {
		dir = filepath.Clean(filepath.Dir(component))
	}
	var out []string
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "PATH":
			if dir != "" {
				out = append(out, "PATH="+pathWithFirst(dir, v))
				continue
			}
			out = append(out, kv)
		case "HOME", "USER", "LOGNAME", "LANG", "SHELL", "TERM", "DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "SSH_AUTH_SOCK", "SSL_CERT_FILE", "SSL_CERT_DIR":
			out = append(out, kv)
		default:
			if strings.HasPrefix(k, "LC_") {
				out = append(out, kv)
			}
		}
	}
	if dir != "" && !slices.ContainsFunc(out, func(kv string) bool { return strings.HasPrefix(kv, "PATH=") }) {
		out = append(out, "PATH="+dir)
	}
	if configDir != "" {
		out = append(out, "CLAUDE_CONFIG_DIR="+configDir)
	}
	return out
}

// pathWithFirst places dir first on path, dropping any later duplicate so
// the policy-bound executable can never be shadowed by a stale entry.
func pathWithFirst(dir, path string) string {
	var rest []string
	for _, d := range filepath.SplitList(path) {
		if d != "" && filepath.Clean(d) != dir {
			rest = append(rest, d)
		}
	}
	return strings.Join(append([]string{dir}, rest...), string(filepath.ListSeparator))
}

type HostCommand func(context.Context, string, []string, string, ...string) ([]byte, error)

func hostCommand(ctx context.Context, dir string, env []string, bin string, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, bin, args...) //nolint:gosec // operator-pinned executable, typed arguments
	c.Dir, c.Env = dir, env
	out := &boundedBuffer{limit: MaxWire}
	c.Stdout, c.Stderr = out, io.Discard
	if err := c.Run(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// hostCombined is hostCommand with stderr kept: for status lines a harness
// prints there. Callers compare it and never return it.
func hostCombined(ctx context.Context, dir string, env []string, bin string, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, bin, args...) //nolint:gosec // operator-pinned executable, typed arguments
	c.Dir, c.Env = dir, env
	out := &boundedBuffer{limit: MaxWire}
	c.Stdout, c.Stderr = out, out
	if err := c.Run(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (s *Server) combined(ctx context.Context, dir string, env []string, bin string, args ...string) ([]byte, error) {
	f := s.Combined
	if f == nil {
		f = hostCombined
	}
	return f(ctx, dir, env, bin, args...)
}

func (s *Server) command(ctx context.Context, dir string, env []string, bin string, args ...string) ([]byte, error) {
	f := s.Command
	if f == nil {
		f = hostCommand
	}
	return f(ctx, dir, env, bin, args...)
}
