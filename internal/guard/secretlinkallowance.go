package guard

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// One link, literal assignments before it, and metadata checks need no
// analysis of newly created links. Any reader or shell construct removes the
// exemption for the entire command and uses the base guard's refusals.
type linkAllowance struct {
	allowed bool
	vars    map[string]string
}

func simpleLinkAllowance(cmd string, helpers map[string]string) linkAllowance {
	out := linkAllowance{vars: map[string]string{}}
	f, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil {
		return out
	}
	links := 0
	for _, st := range f.Stmts {
		if !plainLinkStmt(st) {
			return out
		}
		switch node := st.Cmd.(type) {
		case *syntax.FuncDecl:
			if helpers[node.Name.Value] == "" {
				return out
			}
		case *syntax.CallExpr:
			if len(node.Args) == 0 {
				for _, a := range node.Assigns {
					if links != 0 || a.Name == nil || a.Value == nil || a.Append || a.Index != nil || a.Array != nil || !literalLinkWord(a.Value, nil) {
						return out
					}
					if _, seen := out.vars[a.Name.Value]; seen {
						return out
					}
					value := shellWordText(a.Value)
					if value == "" {
						return out
					}
					out.vars[a.Name.Value] = value
				}
				continue
			}
			if len(node.Assigns) != 0 {
				return out
			}
			for _, a := range node.Args {
				if !literalLinkWord(a, out.vars) {
					return out
				}
			}
			name := node.Args[0].Lit()
			switch {
			case name == "ln" || helpers[name] != "":
				links++
				if links > 1 {
					return out
				}
			case name == "readlink" || name == statCommand || name == testCommand || name == "[":
			default:
				return out
			}
		default:
			return out
		}
	}
	out.allowed = true
	return out
}

func plainLinkStmt(st *syntax.Stmt) bool {
	return len(st.Redirs) == 0 && !st.Negated && !st.Background && !st.Coprocess && !st.Disown
}

// Only a plain top-level shell invocation may grant its script the allowance.
// Executable arguments to another utility always keep the base refusal.
func standaloneLinkScript(cmd string) string {
	f, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil || len(f.Stmts) != 1 || !plainLinkStmt(f.Stmts[0]) {
		return ""
	}
	call, ok := f.Stmts[0].Cmd.(*syntax.CallExpr)
	if !ok || len(call.Assigns) != 0 || len(call.Args) < 2 || len(call.Args) > 3 {
		return ""
	}
	for _, arg := range call.Args {
		if !literalLinkWord(arg, nil) {
			return ""
		}
	}
	name := call.Args[0].Lit()
	if name != bashCmd && name != "sh" || len(call.Args) == 3 && call.Args[2].Lit() != "--check" {
		return ""
	}
	p := shellWordText(call.Args[1])
	if p == "" || strings.HasPrefix(p, "-") || strings.HasPrefix(p, "+") || strings.ContainsAny(p, "*?[") {
		return ""
	}
	return p
}

// Only HOME and variables already assigned a literal once can expand here.
func literalLinkWord(w *syntax.Word, vars map[string]string) bool {
	ok := true
	syntax.Walk(w, func(n syntax.Node) bool {
		switch node := n.(type) {
		case nil, *syntax.Word, *syntax.Lit, *syntax.DblQuoted:
		case *syntax.SglQuoted:
			ok = !node.Dollar
		case *syntax.ParamExp:
			if node.Param == nil {
				ok = false
				break
			}
			name := node.Param.Value
			_, bound := vars[name]
			word := shellWordText(&syntax.Word{Parts: []syntax.WordPart{node}})
			ok = (name == homeVar || bound) && (word == "$"+name || word == "${"+name+"}")
		default:
			ok = false
		}
		return ok
	})
	return ok
}

func (a linkAllowance) resolve(words []string) []string {
	if !a.allowed {
		return words
	}
	out := slices.Clone(words)
	for i, word := range out {
		out[i] = expandVars(word, a.vars)
	}
	return out
}

// Only a helper whose entire body is one quoted positional ln is exempt.
// Invalid declarations still shadow metadata utility names.
func metadataFunctions(cmd string) map[string]string {
	f, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil {
		return nil
	}
	decls := map[string]*syntax.FuncDecl{}
	syntax.Walk(f, func(n syntax.Node) bool {
		if fn, ok := n.(*syntax.FuncDecl); ok {
			if _, seen := decls[fn.Name.Value]; seen {
				decls[fn.Name.Value] = nil
			} else {
				decls[fn.Name.Value] = fn
			}
		}
		return true
	})
	out := map[string]string{}
	for name, fn := range decls {
		out[name] = ""
		if fn == nil {
			continue
		}
		if _, shadow := decls["ln"]; shadow {
			continue
		}
		body, ok := fn.Body.Cmd.(*syntax.Block)
		if !ok || !plainLinkStmt(fn.Body) || len(body.Stmts) != 1 || !plainLinkStmt(body.Stmts[0]) {
			continue
		}
		call, ok := body.Stmts[0].Cmd.(*syntax.CallExpr)
		if !ok || len(call.Assigns) != 0 || len(call.Args) < 3 || call.Args[0].Lit() != "ln" {
			continue
		}
		flag := call.Args[1].Lit()
		if !simpleLinkFlag(flag) {
			continue
		}
		if len(call.Args) == 4 && quotedLinkParam(call.Args[2], "1") && quotedLinkParam(call.Args[3], "2") ||
			len(call.Args) == 3 && quotedLinkParam(call.Args[2], "@") {
			out[name] = flag
		}
	}
	return out
}

func quotedLinkParam(w *syntax.Word, name string) bool {
	if len(w.Parts) != 1 {
		return false
	}
	q, ok := w.Parts[0].(*syntax.DblQuoted)
	if !ok || len(q.Parts) != 1 {
		return false
	}
	if _, ok := q.Parts[0].(*syntax.ParamExp); !ok {
		return false
	}
	word := shellWordText(w)
	return word == "$"+name || word == "${"+name+"}"
}

func shellWordText(w *syntax.Word) string {
	var b bytes.Buffer
	_ = syntax.NewPrinter().Print(&b, w)
	return strings.Join(shellWords(b.String()), " ")
}

func simpleLinkFlag(flag string) bool {
	return flag == "-s" || flag == "-sf" || flag == "-sfn"
}

// A helper invocation is checked after expansion, never from its body alone.
func linkArgs(name string, args []string, helpers map[string]string) []string {
	if flag, helper := helpers[name]; helper {
		if flag == "" || len(args) != 2 {
			return nil
		}
		return []string{flag, args[0], args[1]}
	}
	if name == "ln" {
		return args
	}
	return nil
}

func (f secretFiles) symbolicMetadata(args, dirs []string) bool {
	if len(args) != 3 || !simpleLinkFlag(args[0]) {
		return false
	}
	for _, arg := range args[1:] {
		arg = strings.ReplaceAll(strings.ReplaceAll(arg, "${HOME}", f.home), "$HOME", f.home)
		if arg == "" || strings.ContainsAny(arg, "*?[$`") {
			return false
		}
	}
	for _, dir := range dirs {
		paths := symbolicLinkPaths(args, dir)
		if len(paths) == 0 {
			return false
		}
		for _, p := range paths {
			if p.source == "" || p.alternate == "" {
				return false
			}
			for _, source := range []string{p.source, p.alternate} {
				if fi, err := os.Lstat(source); err == nil && fi.Mode()&os.ModeSymlink != 0 && f.match(source) != "" {
					return false
				}
			}
		}
	}
	return true
}

type symbolicPath struct{ source, alternate, link string }

func symbolicLinkPaths(args []string, dir string) []symbolicPath {
	if len(args) != 3 || !simpleLinkFlag(args[0]) {
		return nil
	}
	dest := wordPath(args[2], dir)
	if dest == "" {
		return nil
	}
	if fi, err := os.Stat(dest); strings.HasSuffix(args[2], "/") || err == nil && fi.IsDir() {
		dest = filepath.Join(dest, filepath.Base(args[1]))
	}
	return []symbolicPath{{wordPath(args[1], filepath.Dir(dest)), wordPath(args[1], dir), dest}}
}
