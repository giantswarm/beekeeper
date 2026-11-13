package proc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// LookTool is exec.LookPath, then the Go tool directories for a bare name
// PATH lacks: beside the running binary, $GOBIN, every $GOPATH/bin,
// ~/go/bin and ~/.go/bin, in that order. A systemd user unit, the sandbox
// broker for one, runs with the service manager's PATH, the distribution's
// default without the directory go install writes to, where kind and the
// other Go CLIs live beside beekeeper. A tool found nowhere fails naming
// the directories looked in; a name with a path separator is exec's alone.
func LookTool(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err == nil || filepath.Base(name) != name {
		return path, err
	}
	dirs := goToolDirs()
	for _, dir := range dirs {
		if p, serr := exec.LookPath(filepath.Join(dir, name)); serr == nil {
			return p, nil
		}
	}
	if len(dirs) == 0 {
		return "", err
	}
	return "", fmt.Errorf("%w, nor in %s", err, strings.Join(dirs, ", "))
}

// goToolDirs are the directories Go CLIs are installed to, each once.
func goToolDirs() []string {
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		dirs = append(dirs, filepath.Dir(exe))
	}
	if gobin := os.Getenv("GOBIN"); gobin != "" {
		dirs = append(dirs, gobin)
	}
	for _, gopath := range filepath.SplitList(os.Getenv("GOPATH")) {
		if gopath != "" {
			dirs = append(dirs, filepath.Join(gopath, "bin"))
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, "go", "bin"), filepath.Join(home, ".go", "bin"))
	}
	seen := make(map[string]bool, len(dirs))
	return slices.DeleteFunc(dirs, func(dir string) bool {
		dir = filepath.Clean(dir)
		if seen[dir] {
			return true
		}
		seen[dir] = true
		return false
	})
}
