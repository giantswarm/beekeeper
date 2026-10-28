package guard

import (
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/giantswarm/beekeeper/internal/post"
)

const (
	ghIssueSub   = "issue"
	hereStringOp = "<<<"
)

var (
	// ghBodyVerbs are the gh pr and issue verbs whose bodies are checked for
	// unlinked #n and owner/repo#n references.
	ghBodyVerbs = map[string]map[string]bool{
		"pr":       {verbCreate: true, verbEdit: true, verbComment: true},
		ghIssueSub: {verbCreate: true, verbEdit: true, verbComment: true},
	}
	declBuiltins   = wordSet("export local declare typeset read" + "only")
	copyFileCmds   = wordSet("cp mv in" + "stall")
	deleteFileCmds = wordSet("rm unlink trun" + "cate")
)

type ghValue struct {
	text string
	ok   bool
}

type ghToken struct {
	raw        string
	start, end int
	redir      string
}

type ghBodyState struct {
	hook        Hook
	cmd         string
	rawSc       shellScan
	segs        []segment
	states      []segmentState
	extraDocs   []heredoc
	vars        map[string]ghValue
	files       map[string]ghValue
	taintedFile bool
	depth       int
}

// ghBodyRefusal refuses a gh pr or issue create, edit or comment command
// whose --body or --body-file names a bare #n or owner/repo#n without its
// full GitHub URL, or whose body source cannot be statically inspected.
func (h Hook) ghBodyRefusal(cmd, cwd string) string {
	return h.scanGHBody(cmd, cwd, false, 0, nil, nil, nil)
}

func (h Hook) scanGHBody(cmd, cwd string, forceUnknown bool, depth int, parentVars, parentFiles map[string]ghValue, extraDocs []heredoc) string {
	if depth > 4 {
		return ""
	}
	st := h.newGHBodyState(cmd, cwd, forceUnknown, depth, parentVars, parentFiles, extraDocs)
	for i := range st.segs {
		if r := st.stepSegment(i, forceUnknown); r != "" {
			return r
		}
	}
	return ""
}

func (h Hook) newGHBodyState(cmd, cwd string, forceUnknown bool, depth int, parentVars, parentFiles map[string]ghValue, extraDocs []heredoc) *ghBodyState {
	rawSc := scanShell(cmd)
	topSc := maskSubstitutions(rawSc)
	outerPlain, inners, hadSubst := opaquePlain(rawSc.plain)
	sc := scanShell(outerPlain)
	sc.heredocs = rawSc.heredocs
	segs := topSc.segments()
	states := walkSegments(sc, cwd, h.Upstream.Git != nil)
	anyCD := hasAnyCD(rawSc, 0) || hasAnyCD(sc, 0)
	for _, in := range inners {
		if hasAnyCD(scanShell(in), 0) {
			anyCD = true
		}
	}
	lineMoved := (!isPlain(sc) || hadSubst) && anyCD
	for idx := range states {
		if lineMoved || forceUnknown {
			states[idx].known = false
		}
	}
	vars := map[string]ghValue{}
	maps.Copy(vars, parentVars)
	files := map[string]ghValue{}
	maps.Copy(files, parentFiles)
	return &ghBodyState{
		hook:      h,
		cmd:       cmd,
		rawSc:     rawSc,
		segs:      segs,
		states:    states,
		extraDocs: extraDocs,
		vars:      vars,
		files:     files,
		depth:     depth,
	}
}

func (st *ghBodyState) segState(i int) (dir string, known bool) {
	if i < len(st.states) {
		return st.states[i].dir, st.states[i].known
	}
	if len(st.states) > 0 {
		last := st.states[len(st.states)-1]
		return last.dir, last.known
	}
	return "", false
}

func (st *ghBodyState) stepSegment(i int, forceUnknown bool) string {
	sg := st.segs[i]
	dir, known := st.segState(i)
	if forceUnknown {
		known = false
	}
	tokens := tokenizeGHSegment(st.rawSc.plain[sg.start:sg.end], sg.start)
	words, redirs := splitGHTokens(tokens)
	segVars := st.applySegmentVars(words, dir, known)
	st.applySegmentWrites(i, words, redirs, dir, known, segVars)

	if len(words) == 0 {
		return ""
	}
	if r := st.checkGHSegment(i, words, redirs, dir, known, segVars); r != "" {
		return r
	}
	return st.scanNestedShell(i, words, dir, known, segVars)
}

func (st *ghBodyState) scanNestedShell(segIdx int, words []ghToken, dir string, known bool, segVars map[string]ghValue) string {
	evalWords := make([]string, 0, len(words))
	for _, w := range words {
		v, ok := st.evalRawToken(w, dir, known, segVars, false)
		if !ok {
			v = unknownMarker
		}
		evalWords = append(evalWords, v)
	}
	if envCmd := envSplitCommand(evalWords); envCmd != "" {
		if r := st.hook.scanGHBody(envCmd, dir, !known, st.depth+1, segVars, st.files, nil); r != "" {
			return r
		}
	}
	if flockCmd := flockCommand(evalWords); flockCmd != "" {
		if r := st.hook.scanGHBody(flockCmd, dir, !known, st.depth+1, segVars, st.files, nil); r != "" {
			return r
		}
	}
	k := commandAt(evalWords)
	if k == len(evalWords) {
		return ""
	}
	base := path.Base(evalWords[k])
	if base == evalCmd {
		if r := st.hook.scanGHBody(strings.Join(evalWords[k+1:], " "), dir, !known, st.depth+1, segVars, st.files, nil); r != "" {
			return r
		}
	}
	if !shells[base] {
		return ""
	}
	for _, arg := range evalWords[k+1:] {
		if strings.ContainsAny(arg, " \t\n") {
			if r := st.hook.scanGHBody(arg, dir, !known, st.depth+1, segVars, st.files, nil); r != "" {
				return r
			}
		}
	}
	sg := st.segs[segIdx]
	for _, hd := range st.heredocsFor(sg.start, sg.end) {
		if r := st.hook.scanGHBody(st.cmd[hd.start:hd.end], dir, !known, st.depth+1, segVars, st.files, nil); r != "" {
			return r
		}
	}
	return ""
}

func (st *ghBodyState) heredocsFor(start, end int) []heredoc {
	var out []heredoc
	for _, hd := range st.rawSc.heredocs {
		if hd.op >= start && hd.op < end {
			out = append(out, hd)
		}
	}
	out = append(out, st.extraDocs...)
	return out
}

func (st *ghBodyState) applySegmentVars(words []ghToken, dir string, known bool) map[string]ghValue {
	idx := 0
	for idx < len(words) && compoundWords[words[idx].raw] && words[idx].raw != forWord {
		idx++
	}
	if idx+3 <= len(words) && words[idx].raw == forWord && isValidVarName(words[idx+1].raw) {
		name := words[idx+1].raw
		if idx+2 < len(words) && words[idx+2].raw == "in" {
			var vals []string
			allOK := true
			for _, item := range words[idx+3:] {
				v, ok := st.evalRawToken(item, dir, known, st.vars, false)
				if !ok {
					allOK = false
					break
				}
				vals = append(vals, v)
			}
			st.vars[name] = ghValue{text: strings.Join(vals, "\n"), ok: allOK}
		} else {
			st.vars[name] = ghValue{ok: false}
		}
		return maps.Clone(st.vars)
	}
	decl, nameref := parseDeclPrefix(words, &idx)
	segVars := maps.Clone(st.vars)
	assignStart := idx
	for idx < len(words) {
		name, rawVal, ok := cutAssignmentToken(words[idx])
		if !ok {
			break
		}
		if nameref {
			segVars[name] = ghValue{ok: false}
			st.vars[name] = ghValue{ok: false}
		} else {
			val, valOK := st.evalRawToken(rawVal, dir, known, segVars, false)
			segVars[name] = ghValue{text: val, ok: valOK}
			if decl {
				st.vars[name] = ghValue{text: val, ok: valOK}
			}
		}
		idx++
	}
	if idx == len(words) && idx > assignStart {
		maps.Copy(st.vars, segVars)
	}
	return segVars
}

func parseDeclPrefix(words []ghToken, idx *int) (decl, nameref bool) {
	if *idx >= len(words) {
		return false, false
	}
	if declBuiltins[words[*idx].raw] {
		decl = true
		*idx++
		for *idx < len(words) && strings.HasPrefix(words[*idx].raw, "-") {
			flag := words[*idx].raw
			if strings.Contains(flag, "n") || strings.Contains(flag, "i") {
				nameref = true
			}
			*idx++
		}
	}
	return decl, nameref
}

func cutAssignmentToken(tok ghToken) (name string, valTok ghToken, ok bool) {
	eq := strings.IndexByte(tok.raw, '=')
	if eq <= 0 {
		return "", ghToken{}, false
	}
	name = tok.raw[:eq]
	if !isValidVarName(name) {
		return "", ghToken{}, false
	}
	return name, ghToken{
		raw:   tok.raw[eq+1:],
		start: tok.start + eq + 1,
		end:   tok.end,
	}, true
}

func (st *ghBodyState) applySegmentWrites(segIdx int, words, redirs []ghToken, dir string, known bool, segVars map[string]ghValue) {
	var outRedirs []ghToken
	for _, r := range redirs {
		if strings.Contains(r.redir, ">") {
			outRedirs = append(outRedirs, r)
		}
	}
	if len(outRedirs) > 0 {
		outText, outOK := st.evalSegmentStdout(segIdx, words, redirs, dir, known, segVars)
		for _, r := range outRedirs {
			target, ok := st.evalRawToken(r, dir, known, segVars, false)
			if strings.HasPrefix(target, "/dev/") {
				continue
			}
			if strings.HasPrefix(r.redir, "2>") {
				st.recordFileWrite(target, dir, known, "", false, false)
				continue
			}
			appendMode := strings.HasSuffix(r.redir, ">>")
			st.recordFileWrite(target, dir, known, outText, ok && outOK, appendMode)
		}
	}
	st.applyCommandFileWrites(segIdx, words, redirs, dir, known, segVars)
}

func (st *ghBodyState) applyCommandFileWrites(segIdx int, words, redirs []ghToken, dir string, known bool, segVars map[string]ghValue) {
	evalWords, allOK := st.evalAllTokens(words, dir, known, segVars)
	k := commandAt(evalWords)
	if k >= len(evalWords) {
		return
	}
	name, args := path.Base(evalWords[k]), evalWords[k+1:]
	switch {
	case name == teeCmd:
		stdinText, stdinOK := st.evalSegmentStdin(segIdx, redirs, dir, known, segVars)
		appendMode := false
		for _, a := range args {
			if a == "-a" || a == "--append" {
				appendMode = true
				continue
			}
			if strings.HasPrefix(a, "-") {
				continue
			}
			st.recordFileWrite(a, dir, known, stdinText, allOK && stdinOK, appendMode)
		}
	case copyFileCmds[name]:
		non := nonFlags(args)
		if len(non) == 2 && allOK {
			srcText, srcOK := st.readBodyFile(non[0], dir, known)
			st.recordFileWrite(non[1], dir, known, srcText, srcOK, false)
		} else if len(non) >= 2 {
			st.recordFileWrite(non[len(non)-1], dir, known, "", false, false)
		}
	case name == "dd":
		for _, a := range args {
			if dst, ok := strings.CutPrefix(a, "of="); ok {
				st.recordFileWrite(dst, dir, known, "", false, false)
			}
		}
	case deleteFileCmds[name]:
		for _, a := range nonFlags(args) {
			st.recordFileWrite(a, dir, known, "", false, false)
		}
	case name == "sed" || name == "perl":
		for _, a := range args {
			if strings.HasPrefix(a, "-i") || a == "--in-place" || strings.HasPrefix(a, "-pi") {
				for _, f := range nonFlags(args) {
					st.recordFileWrite(f, dir, known, "", false, false)
				}
				break
			}
		}
	}
}

func (st *ghBodyState) recordFileWrite(target, dir string, known bool, content string, ok, appendMode bool) {
	if target == "" || strings.Contains(target, unknownMarker) {
		st.taintedFile = true
		return
	}
	expanded := expandHome(target)
	if !known && !filepath.IsAbs(expanded) {
		st.taintedFile = true
		return
	}
	abs := filepath.Clean(resolve(dir, target))
	if !appendMode {
		st.files[abs] = ghValue{text: content, ok: ok}
		return
	}
	if !ok {
		st.files[abs] = ghValue{ok: false}
		return
	}
	prev, prevOK := st.readBodyFile(target, dir, known)
	if !prevOK {
		if _, err := os.Stat(abs); os.IsNotExist(err) {
			prev, prevOK = "", true
		}
	}
	if !prevOK {
		st.files[abs] = ghValue{ok: false}
		return
	}
	st.files[abs] = ghValue{text: prev + content, ok: true}
}

func (st *ghBodyState) checkGHSegment(segIdx int, words, redirs []ghToken, dir string, known bool, segVars map[string]ghValue) string {
	rawStrs := make([]string, len(words))
	for i, w := range words {
		if v, ok := st.evalRawToken(w, dir, known, segVars, false); ok {
			rawStrs[i] = v
		} else {
			rawStrs[i] = w.raw
		}
	}
	k := commandAt(rawStrs)
	if k >= len(words) || path.Base(rawStrs[k]) != "gh" {
		return ""
	}
	env := ""
	for _, w := range rawStrs[:k] {
		if v, ok := strings.CutPrefix(w, "GH_REPO="); ok {
			env = v
		}
	}
	argTokens := words[k+1:]
	argStrs := rawStrs[k+1:]
	var pos []string
	var bodyTokens, fileTokens []ghToken
	incompleteFlag := ""

	for i := 0; i < len(argTokens); i++ {
		a := argStrs[i]
		tok := argTokens[i]
		if _, ok := repoFlag(a); ok {
			continue
		}
		if _, ok := strings.CutPrefix(tok.raw, "--body="); ok {
			bodyTokens = append(bodyTokens, sliceTokenAfter(tok, len("--body=")))
			continue
		}
		if v, ok := strings.CutPrefix(tok.raw, "-b"); ok && v != "" && !strings.HasPrefix(tok.raw, "--") {
			cut := 2
			if strings.HasPrefix(v, "=") {
				cut = 3
			}
			bodyTokens = append(bodyTokens, sliceTokenAfter(tok, cut))
			continue
		}
		if _, ok := strings.CutPrefix(tok.raw, flagBodyFile+"="); ok {
			fileTokens = append(fileTokens, sliceTokenAfter(tok, len(flagBodyFile+"=")))
			continue
		}
		if v, ok := strings.CutPrefix(tok.raw, "-F"); ok && v != "" && !strings.HasPrefix(tok.raw, "--") {
			cut := 2
			if strings.HasPrefix(v, "=") {
				cut = 3
			}
			fileTokens = append(fileTokens, sliceTokenAfter(tok, cut))
			continue
		}
		if ghValued[a] {
			isBodyFlag := a == "-b" || strings.TrimPrefix(a, "--") == "body"
			isFileFlag := a == "-F" || a == flagBodyFile
			if i+1 >= len(argTokens) {
				if isBodyFlag || isFileFlag {
					incompleteFlag = a
				}
				break
			}
			if isBodyFlag {
				bodyTokens = append(bodyTokens, argTokens[i+1])
			} else if isFileFlag {
				fileTokens = append(fileTokens, argTokens[i+1])
			}
			i++
			continue
		}
		if !strings.HasPrefix(a, "-") {
			pos = append(pos, a)
		}
	}

	if len(pos) < 2 || !ghBodyVerbs[pos[0]][pos[1]] {
		return ""
	}
	if len(bodyTokens) == 0 && len(fileTokens) == 0 && incompleteFlag == "" {
		return ""
	}
	what, repo := st.hook.Upstream.ghWrite(argStrs, dir, env, known)
	if what == "" {
		what = "gh " + pos[0] + " " + pos[1]
	}
	if incompleteFlag != "" {
		return uninspectableGHBodyReason(what, incompleteFlag+" has no value")
	}

	var parts []string
	for _, bt := range bodyTokens {
		val, ok := st.evalRawToken(bt, dir, known, segVars, true)
		if !ok {
			return uninspectableGHBodyReason(what, "an unresolved variable or command substitution in --body")
		}
		parts = append(parts, val)
	}
	for _, ft := range fileTokens {
		part, why := st.resolveBodyFileToken(segIdx, ft, redirs, dir, known, segVars)
		if why != "" {
			return uninspectableGHBodyReason(what, why)
		}
		parts = append(parts, part)
	}

	problems := post.Unlinked(strings.Join(parts, "\n"), repoSlug(repo))
	if len(problems) == 0 {
		return ""
	}
	return "Refused: the body of `" + what + "` names an issue or pull request without its full URL: " + strings.Join(problems, "; ") + "."
}

func sliceTokenAfter(tok ghToken, prefixLen int) ghToken {
	return ghToken{
		raw:   tok.raw[prefixLen:],
		start: tok.start + prefixLen,
		end:   tok.end,
	}
}

func uninspectableGHBodyReason(what, detail string) string {
	return "Refused: the guard cannot inspect the body of `" + what + "` (" + detail + ") to check its issue and pull request URLs: " +
		"supply an existing readable file with `--body-file <file>` or pass a literal `--body`."
}

func (st *ghBodyState) resolveBodyFileToken(segIdx int, ft ghToken, redirs []ghToken, dir string, known bool, segVars map[string]ghValue) (content, why string) {
	if strings.HasPrefix(ft.raw, "<(") && strings.HasSuffix(ft.raw, ")") && ft.end > ft.start+3 {
		val, ok := st.evalSubst(ft.start+2, ft.end-1, dir, known, segVars)
		if !ok {
			return "", "process substitution for --body-file cannot be statically resolved"
		}
		return val, ""
	}
	file, ok := st.evalRawToken(ft, dir, known, segVars, false)
	if !ok || file == "" {
		return "", "unresolved --body-file path"
	}
	if file == "-" {
		val, stdinOK := st.evalSegmentStdin(segIdx, redirs, dir, known, segVars)
		if !stdinOK {
			return "", "stdin for --body-file - cannot be statically resolved"
		}
		return val, ""
	}
	val, readOK := st.readBodyFile(file, dir, known)
	if !readOK {
		return "", "--body-file " + file + " cannot be read"
	}
	return val, ""
}

func (st *ghBodyState) evalSegmentStdin(segIdx int, redirs []ghToken, dir string, known bool, segVars map[string]ghValue) (string, bool) {
	sg := st.segs[segIdx]
	docs := st.heredocsFor(sg.start, sg.end)
	if len(docs) > 0 {
		var parts []string
		for _, hd := range docs {
			val, ok := st.evalHeredoc(hd, dir, known, segVars)
			if !ok {
				return "", false
			}
			parts = append(parts, val)
		}
		return strings.Join(parts, ""), true
	}
	for i := len(redirs) - 1; i >= 0; i-- {
		r := redirs[i]
		switch r.redir {
		case hereStringOp:
			val, ok := st.evalRawToken(r, dir, known, segVars, false)
			if !ok {
				return "", false
			}
			return val + "\n", true
		case "<", "0<":
			file, ok := st.evalRawToken(r, dir, known, segVars, false)
			if !ok || file == "" {
				return "", false
			}
			return st.readBodyFile(file, dir, known)
		}
	}
	if segIdx > 0 && st.segs[segIdx-1].after == "|" {
		prevSg := st.segs[segIdx-1]
		prevDir, prevKnown := st.segState(segIdx - 1)
		prevTokens := tokenizeGHSegment(st.rawSc.plain[prevSg.start:prevSg.end], prevSg.start)
		prevWords, prevRedirs := splitGHTokens(prevTokens)
		return st.evalSegmentStdout(segIdx-1, prevWords, prevRedirs, prevDir, prevKnown, segVars)
	}
	return "", false
}

func (st *ghBodyState) evalSegmentStdout(segIdx int, words, redirs []ghToken, dir string, known bool, segVars map[string]ghValue) (string, bool) {
	if len(words) == 0 {
		for _, r := range redirs {
			if r.redir == "<" || r.redir == "0<" {
				file, ok := st.evalRawToken(r, dir, known, segVars, false)
				if !ok || file == "" {
					return "", false
				}
				return st.readBodyFile(file, dir, known)
			}
		}
		return "", false
	}
	evalWords, allOK := st.evalAllTokens(words, dir, known, segVars)
	if !allOK {
		return "", false
	}
	k := commandAt(evalWords)
	if k >= len(evalWords) {
		return "", false
	}
	name, args := path.Base(evalWords[k]), evalWords[k+1:]
	switch name {
	case "print" + "f":
		return evalPrintfArgs(args)
	case "echo":
		return evalEchoArgs(args)
	case "cat":
		return st.evalCatArgs(segIdx, args, redirs, dir, known, segVars)
	case teeCmd:
		return st.evalSegmentStdin(segIdx, redirs, dir, known, segVars)
	default:
		return "", false
	}
}

func (st *ghBodyState) evalCatArgs(segIdx int, args []string, redirs []ghToken, dir string, known bool, segVars map[string]ghValue) (string, bool) {
	var files []string
	for _, a := range args {
		if a == "--" || a == "-u" {
			continue
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			return "", false
		}
		files = append(files, a)
	}
	if len(files) == 0 {
		return st.evalSegmentStdin(segIdx, redirs, dir, known, segVars)
	}
	var b strings.Builder
	for _, f := range files {
		var part string
		var ok bool
		if f == "-" {
			part, ok = st.evalSegmentStdin(segIdx, redirs, dir, known, segVars)
		} else {
			part, ok = st.readBodyFile(f, dir, known)
		}
		if !ok {
			return "", false
		}
		b.WriteString(part)
	}
	return b.String(), true
}

func (st *ghBodyState) evalAllTokens(words []ghToken, dir string, known bool, segVars map[string]ghValue) ([]string, bool) {
	out := make([]string, len(words))
	for i, w := range words {
		v, ok := st.evalRawToken(w, dir, known, segVars, false)
		if !ok {
			return nil, false
		}
		out[i] = v
	}
	return out, true
}

func (st *ghBodyState) readBodyFile(name, dir string, known bool) (string, bool) {
	if name == "" || name == "-" || strings.Contains(name, unknownMarker) || st.taintedFile {
		return "", false
	}
	expanded := expandHome(name)
	if !known && !filepath.IsAbs(expanded) {
		return "", false
	}
	abs := filepath.Clean(resolve(dir, name))
	if written, exists := st.files[abs]; exists {
		return written.text, written.ok
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		return "", false
	}
	f, err := os.Open(abs) //nolint:gosec // reading a file the guarded command itself sends
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, pushLimit))
	if err != nil {
		return "", false
	}
	return string(b), true
}

func (st *ghBodyState) evalHeredoc(hd heredoc, dir string, known bool, segVars map[string]ghValue) (string, bool) {
	body := st.cmd[hd.start:hd.end]
	if hd.op < len(st.cmd) {
		if m := heredocOp.FindStringSubmatch(st.cmd[hd.op:]); m != nil && (m[2] != "" || m[3] != "") {
			return body, true // quoted delimiter: no parameter or command expansion
		}
	}
	return st.expandUnquotedText(body, hd.start, dir, known, segVars)
}

func (st *ghBodyState) expandUnquotedText(s string, base int, dir string, known bool, segVars map[string]ghValue) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			if s[i+1] == '\n' {
				i += 2
				continue
			}
			if strings.IndexByte(`\$`+"`", s[i+1]) >= 0 {
				b.WriteByte(s[i+1])
				i += 2
				continue
			}
			b.WriteByte(c)
			i++
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			if i+2 < len(s) && s[i+2] == '(' {
				return "", false
			}
			end := findClosingParen(s, i+1)
			val, ok := st.evalSubst(base+i+2, base+end, dir, known, segVars)
			if !ok {
				return "", false
			}
			b.WriteString(strings.TrimRight(val, "\n"))
			i = min(end+1, len(s))
		case c == '$':
			val, n, ok := evalVarRef(s[i:], segVars)
			if !ok {
				return "", false
			}
			b.WriteString(val)
			i += n
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), true
}

func (st *ghBodyState) evalSubst(subStart, subEnd int, dir string, known bool, segVars map[string]ghValue) (string, bool) {
	if st.depth > 4 || subStart < 0 || subEnd < subStart || subEnd > len(st.cmd) {
		return "", false
	}
	var parentDocs []heredoc
	for _, hd := range st.rawSc.heredocs {
		if hd.op >= subStart && hd.op < subEnd && hd.end > subEnd {
			parentDocs = append(parentDocs, hd)
		}
	}
	innerCmd := st.cmd[subStart:subEnd]
	if len(parentDocs) > 0 {
		innerCmd = st.rawSc.plain[subStart:subEnd]
	}
	child := st.hook.newGHBodyState(innerCmd, dir, !known, st.depth+1, segVars, st.files, parentDocs)
	child.taintedFile = st.taintedFile
	if len(child.segs) == 0 {
		return "", false
	}
	var lastOut string
	var lastOK bool
	for i, sg := range child.segs {
		cDir, cKnown := child.segState(i)
		if !known {
			cKnown = false
		}
		tokens := tokenizeGHSegment(child.rawSc.plain[sg.start:sg.end], sg.start)
		words, redirs := splitGHTokens(tokens)
		if len(words) == 0 && len(redirs) == 0 {
			continue
		}
		cSegVars := child.applySegmentVars(words, cDir, cKnown)
		child.applySegmentWrites(i, words, redirs, cDir, cKnown, cSegVars)
		if len(words) == 0 && len(redirs) > 0 {
			lastOut, lastOK = child.evalSegmentStdout(i, words, redirs, cDir, cKnown, cSegVars)
			continue
		}
		if len(words) > 0 && sg.after != "|" {
			lastOut, lastOK = child.evalSegmentStdout(i, words, redirs, cDir, cKnown, cSegVars)
		}
	}
	return lastOut, lastOK
}

func (st *ghBodyState) evalRawToken(tok ghToken, dir string, known bool, segVars map[string]ghValue, allowMarkdownBackticks bool) (string, bool) {
	s := tok.raw
	base := tok.start
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '$' && i+1 < len(s) && s[i+1] == '$':
			b.WriteString("$$")
			i += 2
		case c == '$' && i+1 < len(s) && s[i+1] == '\'':
			for i += 2; i < len(s) && s[i] != '\''; {
				if s[i] != '\\' {
					b.WriteByte(s[i])
					i++
					continue
				}
				decoded, n := ansiC(s[i:])
				b.Write(decoded)
				i += n
			}
			if i < len(s) {
				i++
			}
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				b.WriteString(s[i+1:])
				i = len(s)
			} else {
				b.WriteString(s[i+1 : i+1+j])
				i += j + 2
			}
		case c == '"':
			end, ok := st.evalDoubleQuoted(s, i, base, dir, known, segVars, allowMarkdownBackticks, &b)
			if !ok {
				return "", false
			}
			i = end
		case c == '\\' && i+1 < len(s):
			if s[i+1] != '\n' {
				b.WriteByte(s[i+1])
			}
			i += 2
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			if i+2 < len(s) && s[i+2] == '(' {
				return "", false
			}
			end := findClosingParen(s, i+1)
			val, ok := st.evalSubst(base+i+2, base+end, dir, known, segVars)
			if !ok {
				return "", false
			}
			b.WriteString(strings.TrimRight(val, "\n"))
			i = min(end+1, len(s))
		case c == '`':
			end := skipBacktickSpan(s, i, false)
			val, ok := st.evalSubst(base+i+1, base+end, dir, known, segVars)
			if !ok {
				return "", false
			}
			b.WriteString(strings.TrimRight(val, "\n"))
			i = min(end+1, len(s))
		case (c == '<' || c == '>') && i+1 < len(s) && s[i+1] == '(':
			return "", false
		case c == '$':
			val, n, ok := evalVarRef(s[i:], segVars)
			if !ok {
				return "", false
			}
			b.WriteString(val)
			i += n
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), true
}

func (st *ghBodyState) evalDoubleQuoted(s string, quoteStart, base int, dir string, known bool, segVars map[string]ghValue, allowMarkdownBackticks bool, b *strings.Builder) (int, bool) {
	i := quoteStart + 1
	for i < len(s) && s[i] != '"' {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && s[i+1] == '\n':
			i += 2
		case c == '\\' && i+1 < len(s) && strings.IndexByte(`"\$`+"`", s[i+1]) >= 0:
			b.WriteByte(s[i+1])
			i += 2
		case c == '\\' && i+1 < len(s):
			b.WriteByte(c)
			i++
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			if i+2 < len(s) && s[i+2] == '(' {
				return 0, false
			}
			end := findClosingParen(s, i+1)
			val, ok := st.evalSubst(base+i+2, base+end, dir, known, segVars)
			if !ok {
				return 0, false
			}
			b.WriteString(strings.TrimRight(val, "\n"))
			i = min(end+1, len(s))
		case c == '`':
			ticks := 0
			for i+ticks < len(s) && s[i+ticks] == '`' {
				ticks++
			}
			if ticks >= 2 {
				b.WriteString(s[i : i+ticks])
				i += ticks
				continue
			}
			end := skipBacktickSpan(s, i, true)
			val, ok := st.evalSubst(base+i+1, base+end, dir, known, segVars)
			if ok {
				b.WriteString(strings.TrimRight(val, "\n"))
				i = min(end+1, len(s))
				continue
			}
			wholeWord := quoteStart == 0 && i == 1 && end+2 == len(s) && s[end+1] == '"'
			if !allowMarkdownBackticks || wholeWord {
				return 0, false
			}
			b.WriteString(s[i:min(end+1, len(s))])
			i = min(end+1, len(s))
		case c == '$':
			val, n, ok := evalVarRef(s[i:], segVars)
			if !ok {
				return 0, false
			}
			b.WriteString(val)
			i += n
		default:
			b.WriteByte(c)
			i++
		}
	}
	if i < len(s) {
		i++
	}
	return i, true
}

func skipBacktickSpan(s string, start int, inDouble bool) int {
	end := start + 1
	for end < len(s) && s[end] != '`' {
		if s[end] == '\\' && end+1 < len(s) && (s[end+1] == '`' || s[end+1] == '\\' || s[end+1] == '$' || (inDouble && s[end+1] == '"')) {
			end++
		}
		end++
	}
	return end
}

func evalVarRef(s string, vars map[string]ghValue) (val string, consumed int, ok bool) {
	if len(s) < 2 {
		return "$", 1, true
	}
	c := s[1]
	if c == ' ' || c == '\t' || c == '\n' || c == '"' || c == '\'' || c == '/' || c == ':' || c == ',' || c == ';' || c == ')' || c == ']' {
		return "$", 1, true
	}
	if c == '$' {
		return "$$", 2, true
	}
	if c == '{' {
		end := strings.IndexByte(s[2:], '}')
		if end < 0 {
			return "", len(s), false
		}
		name := s[2 : 2+end]
		if !isValidVarName(name) {
			return "", 2 + end + 1, false
		}
		v, exists := vars[name]
		if !exists || !v.ok {
			return "", 2 + end + 1, false
		}
		return v.text, 2 + end + 1, true
	}
	if isVarStart(c) {
		k := 2
		for k < len(s) && isVarChar(s[k]) {
			k++
		}
		name := s[1:k]
		v, exists := vars[name]
		if !exists || !v.ok {
			return "", k, false
		}
		return v.text, k, true
	}
	return "", 2, false
}

func isVarStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isVarChar(c byte) bool {
	return isVarStart(c) || c >= '0' && c <= '9'
}

func isValidVarName(s string) bool {
	if len(s) == 0 || !isVarStart(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isVarChar(s[i]) {
			return false
		}
	}
	return true
}

func tokenizeGHSegment(s string, base int) []ghToken {
	var out []ghToken
	for i := 0; i < len(s); {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n') {
			i++
		}
		if i >= len(s) {
			break
		}
		if s[i] == '\\' && i+1 < len(s) && s[i+1] == '\n' {
			i += 2
			continue
		}
		if strings.HasPrefix(s[i:], "<<") && !strings.HasPrefix(s[i:], "<<<") {
			if m := heredocOp.FindStringSubmatch(s[i:]); m != nil {
				i += len(m[0])
				continue
			}
		}
		if op, opLen, fdDup := matchRedirOp(s[i:]); op != "" {
			i += opLen
			if fdDup {
				continue
			}
			for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
				i++
			}
			end := scanRawWordEnd(s, i)
			out = append(out, ghToken{raw: s[i:end], start: base + i, end: base + end, redir: op})
			i = end
			continue
		}
		end := scanRawWordEnd(s, i)
		if end == i {
			i++
			continue
		}
		out = append(out, ghToken{raw: s[i:end], start: base + i, end: base + end})
		i = end
	}
	return out
}

func matchRedirOp(s string) (op string, length int, fdDup bool) {
	if strings.HasPrefix(s, "<(") || strings.HasPrefix(s, ">(") {
		return "", 0, false
	}
	for _, dup := range []string{"2>&1", "1>&2", ">&2", "<&-", ">&-", "&>-"} {
		if strings.HasPrefix(s, dup) {
			return dup, len(dup), true
		}
	}
	for _, r := range []string{hereStringOp, "1>|", "2>>", ">>", ">|", "&>", "0<", "1>", "2>", "<", ">"} {
		if strings.HasPrefix(s, r) {
			return r, len(r), false
		}
	}
	if strings.HasPrefix(s, "1") && strings.HasPrefix(s[1:], ">>") {
		return s[:3], 3, false
	}
	return "", 0, false
}

func scanRawWordEnd(s string, start int) int {
	i := start
	for i < len(s) {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' || c == ';' || c == '&' || c == '|' || c == '<' || c == '>' {
			if (c == '<' || c == '>') && i+1 < len(s) && s[i+1] == '(' {
				i = min(findClosingParen(s, i+1)+1, len(s))
				continue
			}
			break
		}
		switch {
		case c == '\\' && i+1 < len(s):
			i += 2
		case c == '$' && i+1 < len(s) && s[i+1] == '\'':
			i += 2
			for i < len(s) && s[i] != '\'' {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				i++
			}
			if i < len(s) {
				i++
			}
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return len(s)
			}
			i += j + 2
		case c == '"':
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' && i+1 < len(s) {
					i += 2
					continue
				}
				if s[i] == '$' && i+1 < len(s) && s[i+1] == '(' {
					i = min(findClosingParen(s, i+1)+1, len(s))
					continue
				}
				i++
			}
			if i < len(s) {
				i++
			}
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			i = min(findClosingParen(s, i+1)+1, len(s))
		case c == '$' && i+1 < len(s) && s[i+1] == '{':
			j := strings.IndexByte(s[i+2:], '}')
			if j < 0 {
				return len(s)
			}
			i += j + 3
		case c == '`':
			i = min(skipBacktickSpan(s, i, false)+1, len(s))
		default:
			i++
		}
	}
	return i
}

func splitGHTokens(tokens []ghToken) (words, redirs []ghToken) {
	for _, t := range tokens {
		if t.redir != "" {
			redirs = append(redirs, t)
		} else {
			words = append(words, t)
		}
	}
	return words, redirs
}

func evalPrintfArgs(args []string) (string, bool) {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", false
	}
	format, rest := args[0], args[1:]
	var b strings.Builder
	argIdx := 0
	for {
		prevIdx := argIdx
		for i := 0; i < len(format); i++ {
			if format[i] == '\\' {
				ch, n, ok := decodeSimpleEscape(format[i:])
				if !ok {
					return "", false
				}
				b.WriteByte(ch)
				i += n - 1
				continue
			}
			if format[i] != '%' {
				b.WriteByte(format[i])
				continue
			}
			if i+1 < len(format) && format[i+1] == '%' {
				b.WriteByte('%')
				i++
				continue
			}
			j := i + 1
			for j < len(format) && (format[j] == '-' || format[j] == '+' || format[j] == ' ' || format[j] == '.' || format[j] >= '0' && format[j] <= '9') {
				j++
			}
			if j >= len(format) {
				return "", false
			}
			spec := format[j]
			var val string
			if argIdx < len(rest) {
				val = rest[argIdx]
				argIdx++
			}
			switch spec {
			case 's':
				b.WriteString(val)
			case 'b':
				decoded, ok := decodeSimpleString(val)
				if !ok {
					return "", false
				}
				b.WriteString(decoded)
			case 'd', 'i', 'u':
				if val == "" {
					val = "0"
				}
				b.WriteString(val)
			default:
				return "", false
			}
			i = j
		}
		if argIdx >= len(rest) || argIdx == prevIdx {
			break
		}
	}
	return b.String(), true
}

func evalEchoArgs(args []string) (string, bool) {
	noNewline := false
	escapes := false
	for len(args) > 0 && len(args[0]) > 1 && args[0][0] == '-' {
		valid := true
		for i := 1; i < len(args[0]); i++ {
			if args[0][i] != 'n' && args[0][i] != 'e' && args[0][i] != 'E' {
				valid = false
				break
			}
		}
		if !valid {
			break
		}
		for i := 1; i < len(args[0]); i++ {
			switch args[0][i] {
			case 'n':
				noNewline = true
			case 'e':
				escapes = true
			case 'E':
				escapes = false
			}
		}
		args = args[1:]
	}
	joined := strings.Join(args, " ")
	if escapes {
		var ok bool
		joined, ok = decodeSimpleString(joined)
		if !ok {
			return "", false
		}
	}
	if !noNewline {
		joined += "\n"
	}
	return joined, true
}

func decodeSimpleString(s string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		ch, n, ok := decodeSimpleEscape(s[i:])
		if !ok {
			return "", false
		}
		b.WriteByte(ch)
		i += n - 1
	}
	return b.String(), true
}

func decodeSimpleEscape(s string) (byte, int, bool) {
	if len(s) < 2 {
		return '\\', 1, true
	}
	switch s[1] {
	case 'n':
		return '\n', 2, true
	case 't':
		return '\t', 2, true
	case 'r':
		return '\r', 2, true
	case '\\':
		return '\\', 2, true
	case '"':
		return '"', 2, true
	case '\'':
		return '\'', 2, true
	default:
		return 0, 0, false
	}
}

func maskSubstitutions(sc shellScan) shellScan {
	b := []byte(sc.masked)
	p := sc.plain
	inDouble := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		if !inDouble {
			switch {
			case c == '\\' && i+1 < len(p):
				i++
				continue
			case c == '$' && i+1 < len(p) && p[i+1] == '$':
				i++
				continue
			case c == '$' && i+1 < len(p) && p[i+1] == '\'':
				for i += 2; i < len(p) && p[i] != '\''; i++ {
					if p[i] == '\\' && i+1 < len(p) {
						i++
					}
				}
				continue
			case c == '\'':
				j := strings.IndexByte(p[i+1:], '\'')
				if j < 0 {
					i = len(p)
				} else {
					i += j + 1
				}
				continue
			case c == '"':
				inDouble = true
				continue
			case (c == '<' || c == '>') && i+1 < len(p) && p[i+1] == '(':
				end := min(findClosingParen(p, i+1), len(b)-1)
				for k := i; k <= end; k++ {
					b[k] = '_'
				}
				i = end
				continue
			}
		} else {
			if c == '\\' && i+1 < len(p) {
				i++
				continue
			}
			if c == '"' {
				inDouble = false
				continue
			}
		}
		if c == '$' && i+1 < len(p) && p[i+1] == '(' {
			end := min(findClosingParen(p, i+1), len(b)-1)
			for k := i; k <= end; k++ {
				b[k] = '_'
			}
			i = end
			continue
		}
		if c == '`' {
			end := min(skipBacktickSpan(p, i, inDouble), len(b)-1)
			for k := i; k <= end; k++ {
				b[k] = '_'
			}
			i = end
			continue
		}
	}
	return shellScan{masked: string(b), plain: sc.plain, heredocs: sc.heredocs}
}
