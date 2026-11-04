// Package worktreeowner reserves worktree paths for the sessions creating them.
package worktreeowner

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gofrs/flock"
)

// Check reserves a path before git worktree add, or checks the containing
// worktree before a commit/push. Reservations survive hook processes. A failed
// add may leave a reservation; it expires with its session, just like an owner.
func Check(stateDir, session, dir string, create bool, live func(string) (bool, error)) string {
	if session == "" {
		return ""
	}
	dir = canonical(dir)
	if !create {
		for {
			if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
				break
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				return ""
			}
			dir = parent
		}
	}
	sum := sha256.Sum256([]byte(dir))
	records := filepath.Join(stateDir, "worktree-owners")
	if err := os.MkdirAll(records, 0o700); err != nil {
		return unavailable
	}
	p := filepath.Join(records, hex.EncodeToString(sum[:]))
	lock := flock.New(p + ".lock")
	if err := lock.Lock(); err != nil {
		return unavailable
	}
	defer func() { _ = lock.Unlock() }()
	raw, err := os.ReadFile(filepath.Clean(p))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return unavailable
	}
	owner := strings.TrimSpace(string(raw))
	if owner != "" && owner != session {
		active, err := live(owner)
		if err != nil {
			return unavailable
		}
		if active {
			return fmt.Sprintf("Refused: this worktree is owned by live session %s. Use your own worktree or wait until that session ends.", owner)
		}
	}
	if create {
		// Never claim a pre-existing worktree on a failing add.
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return ""
		}
		if err := os.WriteFile(p, []byte(session+"\n"), 0o600); err != nil {
			return unavailable
		}
	}
	return ""
}

const unavailable = "Refused: worktree session ownership could not be checked."

func canonical(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	if parent := filepath.Dir(p); parent != p {
		return filepath.Join(canonical(parent), filepath.Base(p))
	}
	return p
}
