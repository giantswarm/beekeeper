package secret

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// parsePlainRef reads file://<absolute path>#<path> ([FileRef]), one value
// of a plain file on the host as a source, the path a dotted key of the
// file's JSON or YAML mapping or a line of its dotenv lines: a CLI's own
// credential file (the Codex CLI's auth.json, for one).
func parsePlainRef(s string) (Ref, error) {
	file, path, _ := strings.Cut(strings.TrimPrefix(s, FileRef), "#")
	if !filepath.IsAbs(file) || path == "" {
		return Ref{}, fmt.Errorf("%q: one value of a plain file is file://<absolute path>#<path>", s)
	}
	return Ref{Plain: filepath.Clean(file), Path: path}, nil
}

// readPlain reads the one value a file:// reference names, from a file
// under o.FileRoots, as named and with its symlinks resolved. The file is opened for
// reading only. A path outside the roots, a missing file or a path that
// reaches nothing fails in one line naming it, no content quoted.
func (o *Ops) readPlain(r Ref) (string, error) {
	ref := FileRef + r.Plain
	if len(o.FileRoots) == 0 {
		return "", fmt.Errorf("%s: no plain file is a source: no root is configured (secret.fileRoots)", ref)
	}
	outside := fmt.Errorf("%s: beekeeper reads a plain file only under the roots %q (secret.fileRoots)", ref, o.FileRoots)
	if !o.underFileRoot(r.Plain) {
		// refused before the file is looked at: no answer says what exists outside
		return "", outside
	}
	resolved, err := filepath.EvalSymlinks(r.Plain)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%s: no such file", ref)
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w", ref, err)
	}
	if !o.underFileRoot(resolved) {
		return "", outside
	}
	raw, err := os.ReadFile(resolved) //nolint:gosec // a file under the person's configured roots
	if err != nil {
		return "", fmt.Errorf("%s: %w", ref, err)
	}
	v, err := valueAt(raw, r.Path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ref, err)
	}
	return v, nil
}

// underFileRoot reports whether path, its symlinks resolved, lies under one
// of o.FileRoots, as configured or with their symlinks resolved.
func (o *Ops) underFileRoot(path string) bool {
	for _, root := range o.FileRoots {
		roots := []string{root}
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			roots = append(roots, resolved)
		}
		for _, root := range roots {
			rel, err := filepath.Rel(root, path)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}
