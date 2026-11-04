package takeover

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// withSessionLock serializes short file transitions across hook and screen
// processes, without taking the global state lock. Lock files stay outside
// request folders and are never unlinked: replacing a locked inode would
// let two processes enter the same transition at once.
func withSessionLock(ctx context.Context, dir, session string, fn func() error) error {
	if err := checkName(session); err != nil {
		return err
	}
	folder := filepath.Join(dir, ".locks")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	lock := flock.New(filepath.Join(folder, session+".lock"))
	ok, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return fmt.Errorf("takeover: lock session: %w", err)
	}
	if !ok {
		return fmt.Errorf("takeover: session is busy")
	}
	defer func() { _ = lock.Unlock() }()
	return fn()
}
