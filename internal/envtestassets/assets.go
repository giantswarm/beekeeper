// Package envtestassets provisions the control-plane binaries used by Go tests.
package envtestassets

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
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
	marker := filepath.Join(dir, "assets-"+version)
	if data, err := os.ReadFile(marker); err == nil && valid(string(data)) {
		return string(data), nil
	}
	cmd := exec.CommandContext(ctx, "go", "run", tool, "use", "--bin-dir", dir, "-p", "path", version)
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
	if err := os.WriteFile(marker, []byte(assets), 0600); err != nil {
		return "", err
	}
	return assets, nil
}

func valid(dir string) bool {
	if !filepath.IsAbs(dir) {
		return false
	}
	for _, name := range []string{"etcd", "kube-apiserver"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return false
		}
	}
	return true
}
