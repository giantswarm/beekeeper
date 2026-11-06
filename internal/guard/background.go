package guard

import (
	"path/filepath"
	"regexp"
	"strings"
)

var backgroundRunCommand = regexp.MustCompile(`(?m)` + mergePos + `(["']?(?:[^\s=;&|()'" ]*/)?beekeeper["']?\s+run)(?:\s|$)`)

// backgroundRun recognizes runs and readable shell wrappers without executing
// them. Bound recursion so cyclic wrappers cannot stall the hook. Dynamic or
// unreadable wrappers need an explicit --wait.
func backgroundRun(cmd, cwd string, depth int) bool {
	if depth > 4 {
		return false
	}
	if commandMatch(backgroundRunCommand, cmd, notHelp) != nil {
		return true
	}
	sc := scanShell(cmd)
	vars := assignments(sc.plain)
	dir := cwd
	for _, seg := range sc.segments() {
		words := shellWords(sc.plain[seg.start:seg.end])
		c := commandAt(words)
		if c >= len(words) {
			continue
		}
		name, args := filepath.Base(words[c]), words[c+1:]
		if name == "cd" && len(args) == 1 {
			dir = wordPath(expandVars(args[0], vars), dir)
			continue
		}
		script := ""
		switch {
		case localShells[name]:
			ip := shellOptions
			ip.module = []string{"-n"}
			for _, a := range args {
				if isCFlag(a) {
					ip.code = append(ip.code, a)
				}
			}
			p := readProgram(args, ip)
			if p.kind == programCode && backgroundRun(p.arg, dir, depth+1) {
				return true
			}
			if p.kind == programScript {
				script = p.arg
			}
		case name == "source" || name == ".":
			if len(args) > 0 {
				script = args[0]
			}
		case strings.Contains(words[c], "/"):
			script = words[c]
		}
		if script == "" || dir == "" {
			continue
		}
		p := wordPath(expandVars(script, vars), dir)
		if p == "" {
			continue
		}
		if raw, state := readScript(p); state == scriptRead && shellScript(raw) && backgroundRun(string(raw), dir, depth+1) {
			return true
		}
	}
	return false
}
