package guard

import (
	"regexp"
	"slices"
	"strings"
)

// exportCmd exports the variables it assigns.
const exportCmd = "export"

var (
	// assignWord is a word that assigns a variable: NAME=, NAME+= or
	// NAME[i]=, read on the masked line.
	assignWord = regexp.MustCompile(`^(\w+)(?:\[[^\]]*\])?\+?=`)
	// declarations assign the variables their words name.
	declarations = wordSet(exportCmd + " local declare typeset readonly")
	// maskedWord is a word of the masked line: quoted blanks are masked.
	maskedWord = regexp.MustCompile(`\S+`)
)

// assigned is the variable assignment an offset of the command line sits in.
type assigned struct {
	name string
	// exported: the value reaches a command's environment, by export,
	// declare -x or as the prefix of the command it stands before.
	exported bool
	seg      int // the segment
	cmd      int // the offset of the command after the assignments, -1 for none
}

// assignedValue returns the assignment whose value holds the offset at: a
// word NAME=… the simple command starts with (after its keywords), or one
// after export, local, declare, typeset or readonly and their options.
// Assigning stores the text, it runs none of it.
func assignedValue(sc shellScan, segs []segment, at int) (assigned, bool) {
	for i, sg := range segs {
		if at < sg.start || at >= sg.end {
			continue
		}
		a, ok := assigned{seg: i, cmd: -1}, false
		decl, kw := "", true
		for _, w := range maskedWord.FindAllStringIndex(sc.masked[sg.start:sg.end], -1) {
			ws, we := sg.start+w[0], sg.start+w[1]
			word := sc.masked[ws:we]
			if kw && keywords[word] {
				continue
			}
			kw = false
			if m := assignWord.FindStringSubmatch(word); m != nil {
				if at >= ws && at < we {
					a.name, ok = m[1], at >= ws+len(m[0])
				}
				continue
			}
			if decl == "" && a.cmd < 0 && declarations[word] {
				decl = word
				a.exported = word == exportCmd
				continue
			}
			if decl != "" {
				// an option, or a name declared without a value
				a.exported = a.exported || strings.HasPrefix(word, "-") && strings.Contains(word, "x")
				continue
			}
			a.cmd = ws
			break
		}
		if ok && a.cmd >= 0 {
			a.exported = true
		}
		return a, ok
	}
	return assigned{}, false
}

// heldMerges returns why the call is refused when a variable it assigns holds
// a blocking devctl command, quoted, without the gate, and the call uses the
// variable other than as text: the hook writes no gate into an assigned value
// (it is data, often a comment's body), so a variable that reaches a command
// line, a -c or eval string, a shell's input or a program's environment would
// run its merge outside the gate. A value only expanded into a quoted argument
// of a text command (textStage) or into a here-document that is content passes,
// as does one copied into another variable that passes the same test.
func heldMerges(cmd string) string {
	sc := scanShell(cmd)
	segs := sc.segments()
	content := contentBodies(sc)
	held := map[string]string{}
	var names []string
	hold := func(a assigned, what string) string {
		if a.exported && !textStage(sc, fromCommand(segs, a), a.seg) {
			return "Refused: the variable " + a.name + " holds a " + what + " and reaches a command's environment, " +
				"which may run it outside the gate. Run devctl as its own command line; text that only names it goes into " +
				"a quoted argument of gh, git commit, echo or printf, or into a file."
		}
		if _, seen := held[a.name]; !seen {
			held[a.name] = what
			names = append(names, a.name)
		}
		return ""
	}
	for _, m := range anyOwned.FindAllStringIndex(cmd, -1) {
		if sc.masked[m[0]] == cmd[m[0]] || gated.MatchString(cmd[:m[0]]) {
			continue
		}
		if a, ok := assignedValue(sc, segs, m[0]); ok {
			if r := hold(a, commandWords(cmd[m[0]:m[1]])); r != "" {
				return r
			}
		}
	}
	for k := 0; k < len(names); k++ {
		name := names[k]
		use := regexp.MustCompile(`\$\{?` + regexp.QuoteMeta(name) + `\b`)
		for _, e := range use.FindAllStringIndex(cmd, -1) {
			at := e[0]
			if a, ok := assignedValue(sc, segs, at); ok {
				if r := hold(a, held[name]); r != "" {
					return r
				}
				continue
			}
			if inRanges(content, at) {
				continue
			}
			i := slices.IndexFunc(segs, func(sg segment) bool { return at >= sg.start && at < sg.end })
			if i >= 0 && !inHeredoc(sc, at) && sc.masked[at] != cmd[at] && textStage(sc, fromCommand(segs, assigned{seg: i, cmd: commandStart(sc, segs[i])}), i) {
				continue
			}
			return "Refused: the variable " + name + " holds a " + held[name] + " and the call expands it where a command " +
				"may run it (not as a quoted argument of gh, git commit, echo or printf), outside the gate. Run devctl as its " +
				"own command line; text that only names it goes into a quoted argument of a text command, or into a file."
		}
	}
	return ""
}

// commandStart is the offset of the segment's command after its leading
// assignments, -1 for none.
func commandStart(sc shellScan, sg segment) int {
	for _, w := range maskedWord.FindAllStringIndex(sc.masked[sg.start:sg.end], -1) {
		if !assignWord.MatchString(sc.masked[sg.start+w[0] : sg.start+w[1]]) {
			return sg.start + w[0]
		}
	}
	return -1
}

// fromCommand is segs with the assignment's segment starting at its command,
// so that textStage reads the command its prefix assignments stand before.
func fromCommand(segs []segment, a assigned) []segment {
	if a.cmd < 0 {
		return segs
	}
	out := slices.Clone(segs)
	out[a.seg].start = a.cmd
	return out
}
