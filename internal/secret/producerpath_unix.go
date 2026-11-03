//go:build unix

package secret

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// producerDirs are the directories a producer is found in, in this order:
// the system's own, never the caller's PATH, where a session could put a
// wrapper of its own first.
var producerDirs = []string{"/usr/bin", "/bin", "/usr/local/bin"}

// trusted refuses a resolved producer a user other than root could have
// replaced.
var trusted = rootOwned

// UseProducersIn makes the producers resolve in dir alone, trusted
// whoever owns it, until restore runs: the place a test's fake az or
// openssl stands in. Nothing but a test calls it.
func UseProducersIn(dir string) (restore func()) {
	dirs, trust := producerDirs, trusted
	producerDirs, trusted = []string{dir}, func(string) error { return nil }
	return func() { producerDirs, trusted = dirs, trust }
}

// producerPath is the absolute path of the producer name: the first of
// [producerDirs] that holds it, symbolic links resolved, refused unless
// root owns the file and every directory above it and none of them is
// writable by a group or other users.
func producerPath(name string) (string, error) {
	for _, dir := range producerDirs {
		p, err := filepath.EvalSymlinks(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if err := trusted(p); err != nil {
			return "", fmt.Errorf("the producer %s: %w: nothing run", filepath.Join(dir, name), err)
		}
		return p, nil
	}
	return "", fmt.Errorf("the producer %s is in none of %q: nothing run", name, producerDirs)
}

// rootOwned refuses p, a regular executable file, unless it and every
// directory above it are owned by root and writable by no group or other
// user.
func rootOwned(p string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return errors.New("not an executable file")
	}
	for {
		if err := rootOnly(p, fi); err != nil {
			return err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return nil
		}
		p = parent
		if fi, err = os.Lstat(p); err != nil {
			return err
		}
	}
}

// rootOnly refuses p, whose information is fi, unless root owns it and no
// group or other user can write it.
func rootOnly(p string, fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case !ok || st.Uid != 0:
		return fmt.Errorf("%s is not owned by root", p)
	case fi.Mode().Perm()&0o022 != 0:
		return fmt.Errorf("%s is writable by a group or other users", p)
	}
	return nil
}
