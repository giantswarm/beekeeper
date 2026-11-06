package guard

import (
	"regexp"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

const (
	listAnd = "&&"
	listOr  = "||"
)

var (
	// shellStateWords change the shell a later command runs in, or read what
	// only that shell knows, so a command moved to a shell of its own would
	// lose them.
	shellStateWords = wordSet("source . eval exec alias unalias unset trap shift read readarray mapfile getopts hash shopt setopt unsetopt " +
		"builtin command emulate")
	// setupWords do no work of their own: a line of them and builds has
	// nothing to run ahead of the slot.
	setupWords = wordSet("cd pushd popd true : set export")
	// shellParam is a parameter a child shell can inherit: a plain name.
	shellParam = regexp.MustCompile(`^[A-Za-z_]\w*$`)
	// shellOwnParams are plain names only the running shell answers for.
	shellOwnParams = wordSet("PIPESTATUS RANDOM SECONDS LINENO BASHPID BASH_COMMAND FUNCNAME _")
)

// buildLeaf is one pipeline of a list of commands: its bytes in the line, the
// operator before it in its statement, and which statement it belongs to.
type buildLeaf struct {
	start, end uint
	op         string
	stmt       int
	heavy      bool
	setup      bool
}

// buildList is a command line read as a flat list of pipelines.
type buildList struct {
	cmd    string
	leaves []buildLeaf
	// sets are the line's top-level set commands, which each routed command
	// behind them repeats: set -o pipefail and set -e must hold inside it too,
	// and a set further on must not.
	sets []shellSet
}

// routeBuilds returns cmd with only its heavy commands under `beekeeper run`,
// so that a clone or a file edit beside a build runs without waiting for the
// slot, and a slot that does not come costs only the build. Neighbouring builds
// share one run, a pipeline's stages run together and stay together, and a
// command that may run what the line built counts as a build. It returns ""
// when the line must stay whole: it cannot be parsed, it is no flat list of
// simple commands, one command depends on the shell another one changes, or
// splitting it would move nothing out of the slot.
func (h Hook) routeBuilds(cmd string) string {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(cmd), "")
	if err != nil {
		return ""
	}
	l := buildList{cmd: cmd}
	for i, st := range f.Stmts {
		if !l.add(st, i, "") {
			return ""
		}
	}
	if !slices.ContainsFunc(l.leaves, func(b buildLeaf) bool { return !b.heavy && !b.setup }) {
		return ""
	}
	spans := l.spans()
	if len(spans) == 0 {
		return ""
	}
	out := cmd
	for _, sp := range slices.Backward(spans) {
		inner := strings.Join(append(l.setsBefore(sp[0]), cmd[sp[0]:sp[1]]), "; ")
		out = out[:sp[0]] + ShellQuote(h.Self) + " run -- " + ShellQuote(h.Shell) + " -c " + ShellQuote(inner) + out[sp[1]:]
	}
	return out
}

// add reads st into the list, false for a statement that keeps the line whole.
func (l *buildList) add(st *syntax.Stmt, stmt int, op string) bool {
	if st.Background || st.Coprocess || st.Negated {
		return false
	}
	if b, ok := st.Cmd.(*syntax.BinaryCmd); ok && (b.Op == syntax.AndStmt || b.Op == syntax.OrStmt) {
		return len(st.Redirs) == 0 && l.add(b.X, stmt, op) && l.add(b.Y, stmt, b.Op.String())
	}
	if changesShell(st) {
		return false
	}
	start, end := st.Pos().Offset(), stmtEnd(st)
	leaf := buildLeaf{start: start, end: end, op: op, stmt: stmt}
	text := l.cmd[start:end]
	mayRun, ok := l.classify(st, &leaf, op)
	if !ok {
		return false
	}
	leaf.heavy = isHeavy(text) || mayRun
	if leaf.heavy && !portable(st) {
		return false
	}
	l.leaves = append(l.leaves, leaf)
	return true
}

// classify marks a simple command, a pipeline of them or an export; a set
// command joins sets. mayRun says the command is a path, which may run what
// the line built. ok is false for anything else.
func (l *buildList) classify(st *syntax.Stmt, leaf *buildLeaf, op string) (mayRun, ok bool) {
	switch c := st.Cmd.(type) {
	case *syntax.DeclClause:
		leaf.setup = true
		return false, c.Variant.Value == exportCmd
	case *syntax.BinaryCmd:
		return l.pipeline(c)
	case *syntax.CallExpr:
		if len(c.Args) == 0 {
			return false, false // an assignment no child shell inherits
		}
		name := c.Args[0].Lit()
		if shellStateWords[name] {
			return false, false
		}
		leaf.setup = setupWords[name]
		if name == verbSet {
			if op != "" || c.Assigns != nil || len(st.Redirs) > 0 || !literalWords(c.Args) {
				return false, false
			}
			l.sets = append(l.sets, shellSet{leaf.start, l.cmd[leaf.start:leaf.end]})
		}
		return name == "" || strings.Contains(name, "/"), true
	}
	return false, false
}

// pipeline reads a pipeline's stages, which must all be simple commands.
func (l *buildList) pipeline(b *syntax.BinaryCmd) (mayRun, ok bool) {
	for _, st := range []*syntax.Stmt{b.X, b.Y} {
		if st.Background || st.Coprocess || st.Negated {
			return false, false
		}
		var run bool
		switch c := st.Cmd.(type) {
		case *syntax.BinaryCmd:
			if c.Op != syntax.Pipe && c.Op != syntax.PipeAll {
				return false, false
			}
			if run, ok = l.pipeline(c); !ok {
				return false, false
			}
		case *syntax.CallExpr:
			if len(c.Args) == 0 || shellStateWords[c.Args[0].Lit()] {
				return false, false
			}
			name := c.Args[0].Lit()
			run = name == "" || strings.Contains(name, "/")
		default:
			return false, false
		}
		mayRun = mayRun || run
	}
	return mayRun, true
}

// spans are the byte ranges to run in the slot: each heavy leaf, joined with
// the ones next to it where the join keeps the line's meaning. An operator
// chain joins by && alone, and never beside a ||, which binds as tightly: `a
// || b && c` is `(a || b) && c`. Statements join when all of each is heavy.
func (l *buildList) spans() [][2]uint {
	pure, hasOr := map[int]bool{}, map[int]bool{}
	for _, b := range l.leaves {
		if _, seen := pure[b.stmt]; !seen {
			pure[b.stmt] = true
		}
		pure[b.stmt] = pure[b.stmt] && b.heavy
		hasOr[b.stmt] = hasOr[b.stmt] || b.op == listOr
	}
	join := func(prev, next buildLeaf) bool {
		if prev.stmt != next.stmt {
			return pure[prev.stmt] && pure[next.stmt]
		}
		return pure[next.stmt] || next.op == listAnd && !hasOr[next.stmt]
	}
	var out [][2]uint
	for i := 0; i < len(l.leaves); i++ {
		if !l.leaves[i].heavy {
			continue
		}
		j := i
		for j+1 < len(l.leaves) && l.leaves[j+1].heavy && join(l.leaves[j], l.leaves[j+1]) {
			j++
		}
		out = append(out, [2]uint{l.leaves[i].start, l.leaves[j].end})
		i = j
	}
	return out
}

// stmtEnd is where a statement's command and redirections end, short of the
// operator that ends it, which syntax.Stmt.End counts in.
func stmtEnd(st *syntax.Stmt) uint {
	end := st.Cmd.End().Offset()
	for _, r := range st.Redirs {
		end = max(end, r.End().Offset())
	}
	return end
}

// portable reports whether a statement means the same in a child shell: no
// here-document, whose body stands apart from its line, and no parameter of
// this shell alone ($?, $!, $1).
func portable(st *syntax.Stmt) bool {
	ok := true
	syntax.Walk(st, func(n syntax.Node) bool {
		switch x := n.(type) {
		case *syntax.Redirect:
			ok = ok && x.Hdoc == nil
		case *syntax.ParamExp:
			ok = ok && x.Param != nil && shellParam.MatchString(x.Param.Value)
		}
		return ok
	})
	return ok
}

// literalWords reports whether every word is plain text.
func literalWords(words []*syntax.Word) bool {
	return !slices.ContainsFunc(words, func(w *syntax.Word) bool { return w.Lit() == "" })
}

// changesShell reports whether a statement's expansions read what only this
// shell knows, such as each stage's status of the pipeline before it, or change
// the shell they run in, or may end it: ${x:=y} and arithmetic assign, ${x:?y}
// exits. In a child shell a later command would miss the one and outlive the
// other.
func changesShell(st *syntax.Stmt) bool {
	found := false
	syntax.Walk(st, func(n syntax.Node) bool {
		switch x := n.(type) {
		case *syntax.ArithmExp, *syntax.BinaryArithm, *syntax.UnaryArithm:
			found = true
		case *syntax.ParamExp:
			if x.Param != nil && shellOwnParams[x.Param.Value] {
				found = true
			}
			if x.Exp != nil {
				switch x.Exp.Op {
				case syntax.AssignUnset, syntax.AssignUnsetOrNull, syntax.ErrorUnset, syntax.ErrorUnsetOrNull:
					found = true
				}
			}
		}
		return !found
	})
	return found
}

// shellSet is a top-level set command and where it stands in the line.
type shellSet struct {
	at   uint
	text string
}

// setsBefore are the set commands in force at offset at.
func (l *buildList) setsBefore(at uint) []string {
	var out []string
	for _, s := range l.sets {
		if s.at < at {
			out = append(out, s.text)
		}
	}
	return out
}
