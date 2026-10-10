package guard

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
)

// A blocking devctl command the gate rewrite does not reach runs outside the
// gate: one in a script file the call runs, in an interpreter's code, or in
// the program a shell or an interpreter reads from its input (a pipe, a
// here-string, a here-document, a redirected file). The hook refuses such a
// call, naming the place and the line, so the merge runs as its own Bash
// command line or with the gate written in.

// sep separates the words of a command as code names it: blanks, or the
// quotes and commas of a list.
const sep = `[\s"',]+`

const (
	bashCmd = "bash"
	zshCmd  = "zsh"
)

var (
	// looseOwned is a blocking devctl command as code names it.
	looseOwned = regexp.MustCompile(`(?:(?:~|\.\.?|\$\{?\w+\}?)?/(?:[^\s;&|()'"<>=]*/)?)?devctl` + sep +
		`(?:pr` + sep + `(?:merge|wait)|release` + sep + `(?:wait|promote)|rollout` + sep + `wait)\b`)
	// looseGated: the gate ends the text before the command, in code too.
	looseGated = regexp.MustCompile(`\bgate(?:` + sep + `--(?:wait|limit)` + sep + `[\w.]+)*` + sep + `--` + sep + `$`)
	// shebang names the interpreter of a script's first line.
	shebang = regexp.MustCompile(`^#!\s*(?:\S*/env\s+(?:-S\s+)?)?(?:\S*/)?(\w+)`)
	// assignment is a literal variable assignment on a command line.
	assignment = regexp.MustCompile(`(?m)(?:^|[;&|(]\s*|\bexport\s+)(\w+)=("[^"]*"|'[^']*'|[^\s;&|()]*)`)
	// pathVar is a variable reference in a path.
	pathVar = regexp.MustCompile(`\$\{(\w+)\}|\$(\w+)`)
	// localShells read a script file, a -c string or their input as a
	// command line.
	localShells = map[string]bool{"sh": true, bashCmd: true, zshCmd: true, "dash": true, "ksh": true}
	// nodeCode are node's code options; runSub the subcommand a script
	// follows (deno run, bun run).
	nodeCode = []string{"-e", "--eval", "-p", "--print"}
	runSub   = []string{"run"}
	// interpreters run a script file, the code behind one of their options,
	// or the program on their input.
	interpreters = map[string]interpreter{
		"python":  {code: []string{"-c"}},
		"python2": {code: []string{"-c"}},
		"python3": {code: []string{"-c"}},
		"node":    {code: nodeCode},
		"nodejs":  {code: nodeCode},
		"bun":     {code: nodeCode, sub: runSub},
		"deno":    {code: []string{"eval"}, sub: runSub},
		"perl":    {code: []string{"-e", "-E"}},
		"ruby":    {code: []string{"-e"}},
		"php":     {code: []string{"-r"}},
		"lua":     {code: []string{"-e"}},
	}
	// valueOptions are the options of shells and interpreters that take a
	// value, so that the value is not taken for the script.
	valueOptions = map[string]bool{"-o": true, "-m": true, "-W": true, "-X": true, "-r": true, "--require": true, "-I": true, "-M": true}
	// redirection is a redirection word; bare, its target is the next word.
	redirection  = regexp.MustCompile(`^\d*[<>&]`)
	bareRedirect = regexp.MustCompile(`^\d*(?:<<<|[<>]|>>|>\||[<>]&|&>)$`)
)

type interpreter struct {
	code []string // the options whose argument is the program
	sub  []string // the subcommands a script follows (deno run)
}

// scriptMode says how a script's text is read: as a shell command line, as
// code, or as its shebang says (a shell's or none: a command line).
type scriptMode int

const (
	modeShell scriptMode = iota
	modeCode
	modeShebang
)

// ungated is a blocking devctl command the gate rewrite does not reach.
type ungated struct {
	what  string // the command: devctl pr merge
	where string // the script /x/m.sh, python3's code (-c), the input bash reads
	line  int
	text  string // the line, trimmed
	fixed string // the line with the gate written in; "" where it has no place
}

func (u *ungated) reason() string {
	s := fmt.Sprintf("Refused: a %s in %s runs outside the gate (line %d: %s). Run it as its own Bash command line",
		u.what, u.where, u.line, u.text)
	if u.fixed == "" {
		return s + "."
	}
	return s + ", or with the gate written in: " + u.fixed
}

// ungatedRefusal returns why the call runs a blocking devctl command outside
// the gate, "" when it runs none.
func (h Hook) ungatedRefusal(cmd, cwd string) string {
	if u := h.ungated(cmd, cwd, 0); u != nil {
		return u.reason()
	}
	return ""
}

// ungated finds the first blocking devctl command the call runs outside the
// gate: in a script it runs, in an interpreter's code or on a shell's or an
// interpreter's input. A script the call cannot read before it runs (one it
// writes first) has the call's own text read instead.
func (h Hook) ungated(cmd, cwd string, depth int) *ungated {
	if depth > 4 {
		return nil
	}
	sc := scanShell(cmd)
	segs := sc.segments()
	var dirs []string
	if cwd != "" {
		dirs = []string{cwd}
		for _, m := range cdArg.FindAllStringSubmatch(sc.plain, -1) {
			dirs = append(dirs, within(cwd, m[1]))
		}
	}
	vars := assignments(sc.plain)
	unread := ""
	for i := range segs {
		u, missing := h.segmentUngated(cmd, sc, segs, i, dirs, vars, depth)
		if u != nil {
			return u
		}
		if unread == "" {
			unread = missing
		}
	}
	if unread != "" {
		return h.textUngated(cmd, "this call, which runs "+unread+" (not readable before the call)", false)
	}
	return nil
}

// segmentUngated finds what the simple command segs[i] runs outside the gate;
// missing names a script it runs that cannot be read before the call.
func (h Hook) segmentUngated(cmd string, sc shellScan, segs []segment, i int, dirs []string, vars map[string]string, depth int) (*ungated, string) {
	words := shellWords(sc.plain[segs[i].start:segs[i].end])
	c := commandAt(words)
	if c == len(words) {
		return nil, ""
	}
	name, args := path.Base(words[c]), words[c+1:]
	ip, interp := interpreters[name]
	switch {
	case name == "source" || name == ".":
		if len(args) == 0 {
			return nil, ""
		}
		return h.scriptUngated(args[0], modeShell, true, dirs, vars, depth)
	case localShells[name]:
		if slices.ContainsFunc(args, isCFlag) || slices.Contains(args, "-n") {
			return nil, "" // hiddenMerges refuses a -c string; -n runs nothing
		}
		if s := operand(args, nil); s != "" && s != "-" {
			return h.scriptUngated(s, modeShell, true, dirs, vars, depth)
		}
		return h.inputUngated(cmd, sc, segs, i, words[c:], modeShell, dirs, vars, depth), ""
	case interp:
		for k, a := range args {
			if slices.Contains(ip.code, a) && k+1 < len(args) {
				return h.textUngated(args[k+1], name+"'s code ("+a+")", false), ""
			}
		}
		if s := operand(args, ip.sub); s != "" && s != "-" {
			return h.scriptUngated(s, modeCode, true, dirs, vars, depth)
		}
		return h.inputUngated(cmd, sc, segs, i, words[c:], modeCode, dirs, vars, depth), ""
	case strings.Contains(words[c], "/"):
		// A path run twice over the command line is written by it first.
		return h.scriptUngated(words[c], modeShebang, strings.Count(cmd, words[c]) > 1, dirs, vars, depth)
	}
	return nil, ""
}

// isCFlag reports whether a shell's option carries -c.
func isCFlag(a string) bool {
	return strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c")
}

// operand is the first argument that is no option, no option's value, no
// redirection and no subcommand of sub: the script, "" for none.
func operand(args, sub []string) string {
	for k := 0; k < len(args); k++ {
		a := args[k]
		switch {
		case a == "--":
			if k+1 < len(args) {
				return args[k+1]
			}
			return ""
		case valueOptions[a], bareRedirect.MatchString(a):
			k++
		case strings.HasPrefix(a, "-") && a != "-", redirection.MatchString(a), slices.Contains(sub, a):
		default:
			return a
		}
	}
	return ""
}

// inputUngated finds what the shell or interpreter words[0] runs from its
// input: a here-string, a redirected file, a here-document, or the pipeline
// stages before it (their text, and the files they name).
func (h Hook) inputUngated(cmd string, sc shellScan, segs []segment, i int, words []string, mode scriptMode, dirs []string, vars map[string]string, depth int) *ungated {
	name := path.Base(words[0])
	for k, w := range words {
		switch {
		case w == "<<<" && k+1 < len(words):
			return h.textUngated(words[k+1], "the here-string "+name+" reads", mode == modeShell)
		case w == "<" && k+1 < len(words):
			u, _ := h.scriptUngated(words[k+1], mode, false, dirs, vars, depth)
			return u
		case strings.HasPrefix(w, "<") && !strings.HasPrefix(w, "<<") && len(w) > 1:
			u, _ := h.scriptUngated(w[1:], mode, false, dirs, vars, depth)
			return u
		}
	}
	for _, hd := range sc.heredocs {
		if hd.op < segs[i].start || hd.op >= segs[i].end {
			continue
		}
		body := cmd[hd.start:hd.end]
		if u := h.textUngated(body, "the here-document "+name+" reads", mode == modeShell); u != nil {
			return u
		}
		if mode == modeShell && len(dirs) > 0 {
			if u := h.ungated(body, dirs[0], depth+1); u != nil {
				return u
			}
		}
	}
	if i > 0 && segs[i-1].after == "|" {
		before := strings.TrimRight(pipelineBefore(sc.plain, segs, i), "| \t")
		if u := h.textUngated(before, "the input "+name+" reads", false); u != nil {
			return u
		}
		for _, w := range shellWords(before) {
			if u, _ := h.scriptUngated(w, mode, false, dirs, vars, depth); u != nil {
				return u
			}
		}
	}
	return nil
}

// scriptState is what reading a script found.
type scriptState int

const (
	scriptRead   scriptState = iota
	scriptAbsent             // no file at the path
	scriptNone               // no script: a directory, a binary, too large
)

// scriptUngated finds what the script at p, relative to one of dirs, runs
// outside the gate. A script that is absent is named as missing when the call
// runs it as such (explicit), so that the call's own text is read instead.
func (h Hook) scriptUngated(p string, mode scriptMode, explicit bool, dirs []string, vars map[string]string, depth int) (*ungated, string) {
	p = expandVars(p, vars)
	if len(dirs) == 0 || p == "" {
		return nil, ""
	}
	absent := false
	for _, d := range dirs {
		fp := wordPath(p, d)
		if fp == "" {
			return nil, ""
		}
		raw, st := readScript(fp)
		switch st {
		case scriptAbsent:
			absent = true
			continue
		case scriptNone:
			return nil, ""
		}
		shell := mode == modeShell || mode == modeShebang && shellScript(raw)
		if u := h.textUngated(string(raw), "the script "+fp, shell); u != nil {
			return u, ""
		}
		if shell {
			return h.ungated(string(raw), dirs[0], depth+1), ""
		}
		return nil, ""
	}
	if absent && explicit {
		return nil, p
	}
	return nil, ""
}

// readScript reads the script at p: a regular text file of at most
// maxScript bytes.
func readScript(p string) ([]byte, scriptState) {
	fi, err := os.Stat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, scriptAbsent
	case err != nil, !fi.Mode().IsRegular(), fi.Size() > maxScript:
		return nil, scriptNone
	}
	raw, err := os.ReadFile(p) //nolint:gosec // a script the command line runs, read to scan it
	if err != nil || bytes.IndexByte(raw, 0) >= 0 {
		return nil, scriptNone
	}
	return raw, scriptRead
}

// shellScript reports whether the script is a shell command line: its
// shebang names a shell, or it has none.
func shellScript(raw []byte) bool {
	first, _, _ := strings.Cut(string(raw), "\n")
	if !strings.HasPrefix(first, "#!") {
		return true
	}
	m := shebang.FindStringSubmatch(first)
	return m != nil && localShells[m[1]]
}

// textUngated finds the first blocking devctl command in text that the gate
// does not precede: at a command position or in a -c or eval string when the
// text is a shell command line, anywhere in it when it is code.
func (h Hook) textUngated(text, where string, shell bool) *ungated {
	at, fix := -1, false
	if shell {
		at = firstShellUngated(text)
		fix = at >= 0
	} else {
		for _, m := range looseOwned.FindAllStringIndex(text, -1) {
			if !looseGated.MatchString(text[:m[0]]) {
				at, fix = m[0], !strings.ContainsAny(text[m[0]:m[1]], `"',`)
				break
			}
		}
	}
	if at < 0 {
		return nil
	}
	ls := strings.LastIndexByte(text[:at], '\n') + 1
	le := strings.IndexByte(text[at:], '\n')
	if le < 0 {
		le = len(text)
	} else {
		le += at
	}
	line := text[ls:le]
	u := &ungated{what: commandWords(looseOwned.FindString(text[at:])), where: where, line: strings.Count(text[:at], "\n") + 1, text: strings.TrimSpace(line)}
	if fix {
		u.fixed = strings.TrimSpace(line[:at-ls] + ShellQuote(h.Self) + " gate -- " + line[at-ls:])
	}
	return u
}

// firstShellUngated is the offset of the first blocking devctl command of a
// shell command line that the gate does not precede, -1 for none.
func firstShellUngated(text string) int {
	at := -1
	for _, m := range owned.FindAllStringSubmatchIndex(text, -1) {
		if !gated.MatchString(text[:m[2]]) {
			at = m[2]
			break
		}
	}
	for _, m := range shellC.FindAllStringSubmatchIndex(text, -1) {
		body := text[m[1]:]
		end := closingQuote(body, text[m[2]])
		for _, mm := range anyOwned.FindAllStringIndex(body[:end], -1) {
			if !gated.MatchString(body[:mm[0]]) && (at < 0 || m[1]+mm[0] < at) {
				at = m[1] + mm[0]
			}
		}
	}
	return at
}

// commandWords is the devctl command the match names, its words apart by one
// blank and devctl without its path.
func commandWords(match string) string {
	f := strings.FieldsFunc(match, func(r rune) bool { return strings.ContainsRune(" \t\n\"',", r) })
	if len(f) == 0 {
		return "devctl command"
	}
	f[0] = path.Base(f[0])
	return strings.Join(f, " ")
}

// assignments are the literal values the command line assigns its variables.
func assignments(plain string) map[string]string {
	vars := map[string]string{}
	for _, m := range assignment.FindAllStringSubmatch(plain, -1) {
		vars[m[1]] = strings.Trim(m[2], `'"`)
	}
	return vars
}

// expandVars puts the command line's literal values into p's variable
// references; an unknown one stays.
func expandVars(p string, vars map[string]string) string {
	return pathVar.ReplaceAllStringFunc(p, func(ref string) string {
		name := strings.Trim(ref[1:], "{}")
		if v, ok := vars[name]; ok {
			return v
		}
		return ref
	})
}

// runsLocalShell reports whether the simple command runs a local shell.
func runsLocalShell(words []string) bool {
	c := commandAt(words)
	return c < len(words) && localShells[path.Base(words[c])]
}
