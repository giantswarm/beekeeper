package guard

import (
	"os"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

func (h Hook) recoveryReason() string {
	return "Beekeeper configuration cannot load: " + h.ConfigErr.Error() +
		". Recovery mode permits read-only inspection and beekeeper (use beekeeper config set or self-update to repair it)."
}

// recoveryPrograms run with any literal arguments; git runs only as
// recoveryGit.
var recoveryPrograms = strings.Fields("beekeeper pwd ls cat head tail wc stat cd")

const recoveryGit = gitCmd + " status"

// recoveryCommand admits literal commands only. No expansion, redirection,
// assignments, shell wrappers or executable search helpers can escape the list.
func recoveryCommand(command string) bool {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil || len(f.Stmts) == 0 {
		return false
	}
	safe := true
	syntax.Walk(f, func(n syntax.Node) bool {
		if !safe {
			return false
		}
		switch v := n.(type) {
		case *syntax.Redirect, *syntax.Assign, *syntax.CmdSubst, *syntax.ProcSubst, *syntax.ParamExp, *syntax.ArithmExp, *syntax.FuncDecl:
			safe = false
		case *syntax.CallExpr:
			args := make([]string, len(v.Args))
			for i, w := range v.Args {
				// Decode quoted arguments. The walk also rejects dynamic expansions.
				value, ok := recoveryWord(w.Parts)
				if !ok {
					safe = false
					return false
				}
				args[i] = value
			}
			if len(args) == 0 {
				safe = false
				return false
			}
			safe = slices.Contains(recoveryPrograms, args[0]) || strings.Join(args, " ") == recoveryGit
		}
		return safe
	})
	if safe {
		cwd, _ := os.Getwd()
		if (secretGuard{cwd: cwd, files: newSecretFiles(nil)}).leak(command) != nil {
			return false
		}
	}
	return safe
}

func recoveryWord(parts []syntax.WordPart) (string, bool) {
	var b strings.Builder
	for _, part := range parts {
		switch v := part.(type) {
		case *syntax.Lit:
			if strings.ContainsAny(v.Value, "\\*?[]{}~") {
				return "", false
			}
			b.WriteString(v.Value)
		case *syntax.SglQuoted:
			b.WriteString(v.Value)
		case *syntax.DblQuoted:
			text, ok := recoveryWord(v.Parts)
			if !ok {
				return "", false
			}
			b.WriteString(text)
		default:
			return "", false
		}
	}
	return b.String(), true
}
