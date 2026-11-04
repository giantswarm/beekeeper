package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/giantswarm/go-selfupdate"
	selfupdatecosign "github.com/giantswarm/selfupdate-cosign"
)

const windowsOS = "windows"

var installVerified = selfupdatecosign.Install

// installRelease holds the recovery lock through verification, canary checks
// and durable replacement. Directory permission errors fail closed: the former
// sudo rename fallback could not preserve a durable recovery transaction.
func installRelease(ctx context.Context, up *selfupdate.Updater, rel *selfupdate.Release, path string) error {
	i, err := openInstallation(ctx, path)
	if err != nil {
		return fmt.Errorf("opening durable installation (directory must be writable): %w", err)
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
	stageDir, err := os.MkdirTemp(filepath.Dir(i.target), ".beekeeper-update-*")
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
	return i.replace(staged, version)
}
