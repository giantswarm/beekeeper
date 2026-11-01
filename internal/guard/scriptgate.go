package guard

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
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
	// command line; shellOptions are their options that take a value.
	localShells  = map[string]bool{"sh": true, bashCmd: true, zshCmd: true, "dash": true, "ksh": true}
	shellOptions = interpreter{values: []string{"-o", "-O", "+o", "+O", "--rcfile", "--init-file"}}
	// python, node, perl and ruby, as their command lines are read.
	python = interpreter{code: []string{"-c"}, module: []string{"-m"}, values: []string{"-W", "-X", "--check-hash-based-pycs"}}
	node   = interpreter{code: []string{"-e", "--eval", "-p", "--print"}, values: []string{"-r", "--require", "--import", "--loader", "--input-type", "--env-file"}}
	perl   = interpreter{code: []string{"-e", "-E"}, combined: "eE", values: []string{"-I"}}
	ruby   = interpreter{code: []string{"-e"}, combined: "e", values: []string{"-I", "-r", "-C", "-E"}}
	// runSub is the subcommand a script follows (deno run, bun run).
	runSub = []string{"run"}
	// interpreters run a script file, the code behind one of their options,
	// or the program on their input.
	interpreters = map[string]interpreter{
		"python":  python,
		"python2": python,
		"python3": python,
		"node":    node,
		"nodejs":  node,
		"bun":     {code: node.code, sub: runSub, values: []string{"-r", "--preload"}},
		"deno":    {code: []string{"eval"}, sub: runSub, values: []string{"--config", "--import-map"}},
		"perl":    perl,
		"ruby":    ruby,
		"php":     {code: []string{"-r"}, file: []string{"-f"}, values: []string{"-d", "-c", "-z"}},
		"lua":     {code: []string{"-e"}, values: []string{"-l"}},
	}
	// redirection is a redirection word; bare, its target is the next word.
	redirection  = regexp.MustCompile(`^\d*[<>&]`)
	bareRedirect = regexp.MustCompile(`^\d*(?:<<<|[<>]|>>|>\||[<>]&|&>)$`)
)

// interpreter says how an interpreter's (or a shell's) command line names
// what it runs. Its program is read in the line's order: the argument of a
// code option, a module option (the program is a module: nothing on the line
// is a script), the argument of a file option, or the first operand (a
// script, or - for the input); an option's value, a redirection's target and
// a subcommand are no operand. What follows the program is its data.
type interpreter struct {
	code     []string // the options whose argument is the program (-c, -e)
	combined string   // the letters a short option ends in to carry a code option (perl -ne)
	module   []string // the options whose argument names a module to run (python -m)
	file     []string // the options whose argument is the script (php -f)
	values   []string // the other options that take a value
	sub      []string // the subcommands a script follows (deno run)
}

// codeOption reports whether the option carries the interpreter's code option.
func (ip interpreter) codeOption(a string) bool {
	if slices.Contains(ip.code, a) {
		return true
	}
	return ip.combined != "" && len(a) > 2 && a[0] == '-' && a[1] != '-' && strings.ContainsRune(ip.combined, rune(a[len(a)-1]))
}

// programKind says what an interpreter's or a shell's command line runs.
type programKind int

const (
	programNone   programKind = iota // nothing on the line: a module (python -m)
	programCode                      // the argument of a code option (python -c, node -e)
	programScript                    // a script file
	programInput                     // the program on its input: an operand -, or none
)

// program is what the interpreter or shell runs, as the interpreter's
// command line names it: opt the option that names it, arg its argument.
type program struct {
	kind     programKind
	opt, arg string
}

// readProgram reads the interpreter's program off its arguments, in their
// order, so that what follows the program is left as its data.
func readProgram(args []string, ip interpreter) program {
	for k := 0; k < len(args); k++ {
		a := args[k]
		switch {
		case a == "--":
			if k+1 < len(args) && args[k+1] != "-" {
				return program{kind: programScript, arg: args[k+1]}
			}
			return program{kind: programInput}
		case a == "-":
			return program{kind: programInput}
		case ip.codeOption(a):
			if k+1 < len(args) {
				return program{programCode, a, args[k+1]}
			}
			return program{kind: programNone}
		case slices.Contains(ip.module, a):
			return program{kind: programNone}
		case slices.Contains(ip.file, a):
			if k+1 < len(args) {
				return program{programScript, a, args[k+1]}
			}
			return program{kind: programNone}
		case slices.Contains(ip.values, a), bareRedirect.MatchString(a):
			k++
		case strings.HasPrefix(a, "-"), strings.HasPrefix(a, "+"), redirection.MatchString(a), slices.Contains(ip.sub, a):
		default:
			return program{kind: programScript, arg: a}
		}
	}
	return program{kind: programInput}
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
	refusal string
	what    string // the command: devctl pr merge
	where   string // the script /x/m.sh, python3's code (-c), the input bash reads
	line    int
	text    string // the line, trimmed
	fixed   string // the line with the gate written in; "" where it has no place
}

func (u *ungated) reason() string {
	if u.refusal != "" {
		return u.refusal
	}
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
			dirs = append(dirs, cdDirs(cwd, m[1])...)
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
		return h.textUngated(cmd, "this call, which runs "+unread+" (not readable before the call)", textAny)
	}
	return nil
}

// segmentUngated finds what the simple command segs[i] runs outside the gate;
// missing names a script it runs that cannot be read before the call. A file
// the command only reads is data, whatever it says: one after the program
// (`python3 -c '…' log.txt`, `bash x.sh log.txt`, `python3 - log.txt <<'EOF'`),
// a module's argument (`python3 -m json.tool log.txt`), a read-only command's
// argument, or a redirection with no command (`$(<report.txt)`).
func (h Hook) segmentUngated(cmd string, sc shellScan, segs []segment, i int, dirs []string, vars map[string]string, depth int) (*ungated, string) {
	words := shellWords(sc.plain[segs[i].start:segs[i].end])
	c := commandAt(words)
	// A redirection before the command word is the command's; one with no
	// command word reads its file as data ($(<f)).
	run := c
	for c < len(words) && redirection.MatchString(words[c]) {
		if bareRedirect.MatchString(words[c]) {
			c++
		}
		c++
	}
	if c >= len(words) {
		return nil, ""
	}
	name, args := path.Base(words[c]), words[c+1:]
	ip, interp := interpreters[name]
	mode := modeCode
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
		ip, mode = shellOptions, modeShell
		fallthrough
	case interp:
		switch p := readProgram(args, ip); p.kind {
		case programCode:
			return h.textUngated(p.arg, name+"'s code ("+p.opt+")", textCode), ""
		case programScript:
			return h.scriptUngated(p.arg, mode, true, dirs, vars, depth)
		case programInput:
			return h.inputUngated(cmd, sc, segs, i, name, words[run:], mode, dirs, vars, depth), ""
		}
		return nil, ""
	case strings.Contains(words[c], "/"):
		return h.scriptUngated(words[c], modeShebang, writtenByCall(cmd, words[c]), dirs, vars, depth)
	}
	return nil, ""
}

// writtenByCall reports whether the call names the file it runs by path more
// than once, however it spells it and wherever, quoted text included: a
// script the call writes first (cat > run.sh, chmod +x run.sh, then ./run.sh).
func writtenByCall(cmd, run string) bool {
	name := regexp.MustCompile(`(?:^|[^\w.-])` + regexp.QuoteMeta(path.Base(run)) + `(?:$|[^\w.-])`)
	return len(name.FindAllStringIndex(cmd, -1)) > 1
}

// cdDirs are the directories a cd to arg reaches from cwd, its glob expanded
// as the shell expands it ([!…] is Go's [^…]).
func cdDirs(cwd, arg string) []string {
	d := within(cwd, arg)
	if !strings.ContainsAny(d, "*?[") {
		return []string{d}
	}
	if m, err := filepath.Glob(strings.ReplaceAll(d, "[!", "[^")); err == nil && len(m) > 0 {
		return m
	}
	return []string{d}
}

// isCFlag reports whether a shell's option carries -c.
func isCFlag(a string) bool {
	return strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c")
}

// inputUngated finds what the shell or interpreter name runs from its input:
// a here-string, a redirected file, a here-document, or the pipeline stages
// before it (their text, and the files they name); words are its command's,
// a redirection before the command word included.
func (h Hook) inputUngated(cmd string, sc shellScan, segs []segment, i int, name string, words []string, mode scriptMode, dirs []string, vars map[string]string, depth int) *ungated {
	for k, w := range words {
		switch {
		case w == "<<<" && k+1 < len(words):
			return h.textUngated(words[k+1], "the here-string "+name+" reads", kindOf(mode == modeShell))
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
		if u := h.textUngated(body, "the here-document "+name+" reads", kindOf(mode == modeShell)); u != nil {
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
		if u := h.textUngated(before, "the input "+name+" reads", textAny); u != nil {
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
		if u := h.textUngated(string(raw), "the script "+fp, kindOf(shell)); u != nil {
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

// textKind says where a text runs the commands it names.
type textKind int

const (
	// textShell is a shell command line: a command at a command position, or
	// in a -c or eval string.
	textShell textKind = iota
	// textCode is an interpreter's program: a command its string literals run
	// (codeUngated); its comments and its prose run nothing.
	textCode
	// textAny is text that writes or feeds what runs (a call that writes the
	// script it runs, the stages before an interpreter's input): a command
	// anywhere in it.
	textAny
)

// kindOf is the text kind of a script or an input: a shell command line, or
// code.
func kindOf(shell bool) textKind {
	if shell {
		return textShell
	}
	return textCode
}

// textUngated finds the first blocking devctl command in text that the gate
// does not precede, as the text's kind runs it.
func (h Hook) textUngated(text, where string, kind textKind) *ungated {
	if why := h.hiddenDeskMerge(text, where, kind); why != "" {
		return &ungated{refusal: why}
	}
	at, fix := -1, false
	switch kind {
	case textShell:
		at = firstShellUngated(text)
		fix = at >= 0
	case textCode:
		at = codeUngated(text)
	case textAny:
		for _, m := range looseOwned.FindAllStringIndex(text, -1) {
			if !looseGated.MatchString(text[:m[0]]) {
				at = m[0]
				break
			}
		}
	}
	if at >= 0 && kind != textShell {
		fix = !strings.ContainsAny(looseOwned.FindString(text[at:]), `"',`)
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
	for _, cs := range shellStrings(text) {
		for _, mm := range anyOwned.FindAllStringIndex(cs.text, -1) {
			if o := cs.at + cs.offs[mm[0]]; !gated.MatchString(cs.text[:mm[0]]) && (at < 0 || o < at) {
				at = o
			}
		}
	}
	return at
}

// codeUngated is the offset of the first blocking devctl command an
// interpreter's program runs that the gate does not precede, -1 for none. A
// program runs a command from a string literal: one the literal starts with (a
// command string, the first word of an argument list) or one after a shell
// operator in it (a command line a shell runs). A command a comment names, or
// prose in a literal (a document's text, a CHANGELOG entry), runs nothing.
func codeUngated(code string) int {
	lits := stringLiterals(code)
	k := 0
	for _, m := range looseOwned.FindAllStringIndex(code, -1) {
		for k < len(lits) && lits[k][1] <= m[0] {
			k++
		}
		switch {
		case k == len(lits) || m[0] < lits[k][0]:
			if !quotedCommand.MatchString(code[:m[0]]) {
				continue // outside a literal: code or a comment
			}
		case !literalCommand.MatchString(code[lits[k][0]:m[0]]):
			continue // prose: the command is no word a shell or a program runs
		}
		if !looseGated.MatchString(code[:m[0]]) {
			return m[0]
		}
	}
	return -1
}

var (
	// literalCommand is the text in a string literal before a command a shell
	// or a program runs from it: none, or a shell operator, an escaped line
	// break, a shell's -c or eval, and the prefix commands after it.
	literalCommand = regexp.MustCompile(`(?s)^(?:.*(?:[;&|({]|\$\(|\\n|(?:\b(?:ba|z|da|k)?sh\s+(?:-\w+\s+)*-\w*c\w*|\beval)\s+\\?['"]?))?\s*` +
		`(?:(?:sudo|env|exec|command|nohup|time|nice|timeout(?:\s+\d\S*)?|\w+=\S*)(?:\s+-\S+(?:\s+\d+)?)*\s+)*$`)
	// quotedCommand is a Perl or Ruby command quote that opens before a
	// command: qx(…), %x{…}.
	quotedCommand = regexp.MustCompile(`(?:\bqx|%x)\s*[^\w\s]\s*$`)
)

// stringLiterals are the contents of the program's string literals, in their
// order: quoted by ', " or ` (a template, a Perl or Ruby command), or by three
// of one (a Python docstring). A comment (# or //, at a line's start or after
// a blank) is no literal, nor is a quote inside one.
func stringLiterals(code string) [][2]int {
	var out [][2]int
	for i := 0; i < len(code); i++ {
		c := code[i]
		switch {
		case (c == '#' || c == '/' && strings.HasPrefix(code[i:], "//")) && (i == 0 || strings.ContainsRune(" \t\n", rune(code[i-1]))):
			if nl := strings.IndexByte(code[i:], '\n'); nl >= 0 {
				i += nl
			} else {
				i = len(code)
			}
		case c == '\'' || c == '"' || c == '`':
			q := code[i : i+1]
			if strings.HasPrefix(code[i:], strings.Repeat(q, 3)) {
				q = strings.Repeat(q, 3)
			}
			start := i + len(q)
			end := start
			for end < len(code) && !strings.HasPrefix(code[end:], q) {
				if code[end] == '\\' {
					end++
				}
				end++
			}
			end = min(end, len(code))
			out = append(out, [2]int{start, end})
			i = end + len(q) - 1
		}
	}
	return out
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
