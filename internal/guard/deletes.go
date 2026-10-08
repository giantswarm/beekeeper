package guard

import (
	"os"
	"path"
	"regexp"
	"strings"
)

// The delete guard refuses a Bash command whose delete (rm, shred, unlink,
// truncate, find -delete or -exec rm) would reach further than it names: a
// target under a variable or command substitution that may be empty
// ("$D"/* is /* when D is unset), or one that is the root, a top-level
// directory, the home directory, or every entry of one of them.

// deleteFix is what every refusal offers instead.
const deleteFix = "Guard the variable so that the shell stops on an empty one (`${D:?}`: `rm -rf \"${D:?}\"/*`), " +
	"run the cleanup from a `set -euo pipefail` script with an `EXIT` trap, or move the files aside instead of deleting them."

// The delete commands named more than once.
const (
	rmCmd     = "rm"
	shredCmd  = "shred"
	unlinkCmd = "unlink"
)

// Placeholders in a target's resolved form: the home directory, and a part
// that is never empty but unknown (a guarded variable, $PWD, $$).
const (
	homePart   = "\x01"
	opaquePart = "\x02"
)

var (
	// deleteValues: the options of the delete commands that take a value.
	deleteValues = map[string]map[string]bool{
		shredCmd:   {"-n": true, "-s": true, "--iterations": true, "--size": true, "--random-source": true},
		"truncate": {"-s": true, "-r": true, "--size": true, "--reference": true},
	}
	// findValues: find's leading options that take a value.
	findValues = map[string]bool{"-D": true}
	// literalValue: an assigned value with no expansion in it.
	literalValue = regexp.MustCompile("^[^$`]*$")
	// nounset: a set option word that turns on nounset (-u, -eu, -euo).
	nounset  = regexp.MustCompile(`^-[a-zA-Z]*u[a-zA-Z]*$`)
	globChar = "*?["
	// keywords: the reserved words a simple command may follow.
	keywords = map[string]bool{"do": true, "then": true, "else": true, "elif": true, "if": true, "while": true, "until": true, "!": true, "{": true}
	// neverEmpty: the special parameters that always expand to something.
	neverEmpty = map[string]bool{"$": true, "?": true, "!": true, "#": true, "-": true, "0": true, "HOME": true, "PWD": true}
)

// deleteRefusal returns why cmd would delete more than it names, "" when it
// would not.
func deleteRefusal(cmd string) string {
	return scanDeletes(cmd, 0)
}

func scanDeletes(cmd string, depth int) string {
	if depth > 4 {
		return ""
	}
	sc := scanShell(cmd)
	// A here-document a shell reads is a command line of its own.
	for _, sg := range sc.segments() {
		if !runsShell(shellWords(sc.plain[sg.start:sg.end])) {
			continue
		}
		for _, h := range sc.heredocs {
			if h.op >= sg.start && h.op < sg.end {
				if r := scanDeletes(cmd[h.start:h.end], depth+1); r != "" {
					return r
				}
			}
		}
	}
	st := deleteState{assigned: map[string]string{}}
	for _, raw := range rawCommands(sc.plain) {
		if r := st.check(raw, depth); r != "" {
			return r
		}
	}
	return ""
}

func runsShell(words []string) bool {
	for _, w := range words {
		if shells[path.Base(w)] {
			return true
		}
	}
	return false
}

// deleteState is what the simple commands before the one checked tell about
// its variables: whether nounset is on and the literal values assigned.
type deleteState struct {
	nounset  bool
	assigned map[string]string
}

// check returns why the simple command (its words as written) deletes more
// than it names, and records its set -u and literal assignments.
func (st *deleteState) check(raw []string, depth int) string {
	for len(raw) > 0 && keywords[raw[0]] {
		raw = raw[1:]
	}
	words := make([]string, len(raw))
	for i, r := range raw {
		words[i] = strings.Join(shellWords(r), "")
	}
	k := commandAt(words)
	if k == len(words) {
		st.assign(raw, words)
		return ""
	}
	name, args := path.Base(words[k]), raw[k+1:]
	switch {
	case name == "set":
		for i, w := range words[k+1:] {
			if nounset.MatchString(w) || w == "-o" && k+2+i < len(words) && words[k+2+i] == "nounset" {
				st.nounset = true
			}
		}
	case name == "export" || name == "local" || name == "declare" || name == "readonly" || name == "typeset":
		st.assign(raw[k+1:], words[k+1:])
	case shells[name]:
		for _, a := range words[k+1:] {
			if strings.ContainsAny(a, " \t\n;|&") {
				if r := scanDeletes(a, depth+1); r != "" {
					return r
				}
			}
		}
	}
	for _, t := range deleteTargets(name, args) {
		if why := st.reaches(t); why != "" {
			at := strings.Join(raw[k:], " ")
			if len(at) > 200 {
				at = at[:200] + "…"
			}
			return "Refused: `" + at + "` deletes `" + t + "`: " + why + ". " + deleteFix
		}
	}
	return ""
}

// assign records the literal values of a command made of assignments only.
func (st *deleteState) assign(raw, words []string) {
	for i, w := range words {
		m := envWord.FindStringSubmatch(w)
		if m == nil {
			return
		}
		if _, v, _ := strings.Cut(raw[i], "="); literalValue.MatchString(v) {
			if v == "~" || strings.HasPrefix(v, "~/") {
				m[2] = homePart + m[2][1:]
			}
			st.assigned[m[1]] = m[2]
		} else {
			delete(st.assigned, m[1])
		}
	}
}

// deleteTargets returns the words (as written) a delete command removes,
// nil for any other command.
func deleteTargets(name string, args []string) []string {
	switch name {
	case rmCmd, shredCmd, unlinkCmd, "truncate":
		var out []string
		operands := false
		for i := 0; i < len(args); i++ {
			a := strings.Join(shellWords(args[i]), "")
			switch {
			case operands || !strings.HasPrefix(a, "-") || a == "-":
				out = append(out, args[i])
			case a == "--":
				operands = true
			case deleteValues[name][a]:
				i++
			}
		}
		return out
	case findCmd:
		deletes := false
		for i, a := range args {
			a = strings.Join(shellWords(a), "")
			next := ""
			if i+1 < len(args) {
				next = path.Base(strings.Join(shellWords(args[i+1]), ""))
			}
			if a == "-delete" || (a == "-exec" || a == "-execdir" || a == "-ok" || a == "-okdir") &&
				(next == rmCmd || next == shredCmd || next == unlinkCmd) {
				deletes = true
			}
		}
		if !deletes {
			return nil
		}
		var out []string
		for i := 0; i < len(args); i++ {
			a := strings.Join(shellWords(args[i]), "")
			switch {
			case findValues[a]:
				i++
			case a == "-H" || a == "-L" || a == "-P" || strings.HasPrefix(a, "-O"):
			case strings.HasPrefix(a, "-") || a == "(" || a == "!" || a == ")":
				return out
			default:
				out = append(out, args[i])
			}
		}
		return out
	}
	return nil
}

// expansion is one parameter expansion or command substitution in a word.
type expansion struct {
	token string
	// at is where it stands in the word's literal text.
	at int
	// value is what it resolves to: a known literal, the home or opaque
	// placeholder, or "" when it may be empty.
	value string
	// maybeEmpty: nothing makes the shell stop when it is empty or unset.
	maybeEmpty bool
}

// reaches returns what the target word reaches beyond what it names, "" when
// nothing.
func (st *deleteState) reaches(word string) string {
	lit, exps := st.expansions(word)
	for _, e := range exps {
		if e.maybeEmpty && strings.HasPrefix(lit[e.at:], "/") {
			return "when `" + e.token + "` is empty or unset (a failed `cd … && D=…` before it), that is `" +
				shown(st.resolve(lit, exps)) + "`"
		}
	}
	if what := dangerous(st.resolve(lit, exps)); what != "" {
		return "that is " + what
	}
	return ""
}

// resolve returns the word's literal text with each expansion's value in place.
func (st *deleteState) resolve(lit string, exps []expansion) string {
	var b strings.Builder
	last := 0
	for _, e := range exps {
		b.WriteString(lit[last:e.at])
		b.WriteString(e.value)
		last = e.at
	}
	b.WriteString(lit[last:])
	return b.String()
}

func shown(p string) string {
	return strings.ReplaceAll(strings.ReplaceAll(p, homePart, "~"), opaquePart, "…")
}

// dangerous names what a resolved target is when it is the root, a top-level
// directory, the home directory or every entry of one of them.
func dangerous(p string) string {
	if strings.Contains(p, opaquePart) {
		return ""
	}
	if rest, ok := strings.CutPrefix(p, homePart); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
		c := path.Clean("/" + rest)
		switch {
		case c == "/":
			return "the home directory"
		case path.Dir(c) == "/" && strings.ContainsAny(c, globChar):
			return "every entry of the home directory (`" + shown(p) + "`)"
		}
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		return ""
	}
	c := path.Clean(p)
	if home, err := os.UserHomeDir(); err == nil && home != "/" {
		if c == home || strings.HasPrefix(c, home+"/") {
			return dangerous(homePart + strings.TrimPrefix(c, home))
		}
	}
	switch dir := path.Dir(c); {
	case c == "/":
		return "the root directory"
	case dir == "/" && strings.ContainsAny(c, globChar):
		return "every entry of the root directory (`" + p + "`)"
	case dir == "/":
		return "the top-level directory `" + c + "`"
	case path.Dir(dir) == "/" && strings.ContainsAny(path.Base(c), globChar):
		return "every entry of the top-level directory `" + dir + "`"
	}
	return ""
}

// expansions returns the word's literal text (quotes removed, expansions left
// out, a leading ~ as the home placeholder) and its expansions.
func (st *deleteState) expansions(word string) (string, []expansion) {
	var lit strings.Builder
	var exps []expansion
	quote := byte(0)
	for i := 0; i < len(word); i++ {
		c := word[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				lit.WriteByte(c)
			}
		case c == '\'' && quote == 0, c == '"' && quote == 0:
			quote = c
		case c == '"' && quote == '"':
			quote = 0
		case c == '\\' && i+1 < len(word):
			i++
			lit.WriteByte(word[i])
		case c == '~' && i == 0 && (len(word) == 1 || word[1] == '/'):
			lit.WriteString(homePart)
		case c == '$' || c == '`':
			end := expansionEnd(word, i)
			if end <= i+1 {
				lit.WriteByte(c)
				continue
			}
			exps = append(exps, st.expansion(word[i:end], lit.Len()))
			i = end - 1
		default:
			lit.WriteByte(c)
		}
	}
	return lit.String(), exps
}

// expansion classifies one expansion token at offset at.
func (st *deleteState) expansion(token string, at int) expansion {
	e := expansion{token: token, at: at, value: opaquePart}
	name, op := token[1:], ""
	switch {
	case token[0] == '`' || strings.HasPrefix(token, "$("):
		e.value, e.maybeEmpty = "", true
		return e
	case strings.HasPrefix(token, "${"):
		inner := strings.TrimSuffix(token[2:], "}")
		n := len(inner) - len(strings.TrimLeft(inner, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_"))
		if n == 0 && inner != "" {
			n = 1
		}
		name, op = inner[:n], inner[n:]
	}
	switch {
	case name == "HOME" && (op == "" || op[0] == ':'):
		e.value = homePart
	case neverEmpty[name] && op == "",
		name == "#" && op != "",
		strings.HasPrefix(op, ":?"),
		(strings.HasPrefix(op, ":-") || strings.HasPrefix(op, ":=")) && len(op) > 2:
	case op != "" && op[0] != '[':
		// Another operator (${D-x}, ${D:+x}, ${D#x}, ${#D}) may give "".
		e.value, e.maybeEmpty = "", true
	case st.nounset:
		if v, ok := st.assigned[name]; ok {
			e.value = v
		}
	default:
		e.value, e.maybeEmpty = st.assigned[name], true
	}
	return e
}

// expansionEnd returns the end of the expansion starting at word[i] ($name,
// $1, ${…}, $(…), `…`), i+1 when a lone $.
func expansionEnd(word string, i int) int {
	if word[i] == '`' {
		if j := strings.IndexByte(word[i+1:], '`'); j >= 0 {
			return i + j + 2
		}
		return len(word)
	}
	if i+1 >= len(word) {
		return i + 1
	}
	switch c := word[i+1]; {
	case c == '{' || c == '(':
		closer := map[byte]byte{'{': '}', '(': ')'}[c]
		depth := 0
		for j := i + 1; j < len(word); j++ {
			switch word[j] {
			case c:
				depth++
			case closer:
				if depth--; depth == 0 {
					return j + 1
				}
			}
		}
		return len(word)
	case strings.IndexByte("$?!#-@*0123456789", c) >= 0:
		return i + 2
	}
	j := i + 1
	for j < len(word) && (word[j] == '_' || word[j] >= 'a' && word[j] <= 'z' || word[j] >= 'A' && word[j] <= 'Z' || word[j] >= '0' && word[j] <= '9') {
		j++
	}
	return j
}

// rawCommands splits a command line into its simple commands, each a list
// of its words as written (quotes kept). The commands inside $( ), ` ` and
// <( ) are commands of their own, listed before the one that holds them.
func rawCommands(s string) [][]string {
	var out [][]string
	var words []string
	var w strings.Builder
	in := false
	endWord := func() {
		if in {
			words = append(words, w.String())
			w.Reset()
			in = false
		}
	}
	endCommand := func() {
		endWord()
		if len(words) > 0 {
			out = append(out, words)
			words = nil
		}
	}
	// substitution copies the $( ) or ` ` at s[i:end] into the word and
	// lists its commands.
	substitution := func(i, end, skip int) int {
		out = append(out, rawCommands(s[i+skip:max(i+skip, end-1)])...)
		w.WriteString(s[i:end])
		in = true
		return end - 1
	}
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '\'':
			w.WriteByte(c)
			if c == '\'' {
				quote = 0
			}
		case c == '\\' && i+1 < len(s):
			w.WriteString(s[i : i+2])
			in = true
			i++
		case c == '$' && i+1 < len(s) && s[i+1] == '(', c == '`':
			skip := 2
			if c == '`' {
				skip = 1
			}
			i = substitution(i, expansionEnd(s, i), skip)
		case c == '$' && i+1 < len(s) && s[i+1] == '{':
			end := expansionEnd(s, i)
			w.WriteString(s[i:end])
			in = true
			i = end - 1
		case quote == '"':
			w.WriteByte(c)
			if c == '"' {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
			w.WriteByte(c)
			in = true
		case (c == '<' || c == '>') && i+1 < len(s) && s[i+1] == '(':
			endWord()
			i = substitution(i, expansionEnd(s, i), 2)
		case c == ' ' || c == '\t':
			endWord()
		case strings.IndexByte(";&|()\n", c) >= 0:
			endCommand()
		default:
			w.WriteByte(c)
			in = true
		}
	}
	endCommand()
	return out
}
