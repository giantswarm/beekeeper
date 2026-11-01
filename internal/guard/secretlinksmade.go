package guard

import (
	"bytes"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// A link to a file that holds secret values reads none of its content: ln,
// ln -s and link of one pass wherever they stand, in a command line, a script
// it runs or a helper function it calls. What reads the new link stays
// refused. Once the link exists, match resolves a symbolic link to the
// guarded file and finds a hard link by its shared inode; on the command line
// that creates it, before either exists, the link's path and destination
// word are recorded (madeLink) and every read of them is refused like a read
// of the file itself. A source that is a directory holding a guarded file, a
// glob, or a destination the guard cannot name keeps the refusal.

const (
	linkCmd = "link"
	// maxInline bounds how deep helper calls are inlined; a deeper call
	// keeps the refusal of a helper handed a guarded file.
	maxInline = 3
	// maxScanDepth is the deepest nested command line scan reads; a body
	// inlined below it would pass unread, so it is not inlined.
	maxScanDepth = 4
	// maxForms bounds the expansions of one word and of one command.
	maxForms = 32
)

// madeLink is a link to a guarded file that a command line creates: its
// path from one working directory, its destination word as written, and the
// guarded file it names.
type madeLink struct{ path, word, file string }

var (
	// braceRef: ${name}, which normWord writes as $name.
	braceRef = regexp.MustCompile(`\$\{(\w+)\}`)
	// shellVarRef: a variable reference in a word, never a glob ($*, ${x}, $x).
	shellVarRef = regexp.MustCompile(`\$(?:\{[^}]*\}|[A-Za-z_]\w*|[0-9*@#?$!-])`)
	// safeArg: an argument inlining may put into a helper's body as text.
	safeArg = regexp.MustCompile(`^(?:[A-Za-z0-9_./~+,:=%@{}-]|\$[A-Za-z_{])*$`)
)

// normWord is a destination or read word compared as text.
func normWord(w string) string {
	if w == "" {
		return ""
	}
	return path.Clean(braceRef.ReplaceAllString(w, "$$$1"))
}

// linkPlan returns the links ln's args make to guarded files and the first
// guarded file a source names, "" for none. ok is false when the link cannot
// pass: a source that is a directory holding a guarded file or a glob, or a
// destination that is an option, a glob or a substitution.
func (f secretFiles) linkPlan(args, dirs []string) (links []madeLink, file string, ok bool) {
	sources, dest, into, parsed := lnOperands(args)
	for _, d := range dirs {
		linkDir := d
		if dest != "" {
			if p := wordPath(dest, d); p != "" {
				linkDir = filepath.Dir(p)
				if strings.HasSuffix(dest, "/") || isDir(p) {
					linkDir = p
				}
			}
		}
		for _, s := range sources {
			for _, base := range []string{d, linkDir} {
				p := wordPath(s, base)
				held := f.holds(p)
				if held == "" {
					continue
				}
				if file == "" {
					file = held
				}
				if !parsed || strings.ContainsAny(s, "*?[") || f.match(p) == "" || isDir(p) {
					return nil, held, false
				}
				l, good := linkAt(s, dest, into, d)
				if !good {
					return nil, held, false
				}
				l.file = held
				links = append(links, l)
			}
		}
	}
	return links, file, true
}

// linkAt is the link ln makes of source s at dest, from the working
// directory d: dest itself, or the source's base name in dest when dest is a
// directory, in d when ln names none.
func linkAt(s, dest string, into bool, d string) (madeLink, bool) {
	word := dest
	switch {
	case dest == "":
		word = path.Base(s)
	case into || strings.HasSuffix(dest, "/") || isDir(wordPath(dest, d)):
		word = strings.TrimSuffix(dest, "/") + "/" + path.Base(s)
	}
	if strings.HasPrefix(word, "-") || strings.ContainsAny(word, "*?[`") || strings.Contains(word, "$(") {
		return madeLink{}, false
	}
	// a word with a variable left unexpanded names no path; it is compared
	// as written
	p := ""
	if !strings.Contains(homeless(word), "$") {
		if p = wordPath(word, d); p == "" {
			return madeLink{}, false
		}
	}
	return madeLink{path: p, word: normWord(word)}, true
}

// cutBySubstitution reports whether the words of segment sg continue past
// a command or process substitution.
func cutBySubstitution(sg segment) bool {
	return sg.after == "$(" || sg.after == "`" || sg.after == "<(" || sg.after == ">("
}

// lnLinkWords returns the word of every link ln's args make from the
// working directory dir, ok false when one cannot be named.
func lnLinkWords(args []string, dir string) (words []string, ok bool) {
	sources, dest, into, parsed := lnOperands(args)
	if !parsed {
		return nil, false
	}
	for _, s := range sources {
		l, good := linkAt(s, dest, into, dir)
		if !good {
			return nil, false
		}
		words = append(words, l.word)
	}
	return words, true
}

// wordPattern is word w as a glob pattern whose variable references match
// only themselves.
func wordPattern(w string) string {
	return shellVarRef.ReplaceAllStringFunc(w, func(ref string) string {
		var b strings.Builder
		for _, r := range ref {
			if strings.ContainsRune(`*?[\`, r) {
				b.WriteByte('\\')
			}
			b.WriteRune(r)
		}
		return b.String()
	})
}

// name is how a refusal names the link: its path, else its word.
func (l madeLink) name() string {
	if l.path != "" {
		return l.path
	}
	return l.word
}

// homeless is w without the leading $HOME that wordPath expands.
func homeless(w string) string {
	rest, _ := homeRest(w)
	return rest
}

// madeRead returns the made link a read of word w, path p, reaches: the link
// itself, a glob over it, or, for a recursive read, a directory above it.
func (g secretGuard) madeRead(w, p string, recursive bool) (madeLink, bool) {
	links := g.made
	if g.sink != nil {
		links = slices.Concat(links, *g.sink)
	}
	nw := normWord(w)
	if !strings.Contains(homeless(nw), "$") {
		nw = "" // a word without a variable is compared as its path
	}
	pattern := wordPattern(nw)
	for _, l := range links {
		for _, pair := range [][3]string{{p, l.path, p}, {nw, l.word, pattern}} {
			x, y, glob := pair[0], pair[1], pair[2]
			switch {
			case x == "" || y == "":
			case x == y:
				return l, true
			case glob != "" && strings.ContainsAny(shellVarRef.ReplaceAllString(x, ""), "*?["):
				if ok, _ := filepath.Match(glob, y); ok {
					return l, true
				}
			case recursive && strings.HasPrefix(y, strings.TrimSuffix(x, "/")+"/"):
				return l, true
			}
		}
	}
	return madeLink{}, false
}

// linkLeak judges a simple command that may make a link: ln, link, or a
// helper function. It returns the refusal, or checked when the command makes
// links to guarded files that pass and its words need no further read check.
func (g secretGuard) linkLeak(name string, args, dirs []string, depth int) (l *leak, checked bool) {
	flag, isFn := g.metadata[name]
	lnArgs := args
	switch {
	case isFn && flag == "":
		file := g.files.linkSource(args, dirs)
		if file == "" {
			if file = g.opaque[name]; file != "" {
				return &leak{what: "a call of " + name + ", a helper that links " + file + ", a file that holds secret values, and is not inlined", safe: fileReadSafe(file), file: file}, false
			}
			return nil, false
		}
		if bound, binds, ok := g.bindHelper(name, args, depth); ok {
			if l := g.boundScan(bound, binds, depth); l != nil {
				return l, false
			}
			return nil, true
		}
		return &leak{what: "a link that exposes " + file + ", a file that holds secret values", safe: fileReadSafe(file), file: file}, false
	case isFn:
		if lnArgs = linkArgs(name, args, g.metadata); lnArgs == nil {
			if file := g.files.linkSource(args, dirs); file != "" {
				return &leak{what: "a link that exposes " + file + ", a file that holds secret values", safe: fileReadSafe(file), file: file}, false
			}
			return nil, false
		}
	case name != "ln" && name != linkCmd:
		return nil, false
	}
	_, file, ok := g.files.linkPlan(lnArgs, dirs)
	switch {
	case file == "":
		return nil, false
	case !ok || g.cut:
		return &leak{what: "a link that exposes " + file + ", a file that holds secret values", safe: fileReadSafe(file), file: file}, false
	}
	return nil, true
}

// linksMade returns the links to guarded files that the command line cmd
// makes, its helper calls inlined, and the helpers whose calls could not be
// inlined although their bodies make such a link.
func (g secretGuard) linksMade(cmd string, vars map[string][]string, dirs []string, depth int) (links []madeLink, opaque map[string]string) {
	sc := scanShell(cmd)
	opaque = map[string]string{}
	bodies := map[string]string{} // bodyLinks per helper, once
	for _, sg := range sc.segments() {
		words := shellWords(sc.plain[sg.start:sg.end])
		// a link is recorded with every bound variable expanded: a partly
		// expanded word would stand for another function's variable too
		forms, whole := expandAll(words, vars)
		if forms == nil || !whole {
			forms = append(forms, words) // the words as written, compared as text
		}
		for _, exp := range forms {
			k := commandAt(exp)
			if k >= len(exp) {
				continue
			}
			name, args := path.Base(exp[k]), exp[k+1:]
			flag, isFn := g.metadata[name]
			lnArgs := args
			switch {
			case isFn && flag == "":
				bound, binds, ok := g.bindHelper(name, args, depth)
				if !ok {
					file, seen := bodies[name]
					if !seen {
						file = g.bodyLinks(name, vars, dirs, depth)
						bodies[name] = file
					}
					if file != "" {
						opaque[name] = file
					}
					continue
				}
				bsc := scanShell(bound)
				sub := g
				sub.inlines++
				inner, innerOpaque := sub.linksMade(bound, mergeVars(mergeVars(vars, binds), scanVars(bsc, bsc.segments())), dirs, depth+1)
				links = append(links, inner...)
				for k, v := range innerOpaque {
					opaque[k] = v
				}
				continue
			case isFn:
				if lnArgs = linkArgs(name, args, g.metadata); lnArgs == nil {
					continue
				}
			case name != "ln" && name != linkCmd:
				continue
			}
			if made, file, ok := g.files.linkPlan(lnArgs, dirs); file != "" && ok {
				links = append(links, made...)
			} else if file == "" {
				links = append(links, g.chained(lnArgs, dirs, links)...)
			}
		}
	}
	return links, opaque
}

// chained returns the links ln's args make to a link made on the command
// line, or to a directory above one: each exposes the guarded file as well.
func (g secretGuard) chained(args, dirs []string, known []madeLink) []madeLink {
	known = slices.Concat(g.made, known)
	if g.sink != nil {
		known = slices.Concat(known, *g.sink)
	}
	if len(known) == 0 {
		return nil
	}
	sources, dest, into, parsed := lnOperands(args)
	if !parsed {
		return nil
	}
	var out []madeLink
	for _, d := range dirs {
		for _, s := range sources {
			p := wordPath(s, d)
			for _, k := range known {
				rel := ""
				switch {
				case p == "" || k.path == "":
					continue
				case p == k.path:
				case strings.HasPrefix(k.path, p+string(filepath.Separator)):
					rel = strings.TrimPrefix(k.path, p)
				default:
					continue
				}
				l, good := linkAt(s, dest, into, d)
				if !good {
					continue
				}
				if rel != "" {
					l.path, l.word = l.path+rel, normWord(l.word+rel)
				}
				l.file = k.file
				out = append(out, l)
			}
		}
	}
	return out
}

// bodyLinks returns a guarded file a link in the helper name's body names
// with the body's own words, "" for none.
func (g secretGuard) bodyLinks(name string, vars map[string][]string, dirs []string, depth int) string {
	body, ok := g.funcs[name]
	if !ok {
		return ""
	}
	sub := g
	sub.funcs = nil // no further inlining from here
	sub.metadata = map[string]string{}
	bsc := scanShell(body)
	links, _ := sub.linksMade(body, mergeVars(vars, scanVars(bsc, bsc.segments())), dirs, depth)
	if len(links) > 0 {
		return links[0].file
	}
	return ""
}

// expandAll returns the forms of words with every bound variable expanded,
// across words and through chains of variables, at most maxForms of them;
// whole is false when the bound cut some off.
func expandAll(words []string, vars map[string][]string) (out [][]string, whole bool) {
	if len(vars) == 0 {
		return nil, true
	}
	whole = true
	names := make([]string, 0, len(vars))
	for n := range vars {
		names = append(names, n)
	}
	sort.Strings(names)
	out = [][]string{nil}
	for _, w := range words {
		forms := []string{w}
		for range 3 {
			var next []string
			for _, f := range forms {
				grown := false
				for _, n := range names {
					if !strings.Contains(f, "$") || !hasRef(f, n) {
						continue
					}
					for _, v := range vars[n] {
						next = append(next, replaceRef(f, n, v))
					}
					grown = true
					break
				}
				if !grown {
					next = append(next, f)
				}
			}
			if forms = slices.Compact(slices.Sorted(slices.Values(next))); len(forms) > maxForms {
				forms, whole = forms[:maxForms], false
			}
		}
		var grown [][]string
		for _, o := range out {
			for _, f := range forms {
				if len(grown) >= maxForms {
					whole = false
					break
				}
				grown = append(grown, append(slices.Clone(o), f))
			}
		}
		out = grown
	}
	return out, whole
}

// refAt returns the length of a reference to the variable name at s[i],
// a $, as $name or ${name}; 0 for none.
func refAt(s string, i int, name string) int {
	rest := s[i+1:]
	if strings.HasPrefix(rest, "{"+name+"}") {
		return len(name) + 3
	}
	if strings.HasPrefix(rest, name) {
		if j := len(name); j == len(rest) || !isNameByte(rest[j]) {
			return len(name) + 1
		}
	}
	return 0
}

// isNameByte reports whether c may continue a shell variable name.
func isNameByte(c byte) bool {
	return c == '_' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

func hasRef(s, name string) bool {
	for i := strings.IndexByte(s, '$'); i >= 0; {
		if refAt(s, i, name) > 0 {
			return true
		}
		j := strings.IndexByte(s[i+1:], '$')
		if j < 0 {
			break
		}
		i += j + 1
	}
	return false
}

// replaceRef replaces every reference to the variable name in s with val.
func replaceRef(s, name, val string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '$' {
			if n := refAt(s, i, name); n > 0 {
				b.WriteString(val)
				i += n - 1
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// boundScan scans the inlined body of a helper call with its caller's
// bindings, functions and made links.
func (g secretGuard) boundScan(body string, binds map[string][]string, depth int) *leak {
	g.bound, g.vars = true, mergeVars(g.vars, binds)
	g.inlines++
	return g.scan(body, depth+1)
}

func mergeFuncs(outer, inner map[string]string) map[string]string {
	out := make(map[string]string, len(outer)+len(inner))
	for k, v := range outer {
		out[k] = v
	}
	for k, v := range inner {
		out[k] = v
	}
	return out
}

func mergeVars(outer, inner map[string][]string) map[string][]string {
	out := make(map[string][]string, len(outer)+len(inner))
	for k, v := range outer {
		out[k] = slices.Clone(v)
	}
	for k, v := range inner {
		out[k] = append(out[k], v...)
	}
	return out
}

// functionBodies returns the body text of each function cmd defines once;
// a function defined twice has none, so its calls are never inlined.
func functionBodies(cmd string) map[string]string {
	f, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil {
		return nil
	}
	out := map[string]string{}
	seen := map[string]bool{}
	syntax.Walk(f, func(n syntax.Node) bool {
		fn, ok := n.(*syntax.FuncDecl)
		if !ok {
			return true
		}
		name := fn.Name.Value
		if seen[name] {
			delete(out, name)
			return true
		}
		seen[name] = true
		var b bytes.Buffer
		if err := syntax.NewPrinter().Print(&b, fn.Body); err == nil {
			out[name] = b.String()
		}
		return true
	})
	return out
}

// bindHelper returns the body of the helper name with its positional
// parameters replaced by args, and the bindings of its assignments: a
// declaration (local src="$1") or plain assignment keeps only its name in the
// text, so naming a guarded file there is no read, and its value is bound for
// the scan that judges what the call does. A value that runs a command
// (local v="$(cat "$1")") keeps its text, so that scan judges the command. ok is false when the call cannot
// be inlined: too deep, ln shadowed, an argument that is no plain word, or a
// body that uses its parameters in a way the text does not carry ($@, $#,
// ${1:-x}, shift, set, a declaration with options or arrays).
func (g secretGuard) bindHelper(name string, args []string, depth int) (string, map[string][]string, bool) {
	body, ok := g.funcs[name]
	if _, shadow := g.metadata["ln"]; !ok || shadow || g.inlines >= maxInline || depth >= maxScanDepth {
		return "", nil, false
	}
	for _, a := range args {
		if !safeArg.MatchString(a) {
			return "", nil, false
		}
	}
	f, err := syntax.NewParser().Parse(strings.NewReader(body), "")
	if err != nil {
		return "", nil, false
	}
	good := true
	positional := func(parts []syntax.WordPart) {
		for i, p := range parts {
			pe, isParam := p.(*syntax.ParamExp)
			if !isParam || pe.Param == nil {
				continue
			}
			v := pe.Param.Value
			switch {
			case v == "@" || v == "*" || v == "#":
				good = false
			case len(v) == 1 && v[0] >= '1' && v[0] <= '9':
				if pe.Excl || pe.Length || pe.Width || pe.Index != nil || pe.Slice != nil || pe.Repl != nil || pe.Names != 0 || pe.Exp != nil {
					good = false
					continue
				}
				arg := ""
				if n := int(v[0] - '0'); n <= len(args) {
					arg = args[n-1]
				}
				parts[i] = &syntax.Lit{Value: arg}
			}
		}
	}
	syntax.Walk(f, func(n syntax.Node) bool {
		switch node := n.(type) {
		case *syntax.Word:
			positional(node.Parts)
		case *syntax.DblQuoted:
			positional(node.Parts)
		case *syntax.CallExpr:
			if len(node.Args) > 0 {
				switch node.Args[0].Lit() {
				case "shift", verbSet, "getopts", "unset":
					good = false
				}
			}
		case *syntax.DeclClause:
			for _, a := range node.Args {
				if a.Name == nil || a.Array != nil || a.Index != nil || a.Append {
					good = false
				}
			}
		}
		return good
	})
	if !good {
		return "", nil, false
	}
	binds := map[string][]string{}
	bind := func(as []*syntax.Assign) {
		for _, a := range as {
			if a.Name == nil || a.Value == nil || a.Array != nil || a.Index != nil || a.Append {
				good = good && a.Name != nil && a.Array == nil && a.Index == nil && !a.Append
				continue
			}
			var b bytes.Buffer
			if err := syntax.NewPrinter().Print(&b, a.Value); err != nil {
				good = false
				continue
			}
			if runsCommand(a.Value) {
				// a value that runs a command keeps its text: the scan of
				// the bound body judges the command with the arguments bound
				continue
			}
			binds[a.Name.Value] = append(binds[a.Name.Value], strings.Join(shellWords(b.String()), " "))
			a.Value, a.Naked = nil, true
		}
	}
	syntax.Walk(f, func(n syntax.Node) bool {
		switch node := n.(type) {
		case *syntax.DeclClause:
			bind(node.Args)
		case *syntax.CallExpr:
			if len(node.Args) == 0 {
				bind(node.Assigns)
			}
		}
		return true
	})
	var b bytes.Buffer
	if err := syntax.NewPrinter().Print(&b, f); err != nil || !good {
		return "", nil, false
	}
	return b.String(), binds, true
}

// runsCommand reports whether word w runs a command when it expands: a command or
// process substitution anywhere in it, also inside a parameter's default.
func runsCommand(w *syntax.Word) bool {
	found := false
	syntax.Walk(w, func(n syntax.Node) bool {
		switch n.(type) {
		case *syntax.CmdSubst, *syntax.ProcSubst:
			found = true
		}
		return !found
	})
	return found
}

// sameGuarded returns the guarded file that c is a hard link of, "" for
// none: an existing regular file that shares a guarded file's inode.
func (f secretFiles) sameGuarded(c string) string {
	fi, err := os.Stat(c)
	if err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	for _, g := range f.globs {
		m := []string{g}
		if strings.ContainsAny(g, "*?[") {
			m, _ = filepath.Glob(g)
		}
		for _, file := range m {
			if gi, err := os.Stat(file); err == nil && os.SameFile(fi, gi) {
				return file
			}
		}
	}
	return ""
}
