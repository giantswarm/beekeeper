package guard

import (
	"path"
	"path/filepath"
	"strings"
)

// A command can reach a guarded file without naming it: a link to a
// directory that holds one, a relative link source resolved from the link's
// own directory, a find that runs a reader on what it finds, or a metadata
// command whose printed path another command consumes. These helpers name
// the guarded file such a command exposes, "" for none; both adapters refuse
// what they name.

// holds returns a guarded file at or below p, a directory or what a symlink
// at p points to, "" for none. Unlike under, the home directory and the root
// count: a link to either exposes every file below it.
func (f secretFiles) holds(p string) string {
	if p == "" {
		return ""
	}
	cands := []string{filepath.Clean(p)}
	if strings.ContainsAny(p, "*?[") {
		m, _ := filepath.Glob(p)
		cands = append(cands, m...)
	}
	for _, c := range cands {
		if real, err := filepath.EvalSymlinks(c); err == nil && real != c {
			cands = append(cands, real)
		}
	}
	for _, c := range cands {
		if file := f.match(c); file != "" {
			return file
		}
		prefix := strings.TrimSuffix(c, string(filepath.Separator)) + string(filepath.Separator)
		for _, g := range f.globs {
			if !strings.HasPrefix(g, prefix) {
				continue
			}
			if m, _ := filepath.Glob(g); len(m) > 0 {
				return m[0]
			}
			if !strings.ContainsAny(g, "*?[") {
				return g
			}
		}
	}
	return ""
}

// linkSource returns the guarded file a link made by ln's args exposes: a
// source that is a guarded file or a directory holding one, resolved from
// every working directory and, as the link itself resolves it, from the
// directory the link is created in.
func (f secretFiles) linkSource(args, dirs []string) string {
	var operands []string
	target, options := "", true
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case options && a == "--":
			options = false
		case options && (a == "-t" || a == "--target-directory") && i+1 < len(args):
			target = args[i+1]
			i++
		case options && strings.HasPrefix(a, "--target-directory="):
			target = strings.TrimPrefix(a, "--target-directory=")
		case options && (a == "-S" || a == "--suffix"):
			i++ // the backup suffix, no operand
		case options && strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "t"):
			// -st DIR or -tDIR: t takes the rest of the cluster or the next word.
			if rest := a[strings.Index(a, "t")+1:]; rest != "" {
				target = rest
			} else if i+1 < len(args) {
				target = args[i+1]
				i++
			}
		case options && strings.HasPrefix(a, "-") && a != "-":
		default:
			operands = append(operands, a)
		}
	}
	sources, dest := operands, ""
	switch {
	case target != "":
		dest = target + "/"
	case len(operands) > 1:
		sources, dest = operands[:len(operands)-1], operands[len(operands)-1]
	}
	for _, d := range dirs {
		linkDir := d
		if dest != "" {
			if p := wordPath(dest, d); p != "" {
				linkDir = filepath.Dir(p)
				if strings.HasSuffix(dest, "/") || isDir(p) {
					linkDir = p
				}
			}
		}
		for _, s := range sources {
			for _, base := range []string{d, linkDir} {
				if file := f.holds(wordPath(s, base)); file != "" {
					return file
				}
			}
		}
	}
	return ""
}

// findRuns returns find's start paths ("." when it names none) and the
// commands its -exec, -execdir, -ok and -okdir actions run.
func findRuns(args []string) (starts, runs []string) {
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "-H" || a == "-L" || a == "-P" || strings.HasPrefix(a, "-O") {
			i++
			continue
		}
		if a == "-D" {
			i += 2
			continue
		}
		break
	}
	for ; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") || a == "(" || a == "!" || a == ")" || a == "," {
			break
		}
		starts = append(starts, a)
	}
	if len(starts) == 0 {
		starts = []string{"."}
	}
	for ; i < len(args); i++ {
		switch args[i] {
		case "-exec", "-execdir", "-ok", "-okdir":
			if i+1 < len(args) {
				runs = append(runs, path.Base(args[i+1]))
				i++
			} else {
				runs = append(runs, "")
			}
		}
	}
	return starts, runs
}

// findQuiet are commands a find action runs that print no file content: they
// delete or overwrite, which the delete guard judges.
var findQuiet = map[string]bool{"shred": true, "unlink": true, "rmdir": true}

// findReads reports whether find's actions run a command that is neither a
// metadata command nor quiet. An action runs a program, never a function the
// command line defines, so no helper shadows a metadata command here.
func findReads(args []string) bool {
	_, runs := findRuns(args)
	for _, r := range runs {
		if !metaCmds[r] && !findQuiet[r] {
			return true
		}
	}
	return false
}

// findSource returns the guarded file at or below a start path of a find
// whose actions run a reader, or whose printed paths another command
// consumes when feeds.
func (f secretFiles) findSource(args, dirs []string, feeds bool) string {
	if !feeds && !findReads(args) {
		return ""
	}
	starts, _ := findRuns(args)
	for _, d := range dirs {
		for _, s := range starts {
			if file := f.holds(wordPath(s, d)); file != "" {
				return file
			}
		}
	}
	return ""
}

// printsPaths reports whether a metadata command prints the paths it names;
// beekeeper and omp handle secret files without printing them.
func printsPaths(name string) bool {
	return name != selfCmd && name != ompCommand
}

// feedsOutput reports whether segment i's output is consumed by another
// command: piped on, or inside a command or process substitution.
func feedsOutput(segs []segment, i int) bool {
	sg := segs[i]
	return sg.after == "|" || sg.before == "$(" || sg.before == "`" || sg.before == "<("
}
