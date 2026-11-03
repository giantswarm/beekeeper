package remotebuild

import (
	"bytes"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

const (
	toolCargo = "cargo"
	shellBash = "bash"
)

// Toolchain admits only one literal build invocation. Shell operators,
// assignments, expansions, formatters and arbitrary scripts remain local.
// Builds and tests may write their pod's copy; callers needing those writes in
// the local worktree must select --local.
func Toolchain(argv []string) string {
	if len(argv) == 3 && (filepath.Base(argv[0]) == shellBash || filepath.Base(argv[0]) == "sh") && (argv[1] == "-c" || argv[1] == "-lc") {
		f, err := syntax.NewParser().Parse(strings.NewReader(argv[2]), "")
		if err != nil || len(f.Stmts) != 1 {
			return ""
		}
		s := f.Stmts[0]
		if s.Background || s.Negated || len(s.Redirs) != 0 {
			return ""
		}
		call, ok := s.Cmd.(*syntax.CallExpr)
		if !ok || len(call.Assigns) != 0 {
			return ""
		}
		argv = nil
		for _, w := range call.Args {
			value, ok := literal(w)
			if !ok {
				return ""
			}
			argv = append(argv, value)
		}
	}
	if len(argv) < 2 {
		return ""
	}
	// Absolute executables on the host have no corresponding identity in the image.
	if argv[0] != "go" && argv[0] != toolCargo {
		return ""
	}
	for _, a := range argv[2:] {
		if a == "--fix" {
			return ""
		}
		if strings.HasPrefix(a, "-") && (strings.HasPrefix(a, "-C") || strings.HasPrefix(a, "--manifest-path") || strings.HasPrefix(a, "--target-dir") || strings.HasPrefix(a, "--config") || strings.HasPrefix(a, "-overlay") || strings.HasPrefix(a, "-modfile") || strings.HasPrefix(a, "-exec") || strings.HasPrefix(a, "-toolexec")) {
			return ""
		}
		if filepath.IsAbs(a) {
			return ""
		}
	}
	switch argv[0] + " " + argv[1] {
	case "go build", "go test", "go vet", "cargo build", "cargo test", "cargo check", "cargo clippy":
		return argv[0]
	case "cargo fmt":
		for _, a := range argv[2:] {
			if a == "--check" {
				return toolCargo
			}
		}
	}
	return ""
}

func literal(w *syntax.Word) (string, bool) {
	var b bytes.Buffer
	var add func([]syntax.WordPart) bool
	add = func(parts []syntax.WordPart) bool {
		for _, p := range parts {
			switch p := p.(type) {
			case *syntax.Lit:
				b.WriteString(p.Value)
			case *syntax.SglQuoted:
				b.WriteString(p.Value)
			case *syntax.DblQuoted:
				if !add(p.Parts) {
					return false
				}
			default:
				return false
			}
		}
		return true
	}
	if !add(w.Parts) {
		return "", false
	}
	return b.String(), true
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func shellCommand(argv []string) string {
	var words []string
	for _, s := range argv {
		words = append(words, quote(s))
	}
	return strings.Join(words, " ")
}
