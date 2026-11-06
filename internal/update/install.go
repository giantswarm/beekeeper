package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/giantswarm/go-selfupdate"
	selfupdatecosign "github.com/giantswarm/selfupdate-cosign"
)

const windowsOS = "windows"

var installVerified = selfupdatecosign.Install

// privilegedInstall atomically replaces target with source for an installation
// this account cannot write. It is the only privileged step: the journal is
// durable before it and recovery reconciles either side of it. No shell input is
// interpolated: source, target and mode are positional arguments.
var privilegedInstall = func(ctx context.Context, source, target, mode string) error {
	const script = `set -eu
source=$1
target=$2
mode=$3
dir=$(mktemp -d "$(dirname "$target")/.beekeeper.update-XXXXXXXX")
trap 'rm -f -- "$dir/beekeeper"; rmdir -- "$dir"' EXIT
install -m "$mode" -- "$source" "$dir/beekeeper"
sync "$dir/beekeeper" 2>/dev/null || sync
mv -f -- "$dir/beekeeper" "$target"
`
	out, err := exec.CommandContext(ctx, "sudo", "-n", "--", "sh", "-c", script, "beekeeper-update", source, target, mode).CombinedOutput() //nolint:gosec // fixed command; the arguments are positional
	if err != nil {
		return fmt.Errorf("privileged atomic install: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// installRelease holds the recovery lock through verification, canary checks
// and durable replacement. Download, verification and the journal run as this
// account; an installation it cannot write takes sudo for the replacement alone.
func installRelease(ctx context.Context, up *selfupdate.Updater, rel *selfupdate.Release, path string) error {
	i, err := openInstallation(ctx, path)
	if err != nil {
		return fmt.Errorf("opening durable installation: %w", err)
	}
	defer i.close()
	j, err := i.read()
	if err != nil {
		return err
	}
	if err := i.recover(j); err != nil {
		return err
	}
	canary := optionsFrom(ctx).Canary
	version := "v" + strings.TrimPrefix(rel.Version(), "v")
	info, err := os.Stat(i.target)
	if err != nil {
		return err
	}
	mode := info.Mode().Perm()
	stageRoot := filepath.Dir(i.target)
	if i.external {
		stageRoot = i.dir
	}
	stageDir, err := os.MkdirTemp(stageRoot, ".beekeeper-update-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stageDir) }()
	staged := filepath.Join(stageDir, filepath.Base(i.target))
	if err := os.WriteFile(staged, nil, mode); err != nil {
		return err
	}
	if err := installVerified(ctx, up, rel, staged); err != nil {
		return err
	}
	verifiedDigest, err := digest(staged)
	if err != nil {
		return err
	}
	if canary != nil {
		if err := os.Chmod(staged, mode|0o700); err != nil {
			return err
		}
		if err := canary(ctx, staged, version); err != nil {
			return err
		}
	}
	if err := os.Chmod(staged, mode); err != nil {
		return err
	}
	afterCanary, err := digest(staged)
	if err != nil {
		return err
	}
	if afterCanary != verifiedDigest {
		return errors.New("verified candidate changed during canary checks")
	}
	if err := i.settleProbation(ctx, optionsFrom(ctx).Health); err != nil {
		return err
	}
	return i.replace(staged, version)
}
