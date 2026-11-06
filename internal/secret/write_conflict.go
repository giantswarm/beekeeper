package secret

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
)

// ErrWriteConflict means the destination changed or another writer is committing.
// Callers must reread the document before retrying the operation.
var ErrWriteConflict = errors.New("SOPS write conflict: reread the destination and retry")

type fileSnapshot struct {
	exists bool
	hash   [sha256.Size]byte
}

func snapshotOf(raw []byte, exists bool) *fileSnapshot {
	return &fileSnapshot{exists: exists, hash: sha256.Sum256(raw)}
}

func readSnapshot(file string) (*fileSnapshot, error) {
	raw, err := os.ReadFile(file) //nolint:gosec // caller's destination
	if errors.Is(err, fs.ErrNotExist) {
		return &fileSnapshot{}, nil
	}
	if err != nil {
		return nil, err
	}
	return snapshotOf(raw, true), nil
}

// Keep persistent lock inodes outside the destination tree. Removing a lock
// file after unlocking could let processes lock different inodes for one path.
func writeLock(file string) (*flock.Flock, error) {
	abs, err := filepath.Abs(file)
	if err != nil {
		return nil, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return nil, err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(cache, "beekeeper", "sops-write-locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	key := sha256.Sum256([]byte(filepath.Join(parent, filepath.Base(abs))))
	return flock.New(filepath.Join(dir, fmt.Sprintf("%x.lock", key))), nil
}

func commitEncrypted(file string, out []byte, expected *fileSnapshot) error {
	lock, err := writeLock(file)
	if err != nil {
		return err
	}
	held, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !held {
		return ErrWriteConflict
	}
	defer lock.Unlock() //nolint:errcheck // release on every exit
	current, err := readSnapshot(file)
	if err != nil {
		return err
	}
	if *current != *expected {
		return ErrWriteConflict
	}
	return writeFile(file, out)
}
