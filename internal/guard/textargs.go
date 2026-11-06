package guard

import (
	"path"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// agentsSubcommand is beekeeper's agent command group.
const agentsSubcommand = "agents"

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
		all := name == selfCmd && len(words) >= 3 && ((words[1] == "timer" || words[1] == "note") && words[2] == "add")
		report := name == selfCmd && len(words) >= 3 && words[1] == agentsSubcommand && words[2] == "idle"
		for i := 1; i < len(words); i++ {
			payload := all && i >= 3
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
