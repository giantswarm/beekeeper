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

// journalError is an update that failed where the recovery journal holds
// something to inspect: a journal that disagrees with the installation, or
// the transaction it records (a probation to settle, a replacement or a
// rollback) refused or interrupted.
type journalError struct {
	journal string
	err     error
}

func (e *journalError) Error() string { return e.err.Error() }
func (e *journalError) Unwrap() error { return e.err }

// journalFailed is err, a failure of i's journal or of the transaction it
// records.
func (i *installation) journalFailed(err error) error {
	return &journalError{journal: i.journal(), err: err}
}

// installFailed says why updating path failed. A failure of the journal's
// transaction names the journal to inspect. Any other failure stopped before
// the replacement: the signature, the evidence, the canary or the comparison
// refused, the journal stays as it was, and a retry is safe.
func installFailed(path string, err error) error {
	var je *journalError
	if errors.As(err, &je) {
		return fmt.Errorf("updating %s failed (inspect the recovery journal %s before retrying): %w", path, je.journal, err)
	}
	return fmt.Errorf("updating %s failed, nothing was replaced (a retry is safe): %w", path, err)
}

// installRelease holds the recovery lock through verification, canary checks
// and durable replacement. Download, verification and the journal run as this
// account; an installation it cannot write takes sudo for the replacement alone.
// A failure of the journal or its transaction is a *journalError.
func installRelease(ctx context.Context, up *selfupdate.Updater, rel *selfupdate.Release, path string, source selfupdate.Source) error {
	i, err := openInstallation(ctx, path)
	if err != nil {
		return fmt.Errorf("opening durable installation: %w", err)
	}
	defer i.close()
	j, err := i.read()
	if err == nil {
		err = i.recover(j)
	}
	if err != nil {
		return i.journalFailed(err)
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
	if err := checkEvidence(ctx, source, rel, staged); err != nil {
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
	if err := i.commit(ctx, j, staged, version, verifiedDigest); err != nil {
		return i.journalFailed(err)
	}
	return nil
}

// commit is the transaction the journal records: it settles a pending
// probation and replaces the executable with staged, the verified candidate
// of version, or follows a coordinator back to the recovery target.
func (i *installation) commit(ctx context.Context, j *Journal, staged, version, verifiedDigest string) error {
	// A coordinator rollback is an explicit recovery request, not evidence
	// that this host's candidate passed probation. Only the identical verified
	// recovery release can end a watched probation this way. Verification of
	// the download and retained artifact both remain mandatory.
	if guard := optionsFrom(ctx).HostRollback; guard != nil && j != nil && j.Previous != nil && j.Previous.Version == version && (j.Pending() || j.RolledBack()) {
		if verifiedDigest != j.Previous.Digest {
			return errors.New("coordinator release differs from the verified recovery target")
		}
		return i.rollback(j, "execution host follows coordinator rollback", guard)
	}
	if err := i.settleProbation(ctx, optionsFrom(ctx).Health); err != nil {
		return err
	}
	return i.replace(staged, version)
}
