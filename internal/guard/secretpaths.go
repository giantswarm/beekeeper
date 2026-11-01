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
// the guarded file such a command exposes, "" for none. The Bash adapter
// refuses what they name; Codex runs the same decision after its own checks
// and refuses links and find actions in its segment check too.

// holds returns a guarded file at or below p, a directory or what a symlink
// at p points to, "" for none. Unlike under, the home directory and the root
// count: a link to either exposes every file below it.
func (f secretFiles) holds(p string) string {
	return f.holdsNamed(p, nil)
}

// holdsNamed is holds of the guarded files whose base name named accepts; a
// nil named accepts every name, and a glob's own pattern is always kept.
func (f secretFiles) holdsNamed(p string, named func(string) bool) string {
	keep := func(file string) bool {
		return named == nil || strings.ContainsAny(filepath.Base(file), "*?[") || named(filepath.Base(file))
	}
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
		if file := f.match(c); file != "" && keep(file) {
			return file
		}
		prefix := strings.TrimSuffix(c, string(filepath.Separator)) + string(filepath.Separator)
		for _, g := range f.globs {
			if !strings.HasPrefix(g, prefix) {
				continue
			}
			m, _ := filepath.Glob(g)
			if len(m) == 0 && !strings.ContainsAny(g, "*?[") {
				m = []string{g}
			}
			for _, file := range m {
				if keep(file) {
					return file
				}
			}
			if len(m) == 0 && keep(g) {
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
// delete or overwrite, which the delete guard judges, or echo the found path.
var findQuiet = map[string]bool{"shred": true, "unlink": true, "rmdir": true, echoCommand: true, printfCommand: true}

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

// findNamed returns what find's -name and -iname tests accept of a base name,
// nil when its expression has none or can accept a file they reject (-o,
// -or, a comma, a negation).
func findNamed(args []string) func(string) bool {
	var pats []string
	var fold []bool
	for i, a := range args {
		switch a {
		case "-o", "-or", ",", "!", "-not":
			return nil
		case "-name", "-iname":
			if i+1 < len(args) {
				pats = append(pats, args[i+1])
				fold = append(fold, a == "-iname")
			}
		}
	}
	if len(pats) == 0 {
		return nil
	}
	return func(name string) bool {
		for i, p := range pats {
			n := name
			if fold[i] {
				p, n = strings.ToLower(p), strings.ToLower(n)
			}
			if ok, err := filepath.Match(p, n); err != nil || !ok {
				return err != nil // a malformed pattern decides nothing
			}
		}
		return true
	}
}

// findSource returns the guarded file at or below a start path of a find,
// one its -name tests accept, when its actions run a reader or another
// command consumes the paths it prints (feeds).
func (f secretFiles) findSource(args, dirs []string, feeds bool) string {
	if !feeds && !findReads(args) {
		return ""
	}
	starts, _ := findRuns(args)
	named := findNamed(args)
	for _, d := range dirs {
		for _, s := range starts {
			if file := f.holdsNamed(wordPath(s, d), named); file != "" {
				return file
			}
		}
	}
	return ""
}

// pathPrinters are the metadata commands that print the paths they name (or
// their targets), which a command consuming their output may open.
var pathPrinters = map[string]bool{
	readlinkCmd: true, "realpath": true, findCmd: true, "ls": true, "namei": true, dirnameCmd: true,
	fileCommand: true, "lsof": true, "fuser": true, "getfacl": true, "wc": true, "du": true,
}

// printsPaths reports whether a metadata command prints the paths it names:
// stat does unless its format leaves out %n and %N.
func printsPaths(name string, args []string) bool {
	if name != statCommand {
		return pathPrinters[name]
	}
	for i, a := range args {
		format := ""
		switch {
		case (a == "-c" || a == formatFlag || a == "--printf") && i+1 < len(args):
			format = args[i+1]
		case strings.HasPrefix(a, "--format=") || strings.HasPrefix(a, "--printf="):
			_, format, _ = strings.Cut(a, "=")
		case strings.HasPrefix(a, "-c") && len(a) > 2:
			format = a[2:]
		default:
			continue
		}
		return strings.Contains(format, "%n") || strings.Contains(format, "%N")
	}
	return true
}

// pathConsumers take a substituted path as an argument without opening it.
var pathConsumers = map[string]bool{echoCommand: true, printfCommand: true, testCommand: true, "[": true, "[[": true, "basename": true, dirnameCmd: true}

// feedsOutput reports whether segment i's output reaches a command that may
// open the paths it prints: a later pipeline stage (whose safe sinks the
// caller honours), a process substitution, or a command substitution unless
// the command around it only prints or tests its words.
func feedsOutput(sc shellScan, segs []segment, i int) bool {
	sg := segs[i]
	switch sg.before {
	case "<(":
		return true
	case "$(", "`":
		if i == 0 {
			return true
		}
		words := shellWords(sc.plain[segs[i-1].start:segs[i-1].end])
		k := commandAt(words)
		if k >= len(words) || !pathConsumers[path.Base(words[k])] {
			return true
		}
	}
	return sg.after == "|"
}
