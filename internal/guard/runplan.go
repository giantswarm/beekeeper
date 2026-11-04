package guard

import (
	"path/filepath"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// ProcessRun moves the slot from a shell to each build it invokes. The
// original shell owns sequencing, redirections, variables and jobs; each
// nested run owns only its heavy command. Unknown shell programs retain
// the conservative whole-command cap. Prefixes include a trailing --.
func ProcessRun(argv, runPrefix, gatePrefix []string) ([]string, string) {
	if len(argv) == 0 {
		return nil, ""
	}
	if len(argv) < 3 || argv[1] != "-c" || !slices.Contains([]string{"sh", "bash", "zsh", "dash", "ksh"}, filepath.Base(argv[0])) {
		if at := ownedRunArg(argv); at >= 0 {
			result := append(slices.Clone(argv[:at]), gatePrefix...)
			result = append(result, argv[at:]...)
			return result, "gate-owned devctl command: no build slot"
		}
		if slices.Contains([]string{"tail", "cat"}, filepath.Base(argv[0])) {
			return slices.Clone(argv), "output command: no build slot"
		}
		return nil, ""
	}
	source := argv[2]
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(source), "")
	if err != nil {
		return nil, ""
	}
	type insertion struct {
		at     int
		prefix string
	}
	var inserts []insertion
	safe := true
	syntax.Walk(file, func(n syntax.Node) bool {
		if !safe {
			return false
		}
		// Functions use the caller's shell lookup, which an external run cannot.
		if _, ok := n.(*syntax.FuncDecl); ok {
			safe = false
			return false
		}
		call, ok := n.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		var literal []string
		for _, arg := range call.Args {
			word := arg.Lit()
			if word == "" {
				word = "_"
			}
			literal = append(literal, word)
		}
		text := quotedRunWords(literal)
		at := 0
		var prefix []string
		switch {
		case ownedRunArg(literal) >= 0:
			at = ownedRunArg(literal)
			prefix = gatePrefix
		case isHeavy(text):
			prefix = runPrefix
		default:
			name := call.Args[0].Lit()
			if !slices.Contains(strings.Fields(": true false exit echo printf tail cat cd pwd export unset set shift umask read test [ sleep wait jobs"), name) && !helpOnly(text, 0) {
				safe = false
			}
		}
		if prefix != nil {
			var words []string
			for _, word := range prefix {
				words = append(words, ShellQuote(word))
			}
			inserts = append(inserts, insertion{int(call.Args[at].Pos().Offset()), strings.Join(words, " ") + " "})
		}
		return true
	})
	if !safe {
		return nil, ""
	}
	// Sort backwards so all offsets still refer to the original source.
	slices.SortFunc(inserts, func(a, b insertion) int { return b.at - a.at })
	for _, edit := range inserts {
		source = source[:edit.at] + edit.prefix + source[edit.at:]
	}
	result := slices.Clone(argv)
	result[2] = source
	return result, "shell runs without a build slot; each heavy command takes its own slot"
}

// Quote argv before scanning so punctuation in data cannot become code.
func quotedRunWords(argv []string) string {
	words := make([]string, len(argv))
	for i, word := range argv {
		words[i] = ShellQuote(word)
	}
	return strings.Join(words, " ")
}

// ownedRunArg locates the devctl word itself, leaving env/timeout wrappers
// outside the gate just as the hook does.
func ownedRunArg(argv []string) int {
	text := quotedRunWords(argv)
	match := commandMatch(owned, text, notHelp)
	if match == nil {
		return -1
	}
	offset := 0
	for i, word := range argv {
		if offset == match[2] && filepath.Base(word) == "devctl" {
			return i
		}
		offset += len(ShellQuote(word)) + 1
	}
	return -1
}
