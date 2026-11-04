package guard

import (
	"path/filepath"
	"strings"
)

func (h Hook) checkWorktreeOwner(args []string, dir string, known bool) string {
	if h.WorktreeOwner == nil || h.mergeSession == "" || len(args) == 0 {
		return ""
	}
	create := len(args) > 1 && args[0] == "worktree" && args[1] == "add"
	if !create && args[0] != "commit" && args[0] != "push" {
		return ""
	}
	if !known {
		return "Refused: the guard cannot determine the worktree owner: run git from an explicit worktree directory."
	}
	if create {
		// git worktree add options that consume an argument; the first operand is the path.
		rest := false
		found := false
		for i := 2; i < len(args); i++ {
			a := args[i]
			if !rest && a == "--" {
				rest = true
				continue
			}
			if !rest && (a == "-b" || a == "-B" || a == "--reason") {
				i++
				continue
			}
			if !rest && strings.HasPrefix(a, "-") {
				continue
			}
			dir = absIn(dir, a)
			found = true
			break
		}
		if !found {
			return ""
		}
	}
	return h.WorktreeOwner(h.mergeSession, filepath.Clean(dir), create)
}
