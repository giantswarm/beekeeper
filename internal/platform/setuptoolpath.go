package platform

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// toolPath retains absolute search directories from the installing shell.
// Empty and relative entries depend on a working directory the service
// does not share. Standard installation directories also cover installs
// run by a service or another environment with a minimal PATH.
func (s SetupSpec) toolPath() string {
	var dirs []string
	for _, dir := range append(filepath.SplitList(s.ToolPath), filepath.Dir(s.Exe), "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin") {
		if filepath.IsAbs(dir) && !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	return strings.Join(dirs, string(os.PathListSeparator))
}
