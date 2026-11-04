package secret

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/gofrs/flock"
)

// ErrSOPSConflict means a destination changed or another beekeeper writer
// is committing it. The caller must reread it before retrying the edit.
var ErrSOPSConflict = errors.New("SOPS destination changed or is being written; reread it before retrying")

func (d *document) remember(file string, raw []byte, existed bool) error {
	abs, err := filepath.Abs(file)
	if err != nil {
		return err
	}
	d.source, d.original, d.existed = abs, slices.Clone(raw), existed
	return nil
}

// commit compares the original snapshot and replaces it under a stable
// cache process lock. The lock file stays in place so waiters cannot
// lock an unlinked inode. TryLock is one attempt, without a polling loop.
func (d *document) commit(ctx context.Context, file string, out []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		return err
	}
	lockPath, err := sopsWriteLockPath(abs)
	if err != nil {
		return err
	}
	lock := flock.New(lockPath)
	defer lock.Close() //nolint:errcheck // best-effort release also closes a failed attempt
	held, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !held {
		return fmt.Errorf("%s: %w", file, ErrSOPSConflict)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := os.ReadFile(file) //nolint:gosec // compare the caller's destination snapshot
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	exists := err == nil
	expectedExists := d.source == abs && d.existed
	if exists != expectedExists || exists && !bytes.Equal(current, d.original) {
		return fmt.Errorf("%s: %w", file, ErrSOPSConflict)
	}
	if err := writeFile(file, out); err != nil {
		return err
	}
	return d.remember(file, out, true)
}

// sopsWriteLockPath gives cooperating processes one stable cache lock for
// a canonical destination, leaving its repository directory unchanged.
func sopsWriteLockPath(abs string) (string, error) {
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cache, "beekeeper", "secret-write-locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte(filepath.Join(parent, filepath.Base(abs))))
	return filepath.Join(dir, fmt.Sprintf("%x.lock", hash)), nil
}
