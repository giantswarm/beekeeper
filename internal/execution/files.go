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

// cleanEnvironment retains user-manager plumbing and local authentication
// location only. It never inherits a caller's harness identity/provider keys.
func cleanEnvironment(configDir string) []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "HOME", "PATH", "USER", "LOGNAME", "LANG", "SHELL", "TERM", "DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "SSH_AUTH_SOCK", "SSL_CERT_FILE", "SSL_CERT_DIR":
			out = append(out, kv)
		default:
			if strings.HasPrefix(k, "LC_") {
				out = append(out, kv)
			}
		}
	}
	if configDir != "" {
		out = append(out, "CLAUDE_CONFIG_DIR="+configDir)
	}
	return out
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

func (s *Server) command(ctx context.Context, dir string, env []string, bin string, args ...string) ([]byte, error) {
	f := s.Command
	if f == nil {
		f = hostCommand
	}
	return f(ctx, dir, env, bin, args...)
}
