package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/giantswarm/go-selfupdate"
	selfupdatecosign "github.com/giantswarm/selfupdate-cosign"
)

const windowsOS = "windows"

var installVerified = selfupdatecosign.Install
var privilegedInstall = func(ctx context.Context, source, target, mode string) error {
	// Only the final filesystem operations need privilege. No shell input is
	// interpolated: source, target and mode are positional arguments.
	const script = `set -eu
source=$1
target=$2
mode=$3
dir=$(mktemp -d "$(dirname "$target")/.beekeeper.update-XXXXXXXX")
trap 'rm -f -- "$dir/beekeeper"; rmdir -- "$dir"' EXIT
install -m "$mode" -- "$source" "$dir/beekeeper"
mv -f -- "$dir/beekeeper" "$target"
`
	out, err := exec.CommandContext(ctx, "sudo", "-n", "--", "sh", "-c", script, "beekeeper-update", source, target, mode).CombinedOutput() //nolint:gosec // fixed installer script; verified paths are positional arguments
	if err != nil {
		return fmt.Errorf("privileged atomic install: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// installRelease normally writes beside the executable. A system installation
// may need sudo for that directory; download and verification still run as the
// update account, and sudo never prompts or downloads anything. When a canary
// is configured on ctx, the verified binary is staged and checked before it
// replaces the target executable.
func installRelease(ctx context.Context, up *selfupdate.Updater, rel *selfupdate.Release, path string) error {
	canary := optionsFrom(ctx).Canary
	version := "v" + strings.TrimPrefix(rel.Version(), "v")
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	mode := info.Mode().Perm()
	stageDir, err := os.MkdirTemp(filepath.Dir(target), ".beekeeper-update-*")
	if err == nil {
		defer func() { _ = os.RemoveAll(stageDir) }()
		staged := filepath.Join(stageDir, filepath.Base(target))
		if werr := os.WriteFile(staged, nil, mode); werr == nil {
			err = installVerified(ctx, up, rel, staged)
			if err == nil {
				if canary != nil {
					_ = os.Chmod(staged, mode|0o700)
					if err := canary(ctx, staged, version); err != nil {
						return err
					}
					_ = os.Chmod(staged, mode)
				}
				err = os.Rename(staged, target)
				if err == nil {
					return nil
				}
			}
		} else {
			err = fmt.Errorf("staging release: %w", werr)
		}
	}
	if !errors.Is(err, os.ErrPermission) || runtime.GOOS == windowsOS {
		return err
	}
	dir, err := os.MkdirTemp("", "beekeeper-install-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	staged := filepath.Join(dir, filepath.Base(target))
	if err := os.WriteFile(staged, nil, 0o600); err != nil {
		return err
	}
	if err := installVerified(ctx, up, rel, staged); err != nil {
		return fmt.Errorf("verifying staged release: %w", err)
	}
	if canary != nil {
		_ = os.Chmod(staged, mode|0o700)
		if err := canary(ctx, staged, version); err != nil {
			return err
		}
	}
	return privilegedInstall(ctx, staged, target, strconv.FormatUint(uint64(mode), 8))
}
