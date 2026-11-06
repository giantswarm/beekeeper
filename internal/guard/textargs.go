package guard

import (
	"path"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// beekeeper's command groups that carry text.
const (
	agentsSubcommand = "agents"
	noteSubcommand   = "note"
)

// The flags of timer add and note add. A flag missing here leaves the whole
// call unmasked. Only the description and the note's text flags are text:
// timer --probe and --run and note --until hold commands that run later.
var (
	addBoolFlags    = strings.Fields("expire h help holds json")
	timerValueFlags = strings.Fields("as config every probe repeat request-id run until wake when")
	noteValueFlags  = strings.Fields("as checked config default due fold-into for kind option recommend ref request-id status-quo until why")
	noteTextFlags   = strings.Fields("option status-quo why")
)

// isAddCommand reports whether words are a timer add or a note add.
func isAddCommand(words []string) bool {
	return len(words) >= 3 && path.Base(words[0]) == selfCmd && (words[1] == "timer" || words[1] == noteSubcommand) && words[2] == "add"
}

// laterCommands returns what a timer add or note add may run after this call,
// for the checks of a command: each of its arguments that is not text. Quoted,
// they are no command position of the line itself, so a check would miss them.
func laterCommands(command string) []string {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return nil
	}
	var later []string
	syntax.Walk(f, func(n syntax.Node) bool {
		call, ok := n.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		words := make([]string, len(call.Args))
		for i, w := range call.Args {
			words[i] = shellWordText(w)
		}
		rest, unsure := unwrapWords(words)
		if unsure != "" || !isAddCommand(rest) {
			return true
		}
		text := addTextArgs(rest, call.Args[len(words)-len(rest):])
		for i := 3; i < len(rest); i++ {
			if text[i] {
				continue
			}
			word := rest[i]
			if _, value, ok := strings.Cut(word, "="); ok && strings.HasPrefix(word, "-") {
				word = value
			}
			later = append(later, word)
		}
		return true
	})
	return later
}

// addTextArgs returns the indexes of the words of a timer add or note add that
// are text no one executes. It reads them as the command's own parser does,
// and returns none when it cannot tell which word is which.
func addTextArgs(words []string, args []*syntax.Word) map[int]bool {
	if slices.ContainsFunc(args, expandsAtRunTime) {
		return nil
	}
	valueFlags, textFlags, textAt := timerValueFlags, []string(nil), 1
	if words[1] == noteSubcommand {
		valueFlags, textFlags, textAt = noteValueFlags, noteTextFlags, 0
	}
	text := map[int]bool{}
	positional := 0
	for i := 3; i < len(words); i++ {
		word := words[i]
		if word == "--" {
			return nil
		}
		// A dashed word with a space and no value is no flag of the parser's.
		if !strings.HasPrefix(word, "-") || word == "-" || !strings.Contains(word, "=") && strings.ContainsAny(word, " \t\r\n") {
			text[i] = positional == textAt
			positional++
			continue
		}
		flag, _, inline := strings.Cut(strings.TrimLeft(word, "-"), "=")
		switch {
		case slices.Contains(addBoolFlags, flag):
		case !slices.Contains(valueFlags, flag):
			return nil
		case inline:
			text[i] = slices.Contains(textFlags, flag)
		default:
			i++
			text[i] = slices.Contains(textFlags, flag)
		}
	}
	return text
}

// expandsAtRunTime reports whether the shell may turn a word into something
// else, even another flag, so that its place on the line proves nothing.
func expandsAtRunTime(word *syntax.Word) bool {
	for _, part := range word.Parts {
		if lit, ok := part.(*syntax.Lit); ok && strings.ContainsAny(lit.Value, "{*?[") {
			return true
		}
	}
	found := false
	syntax.Walk(word, func(n syntax.Node) bool {
		switch n.(type) {
		case *syntax.ParamExp, *syntax.CmdSubst, *syntax.ProcSubst, *syntax.ArithmExp, *syntax.ExtGlob:
			found = true
		}
		return !found
	})
	return found
}

// commandText masks literal payloads of commands that carry text. Offsets and
// shell substitutions are preserved: substitutions still execute even in text.
// If parsing fails, the original command is checked conservatively.
func commandText(command string) string {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return command
	}
	masked := []byte(command)
	syntax.Walk(f, func(n syntax.Node) bool {
		call, ok := n.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		words := make([]string, len(call.Args))
		for i, w := range call.Args {
			words[i] = shellWordText(w)
		}
		rest, unsure := unwrapWords(words)
		if unsure != "" || len(rest) == 0 {
			return true
		}
		args := call.Args[len(words)-len(rest):]
		words = rest
		name := path.Base(words[0])
		var addText map[int]bool
		if isAddCommand(words) {
			addText = addTextArgs(words, args)
		}
		report := name == selfCmd && len(words) >= 3 && words[1] == agentsSubcommand && words[2] == "idle"
		for i := 1; i < len(words); i++ {
			payload := addText[i]
			if report && (words[i-1] == "--report" || strings.HasPrefix(words[i], "--report=")) {
				payload = true
			}
			if name == "gh" && (words[i-1] == "--body" || words[i-1] == "-b" || strings.HasPrefix(words[i], "--body=")) {
				payload = true
			}
			if !payload {
				continue
			}
			syntax.Walk(args[i], func(part syntax.Node) bool {
				var start, end uint
				switch p := part.(type) {
				case *syntax.CmdSubst, *syntax.ProcSubst:
					return false
				case *syntax.Lit:
					start, end = p.Pos().Offset(), p.End().Offset()
				case *syntax.SglQuoted:
					start, end = p.Pos().Offset()+1, p.End().Offset()-1
					if p.Dollar {
						start++
					}
				default:
					return true
				}
				for j := start; j < end; j++ {
					masked[j] = 'x'
				}
				return false
			})
		}
		return true
	})
	return string(masked)
}
