package remotebuild

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// archive snapshots git's tracked and nonignored untracked files, including
// uncommitted edits and symlinks, excluding deleted tracked files. It never
// traverses symlinks or includes repository administration/ignored credentials.
func archive(ctx context.Context, root, dir string) (_ *os.File, err error) {
	names, err := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output() //nolint:gosec // fixed executable and typed arguments; test commands use owned fixtures
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, "source-*.tar")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}()
	tw := tar.NewWriter(f)
	seen := map[string]bool{}
	for _, raw := range bytes.Split(names, []byte{0}) {
		name := string(raw)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		clean := filepath.Clean(name)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || clean == ".git" || strings.HasPrefix(clean, ".git/") {
			continue
		}
		path := filepath.Join(root, clean)
		fi, e := os.Lstat(path)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return nil, e
		}
		// Gitlinks (submodules) cannot be faithfully represented by ls-files.
		if fi.IsDir() {
			return nil, &os.PathError{Op: "archive submodule", Path: clean, Err: os.ErrInvalid}
		}
		link := ""
		if fi.Mode()&os.ModeSymlink != 0 {
			link, e = os.Readlink(path)
			if e != nil {
				return nil, e
			}
		}
		h, e := tar.FileInfoHeader(fi, link)
		if e != nil {
			return nil, e
		}
		h.Name = filepath.ToSlash(clean)
		if e = tw.WriteHeader(h); e != nil {
			return nil, e
		}
		if fi.Mode().IsRegular() {
			src, e := os.OpenInRoot(root, clean) //nolint:gosec // OpenInRoot prevents symlink traversal outside the Git worktree
			if e != nil {
				return nil, e
			}
			_, e = io.Copy(tw, src)
			closeErr := src.Close()
			if e != nil {
				return nil, e
			}
			if closeErr != nil {
				return nil, closeErr
			}
		}
	}
	if err = tw.Close(); err != nil {
		return nil, err
	}
	_, err = f.Seek(0, io.SeekStart)
	if err != nil {
		return nil, err
	}
	return f, nil
}
