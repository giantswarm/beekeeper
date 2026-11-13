// Package envtestassets provisions the control-plane binaries used by Go tests.
package envtestassets

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/proc"
)

// Match the versions used by make test-envtest.
const tool = "sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.25"
const version = "1.37.0"
const Env = "KUBEBUILDER_ASSETS"

// Ensure returns cached assets, installing them on first use. An explicit
// caller environment wins. The lock serializes downloads across concurrent runs.
func Ensure(ctx context.Context, stateDir string) (string, error) {
	if assets := os.Getenv(Env); assets != "" {
		return assets, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	dir, err := filepath.Abs(filepath.Join(stateDir, "envtest"))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	lock := flock.New(filepath.Join(dir, "setup.lock"))
	ok, err := lock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ctx.Err()
	}
	defer func() { _ = lock.Unlock() }()
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	const marker = "assets-" + version
	if f, err := root.Open(marker); err == nil {
		data, readErr := io.ReadAll(f)
		_ = f.Close()
		if readErr == nil && valid(string(data)) {
			return string(data), nil
		}
	}
	// Reuse setup-envtest's default store before attempting a download.
	if assets := defaultAssets(); valid(assets) {
		if err := root.WriteFile(marker, []byte(assets), 0600); err != nil {
			return "", err
		}
		return assets, nil
	}
	cmd := proc.CommandContext(ctx, "go", "run", tool, "use", "--bin-dir", dir, "-p", "path", version) // #nosec G204 -- fixed tool and flags; absolute state directory is one argument, never shell input
	// Provision independently of the target repository's module/workspace settings.
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("setup-envtest: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	assets := strings.TrimSpace(string(output))
	if !valid(assets) {
		return "", fmt.Errorf("setup-envtest returned an incomplete assets directory")
	}
	if err := root.WriteFile(marker, []byte(assets), 0600); err != nil {
		return "", err
	}
	return assets, nil
}

func valid(dir string) bool {
	if !filepath.IsAbs(dir) {
		return false
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return false
	}
	defer func() { _ = root.Close() }()
	for _, name := range []string{"etcd", "kube-apiserver"} {
		info, err := root.Stat(name)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return false
		}
	}
	return true
}

// defaultAssets follows setup-envtest's platform-specific default store.
func defaultAssets() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	var base string
	switch runtime.GOOS {
	case "windows":
		base = os.Getenv("LocalAppData")
		if base == "" {
			return ""
		}
	case "darwin":
		base = filepath.Join(home, "Library", "Application Support")
	default:
		base = os.Getenv("XDG_DATA_HOME")
		if base == "" {
			base = filepath.Join(home, ".local", "share")
		}
	}
	return filepath.Join(base, "kubebuilder-envtest", "k8s", version+"-"+runtime.GOOS+"-"+runtime.GOARCH)
}
