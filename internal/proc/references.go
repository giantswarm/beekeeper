package proc

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// References reads a process's files, mappings and temporary/build directory
// environment. An unreadable live process is an error: a pruner must keep
// candidates when it cannot establish whether that process uses them.
func References(pid int) ([]string, error) { return referencesAt(procRoot, pid) }

func referencesAt(root string, pid int) ([]string, error) {
	dir := filepath.Join(root, strconv.Itoa(pid))
	paths := held(root, pid)
	for _, name := range []string{"environ", "maps"} {
		raw, err := os.ReadFile(filepath.Clean(filepath.Join(dir, name)))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("process references unavailable: %w", err)
		}
		if name == "environ" {
			for _, e := range bytes.Split(raw, []byte{0}) {
				k, v, _ := strings.Cut(string(e), "=")
				switch k {
				case "TMPDIR", "GOTMPDIR", "CARGO_TARGET_DIR":
					if filepath.IsAbs(v) {
						paths = append(paths, filepath.Clean(v))
					}
				}
			}
		} else {
			for line := range strings.Lines(string(raw)) {
				if i := strings.Index(line, "/"); i >= 0 {
					paths = append(paths, strings.TrimSuffix(strings.TrimSpace(line[i:]), " (deleted)"))
				}
			}
		}
	}
	// Held tolerates disappearing processes, but a live process with an
	// unreadable fd directory must not look unused.
	if _, err := os.ReadDir(filepath.Join(dir, "fd")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return paths, nil
}
