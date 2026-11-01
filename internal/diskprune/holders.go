package diskprune

import (
	"io/fs"
	"path/filepath"
	"time"

	"github.com/giantswarm/beekeeper/internal/worktrees"
)

// Usage is what one walk of a holder finds: the space its files take, when
// anything in it was last written, and whether it holds a git checkout.
type Usage struct {
	Bytes    int64
	Written  time.Time
	Checkout bool
	// Unreadable says part of the holder could not be read: its size and
	// age are a lower bound.
	Unreadable bool
}

// Inspect walks dir without following symlinks, removing nothing.
func Inspect(dir string) Usage {
	var u Usage
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			u.Unreadable = true
			return nil
		}
		if d.Name() == checkoutMetadata {
			u.Checkout = true
		}
		fi, err := d.Info()
		if err != nil {
			u.Unreadable = true
			return nil
		}
		u.Bytes += worktrees.Allocated(fi)
		if fi.ModTime().After(u.Written) {
			u.Written = fi.ModTime()
		}
		return nil
	})
	return u
}
