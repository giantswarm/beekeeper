package guard

import (
	"path/filepath"
	"strings"
)

// Under reports whether any of paths (absolute; empty ones skipped) is one
// of dirs or lies below one: the directory part of the hooks' scope.
func Under(dirs []string, paths ...string) bool {
	for _, p := range paths {
		if p == "" {
			continue
		}
		p = filepath.Clean(p)
		for _, d := range dirs {
			if p == d || strings.HasPrefix(p, strings.TrimSuffix(d, string(filepath.Separator))+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}
