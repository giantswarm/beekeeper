package guard

import (
	"path/filepath"
	"strings"
)

// Reuse the folder guard's shell state for cd, assignments, loops and -C.
func (w *targetWalk) addWorktree(st *deleteState, dir, name string, raw []string) {
	if name != "git" {
		return
	}
	i := 0
	for i < len(raw) {
		a := strings.Join(shellWords(raw[i]), "")
		if a == "-C" && i+1 < len(raw) {
			ps, ok := w.resolve(st, dir, raw[i+1])
			if !ok || len(ps) != 1 {
				return
			}
			dir = ps[0]
			i += 2
		} else if a == "-c" && i+1 < len(raw) {
			i += 2
		} else if strings.HasPrefix(a, "-") {
			i++
		} else {
			break
		}
	}
	if i+1 >= len(raw) || strings.Join(shellWords(raw[i]), "") != "worktree" || strings.Join(shellWords(raw[i+1]), "") != "add" {
		return
	}
	i += 2
	for i < len(raw) {
		a := strings.Join(shellWords(raw[i]), "")
		if a == "--" {
			i++
			break
		}
		if !strings.HasPrefix(a, "-") {
			break
		}
		if a == "-b" || a == "-B" || a == "--reason" {
			i++
		}
		i++
	}
	if i >= len(raw) {
		return
	}
	ps, ok := w.resolve(st, dir, raw[i])
	if ok {
		for _, p := range ps {
			if filepath.IsAbs(p) {
				w.out = append(w.out, p)
			}
		}
	}
}
