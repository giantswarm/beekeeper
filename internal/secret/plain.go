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
// under o.FileRoots. The file is opened through an [os.Root] of the root it
// lies under, which resolves its symlinks beneath that root in the same
// open and refuses one leading out, so no swap between a check and the read
// reaches a file outside; it is opened for reading only. A path outside the
// roots, a missing file or a path that reaches nothing fails in one line
// naming it, no content quoted.
func (o *Ops) readPlain(r Ref) (string, error) {
	ref := FileRef + r.Plain
	if len(o.FileRoots) == 0 {
		return "", fmt.Errorf("%s: no plain file is a source: no root is configured (secret.fileRoots)", ref)
	}
	outside := fmt.Errorf("%s: beekeeper reads a plain file only under the roots %q (secret.fileRoots)", ref, o.FileRoots)
	root, rel := o.fileRoot(r.Plain)
	if root == "" {
		// refused before the file is looked at: no answer says what exists outside
		return "", outside
	}
	raw, err := readBeneath(root, rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("%s: no such file", ref)
	case errors.Is(err, errEscapes):
		return "", outside
	case err != nil:
		return "", fmt.Errorf("%s: %w", ref, err)
	}
	v, err := valueAt(raw, r.Path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ref, err)
	}
	return v, nil
}

// errEscapes marks a file whose path or symlinks lead out of its root.
var errEscapes = errors.New("escapes its root")

// readBeneath reads rel beneath root through an [os.Root], a path leading
// out of root errEscapes.
func readBeneath(root, rel string) ([]byte, error) {
	rt, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer rt.Close() //nolint:errcheck // read-only
	raw, err := rt.ReadFile(rel)
	if err != nil && !errors.Is(err, fs.ErrNotExist) && strings.Contains(err.Error(), "escapes") {
		return nil, errEscapes
	}
	return raw, err
}

// fileRoot is the root of o.FileRoots, as configured or with its symlinks
// resolved, that path lies under by name, and path relative to it; "" for
// none.
func (o *Ops) fileRoot(path string) (string, string) {
	for _, root := range o.FileRoots {
		roots := []string{root}
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			roots = append(roots, resolved)
		}
		for _, root := range roots {
			rel, err := filepath.Rel(root, path)
			if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return root, rel
			}
		}
	}
	return "", ""
}
