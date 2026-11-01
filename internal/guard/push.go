package guard

import (
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const (
	forWord       = "for"
	pushSubcmd    = "push"
	branchesFlag  = "--branches"
	gitHeadRef    = "HEAD"
	namespaceFlag = "--namespace"
	homeVar       = "HOME"
	sourceCmd     = "source"
	pushdCmd      = "pushd"
	popdCmd       = "popd"
	teeCmd        = "tee"
	chdirFlag     = "--chdir"
	groupFlag     = "--group"
	execCmd       = "exec"
	envCmdName    = "env"
	verboseFlag   = "--verbose"
)

var (
	// guardedKeyword matches commands where unbalanced quotes or unreadable
	// wrappers must fail closed.
	guardedKeyword = regexp.MustCompile(`\b(?:push|gh|--admin)\b`)
	// shortForcePush matches short option clusters on git push that include -f.
	shortForcePush = regexp.MustCompile(`^-[A-Za-z]*f[A-Za-z]*$`)
	// shortDeletePush matches short option clusters on git push that include -d.
	shortDeletePush = regexp.MustCompile(`^-[A-Za-z]*d[A-Za-z]*$`)
	// headSubst matches command substitutions that resolve the current branch name.
	headSubst = regexp.MustCompile(`\$\(\s*git\s+(?:branch\s+--show-current|rev-parse\s+--abbrev-ref\s+HEAD|symbolic-ref\s+(?:--short\s+)?(?:-q\s+)?HEAD)\s*\)|` +
		"`" + `\s*git\s+(?:branch\s+--show-current|rev-parse\s+--abbrev-ref\s+HEAD|symbolic-ref\s+(?:--short\s+)?(?:-q\s+)?HEAD)\s*` + "`")
	// compoundWords are shell reserved words and tokens that open, continue or
	// close a compound command or negation.
	compoundWords = map[string]bool{
		"if": true, "then": true, "else": true, "elif": true, "fi": true,
		forWord: true, "select": true, "while": true, "until": true, "do": true, "done": true,
		"case": true, "esac": true, "function": true, "{": true, "}": true,
		"!": true, "coproc": true, "[[": true, "((": true,
	}
	// gitPushValued are git push options that consume the next word as their argument.
	gitPushValued = map[string]bool{
		"-o": true, "--push-option": true, "--repo": true, "--receive-pack": true, "--exec": true,
	}
	// unknownMarker marks a variable expansion or command substitution whose
	// value cannot be statically known.
	unknownMarker = "\x00"
)

// segmentState is one segment of a shell command line together with the working
// directory it runs in, whether that directory is statically known for branch
// resolution, and the literal variable bindings before it.
type segmentState struct {
	seg   segment
	dir   string
	known bool
	vars  map[string]string
}

// pushRefusal refuses direct or force pushes to main/master, bare or HEAD
// pushes when the current branch is main/master or unknown after an unfollowed
// cd, gh pr merge without an explicit repository after an unfollowed cd, and
// guarded commands hidden behind unbalanced quotes or unmodeled wrappers.
func (h Hook) pushRefusal(cmd, cwd string) string {
	if hasUnbalancedQuotes(cmd) && (guardedKeyword.MatchString(cmd) || h.namesUnlock(cmd)) {
		return "Refused: the command does not parse (unbalanced quotes): quote it so the guard can read it."
	}
	return h.scanPush(cmd, cwd, false, len(cwd) == 0, 0)
}

func (h Hook) namesUnlock(cmd string) bool {
	for _, u := range h.UnlockCommands {
		if u == "" {
			continue
		}
		idx := 0
		for {
			pos := strings.Index(cmd[idx:], u)
			if pos < 0 {
				break
			}
			abs := idx + pos
			// Ignore negated glob patterns like -g '!unlock-helper' or -g '*/!unlock-helper'.
			negated := false
			for j := abs - 1; j >= 0 && strings.IndexByte(" \t\n'\"=", cmd[j]) < 0; j-- {
				if cmd[j] == '!' {
					negated = true
					break
				}
			}
			if !negated {
				return true
			}
			idx = abs + len(u)
		}
	}
	return false
}

func (h Hook) scanPush(cmd, cwd string, forceUnknown, noInitialCWD bool, depth int) string {
	if depth > 4 {
		return ""
	}
	rawSc := scanShell(cmd)
	outerPlain, inners, hadSubst := opaquePlain(rawSc.plain)
	sc := scanShell(outerPlain)
	sc.heredocs = rawSc.heredocs
	segs := sc.segments()
	vars := scanVars(sc, segs)
	hasFakeGit := h.Upstream.Git != nil
	states := walkSegments(sc, cwd, hasFakeGit)
	anyCD := hasAnyCD(rawSc, 0) || hasAnyCD(sc, 0)
	for _, in := range inners {
		if hasAnyCD(scanShell(in), 0) {
			anyCD = true
		}
	}
	lineMoved := (!isPlain(sc) || hadSubst) && anyCD
	guardNamed := guardedKeyword.MatchString(cmd) || h.namesUnlock(cmd)

	for _, in := range inners {
		if r := h.scanPush(in, cwd, lineMoved || forceUnknown, noInitialCWD, depth+1); r != "" {
			return r
		}
	}

	for i, st := range states {
		sg := st.seg
		rawWords := shellWords(sc.plain[sg.start:sg.end])
		if len(rawWords) == 0 {
			continue
		}
		if guardNamed {
			if rest, unsure := unwrapWords(rawWords); unsure != "" {
				restStr := strings.Join(rest, " ")
				isSafeXargs := unsure == xargsCmd && len(rest) > 0 &&
					!guardedKeyword.MatchString(restStr) && !h.namesUnlock(restStr) && !shells[path.Base(rest[0])]
				if !isSafeXargs {
					return "Refused: the guard cannot read `" + unsure + "` in front of this command, which names a helper, a push or a merge: run the command plainly, without the wrapper."
				}
			}
		}
		known := st.known && !lineMoved && !forceUnknown && !noInitialCWD
		hasCDOnLine := lineMoved || st.dir != cwd || !st.known || forceUnknown

		for _, exp := range expandWordsList(rawWords, vars) {
			if r := h.nestedPush(cmd, sc, segs, i, exp, st.dir, known, depth); r != "" {
				return r
			}
			c := commandAt(exp)
			if c >= len(exp) {
				continue
			}
			name := path.Base(exp[c])
			switch name {
			case gitCmd:
				if r := h.checkGitPush(exp[c:], st.dir, known, hasCDOnLine, st.vars, hasFakeGit); r != "" {
					return r
				}
			case "gh":
				if r := h.checkGHCommand(exp, c, st.dir, known); r != "" {
					return r
				}
			case executionCurl:
				if r := h.curlMerge(exp[c+1:]); r != "" {
					return r
				}
			}
		}
	}
	return ""
}

func (h Hook) nestedPush(cmd string, sc shellScan, segs []segment, segIdx int, words []string, dir string, known bool, depth int) string {
	sg := segs[segIdx]
	checkInner := func(inner string) string {
		innerSc := scanShell(inner)
		forceUnk := !known || hasAnyCD(innerSc, 0)
		return h.scanPush(inner, dir, forceUnk, false, depth+1)
	}
	if envCmd := envSplitCommand(words); envCmd != "" {
		if r := checkInner(envCmd); r != "" {
			return r
		}
	}
	if flockCmd := flockCommand(words); flockCmd != "" {
		if r := checkInner(flockCmd); r != "" {
			return r
		}
	}
	c := commandAt(words)
	if c < len(words) {
		name, args := path.Base(words[c]), words[c+1:]
		switch {
		case name == evalCmd:
			if r := checkInner(strings.Join(args, " ")); r != "" {
				return r
			}
		case name == gitCmd:
			for _, aliasCmd := range gitAliasCommands(args) {
				if r := checkInner(aliasCmd); r != "" {
					return r
				}
			}
		case interpreters[name].code != nil || len(interpreters[name].file) > 0:
			if p := readProgram(args, interpreters[name]); p.kind == programCode {
				if r := checkInner(p.arg); r != "" {
					return r
				}
			}
		}
	}
	for k, w := range words {
		name := path.Base(w)
		if w == "$SHELL" || w == "${SHELL}" {
			name = "sh"
		}
		if !shells[name] {
			continue
		}
		for _, arg := range words[k+1:] {
			if strings.HasPrefix(arg, "-") || bareRedirect.MatchString(arg) || redirection.MatchString(arg) {
				continue
			}
			if r := checkInner(arg); r != "" {
				return r
			}
		}
		first := segIdx
		for first > 0 && segs[first-1].after == "|" {
			first--
		}
		for _, hd := range sc.heredocs {
			if hd.op >= segs[first].start && hd.op < sg.end {
				if r := checkInner(cmd[hd.start:hd.end]); r != "" {
					return r
				}
			}
		}
		break
	}
	return ""
}

const unknownBranchReason = "Refused: the guard cannot tell which branch this push runs on (a cd it cannot follow): push your branch by name, `git push origin <branch>`."

func (h Hook) checkGitPush(gitWords []string, cwd string, known, hasCDOnLine bool, vars map[string]string, hasFakeGit bool) string {
	args, gitDir, dirKnown, explicitC := parseGitGlobal(gitWords, cwd, known, vars, hasFakeGit)
	if len(args) == 0 || args[0] != pushSubcmd {
		return ""
	}
	var opts, pos []string
	remote := ""
	for k := 1; k < len(args); k++ {
		a := args[k]
		if v, ok := strings.CutPrefix(a, flagRepo+"="); ok {
			remote = v
			continue
		}
		if gitPushValued[a] && k+1 < len(args) {
			if a == flagRepo {
				remote = args[k+1]
			}
			k++
			continue
		}
		if strings.HasPrefix(a, "-") {
			opts = append(opts, a)
		} else {
			pos = append(pos, a)
		}
	}
	if remote == "" && len(pos) > 0 {
		remote = pos[0]
	}
	for _, o := range opts {
		if o == "--force" || o == "--mirror" ||
			strings.HasPrefix(o, "--force-with-lease") ||
			strings.HasPrefix(o, "--force-if-includes") ||
			shortForcePush.MatchString(o) {
			return "Refused: no force-push (`" + o + "`): push a new commit, or open a new branch and PR."
		}
		if o == "--all" || o == branchesFlag {
			return "Refused: no `git push " + o + "`: it pushes `main` too; push your branch by name."
		}
	}
	var refspecs []string
	if len(pos) > 1 {
		refspecs = pos[1:]
	}
	deleting := slices.Contains(opts, "--delete") || slices.ContainsFunc(opts, shortDeletePush.MatchString)
	for _, r := range refspecs {
		if strings.HasPrefix(r, "+") {
			return "Refused: no force-push (`" + r + "`): push a new commit, or open a new branch and PR."
		}
		dst := r
		if _, rhs, ok := strings.Cut(r, ":"); ok {
			dst = rhs
		}
		needBranch := dst == gitHeadRef || dst == "@" || headSubst.MatchString(dst) ||
			(!strings.Contains(r, ":") && (r == gitHeadRef || r == "@" || headSubst.MatchString(r)))
		if needBranch {
			if !dirKnown {
				return unknownBranchReason
			}
			dst = h.currentBranch(gitDir)
			if dst == "" {
				if h.inWorkTree(gitDir) || hasCDOnLine || explicitC {
					return unknownBranchReason
				}
			}
		}
		dst = strings.TrimPrefix(dst, "refs/heads/")
		if dst == "main" || dst == "master" {
			what := "push to"
			if deleting || strings.HasPrefix(r, ":") {
				what = "delete"
			}
			return "Refused: never " + what + " `" + dst + "`: push a branch and open a pull request."
		}
	}
	if len(refspecs) == 0 && !slices.Contains(opts, "--tags") {
		if !dirKnown {
			return unknownBranchReason
		}
		branch := h.currentBranch(gitDir)
		if branch == "main" || branch == "master" {
			return "Refused: never push to `" + branch + "` (you are on it): commit on a branch and open a pull request."
		}
		if branch == "" && (h.inWorkTree(gitDir) || hasCDOnLine || explicitC) {
			return unknownBranchReason
		}
	}
	if !dirKnown && h.Upstream.on() && urlRepo(remote) == "" {
		return "Refused: the guard cannot tell which repository this push runs in (a cd it cannot follow): pass `git -C <dir> push` or push to the repository URL."
	}
	return ""
}

func (h Hook) currentBranch(dir string) string {
	if dir == "" {
		return ""
	}
	if b := h.Upstream.git(dir, "symbolic-ref", "--short", "-q", gitHeadRef); b != "" {
		return b
	}
	return h.Upstream.git(dir, "branch", "--show-current")
}

func (h Hook) inWorkTree(dir string) bool {
	if dir == "" {
		return false
	}
	return h.Upstream.git(dir, "rev-parse", "--is-inside-work-tree") == "true"
}

func parseGitGlobal(gitWords []string, cwd string, known bool, vars map[string]string, hasFakeGit bool) (args []string, gitDir string, dirKnown, explicitC bool) {
	gitDir = cwd
	dirKnown = known
	i := 1
	for i < len(gitWords) {
		a := gitWords[i]
		switch {
		case a == "--":
			return gitWords[i+1:], gitDir, dirKnown, explicitC
		case a == "-C" && i+1 < len(gitWords):
			explicitC = true
			d, ok := expandDirWord(gitWords[i+1], vars)
			if !ok {
				dirKnown = false
			} else if filepath.IsAbs(d) {
				gitDir = filepath.Clean(d)
				dirKnown = hasFakeGit || isExistingDir(gitDir)
			} else if dirKnown {
				gitDir = filepath.Join(gitDir, d)
				dirKnown = hasFakeGit || isExistingDir(gitDir)
			}
			i += 2
		case strings.HasPrefix(a, "-C") && len(a) > 2 && !strings.HasPrefix(a, "--"):
			explicitC = true
			d, ok := expandDirWord(a[2:], vars)
			if !ok {
				dirKnown = false
			} else if filepath.IsAbs(d) {
				gitDir = filepath.Clean(d)
				dirKnown = hasFakeGit || isExistingDir(gitDir)
			} else if dirKnown {
				gitDir = filepath.Join(gitDir, d)
				dirKnown = hasFakeGit || isExistingDir(gitDir)
			}
			i++
		case a == "--git-dir" || a == "--work-tree" || strings.HasPrefix(a, "--git-dir=") || strings.HasPrefix(a, "--work-tree="):
			dirKnown = false
			if !strings.Contains(a, "=") && i+1 < len(gitWords) {
				i += 2
			} else {
				i++
			}
		case (a == "-c" || a == namespaceFlag || a == "--exec-path") && i+1 < len(gitWords):
			i += 2
		case strings.HasPrefix(a, "-"):
			i++
		default:
			return gitWords[i:], gitDir, dirKnown, explicitC
		}
	}
	return nil, gitDir, dirKnown, explicitC
}

func (h Hook) checkGHCommand(words []string, cmdIdx int, cwd string, known bool) string {
	envRepo := ""
	for _, w := range words[:cmdIdx] {
		if v, ok := strings.CutPrefix(w, "GH_REPO="); ok {
			envRepo = v
		}
	}
	if envRepo == "" {
		getenv := h.Upstream.Getenv
		if getenv == nil {
			getenv = os.Getenv
		}
		envRepo = getenv("GH_REPO")
	}
	args := stripRedirects(words[cmdIdx+1:])
	commandIdx := -1
	var pos []string
	namedRepo := repoSlug(envRepo)
	for i := 0; i < len(args); i++ {
		a := args[i]
		if v, ok := repoFlag(a); ok {
			namedRepo = repoSlug(v)
			continue
		}
		if (a == "-R" || a == flagRepo) && i+1 < len(args) {
			namedRepo = repoSlug(args[i+1])
			i++
			continue
		}
		if ghValued[a] || ghAPIValued[a] {
			i++
			continue
		}
		if !strings.HasPrefix(a, "-") {
			if len(pos) == 0 {
				commandIdx = i
			}
			pos = append(pos, a)
		}
	}
	if len(pos) >= 2 && pos[0] == "alias" && (pos[1] == verbSet || pos[1] == "import") {
		return "Refused: no `gh alias`: an alias hides the command from the guard; run it in full."
	}
	if len(pos) >= 2 && ghWrites[pos[0]][pos[1]] {
		for _, a := range pos[2:] {
			if r := urlRepo(a); r != "" {
				namedRepo = r
			} else if m := issueRef.FindStringSubmatch(a); m != nil {
				namedRepo = m[1] + "/" + m[2]
			} else if pos[0] == ghRepoVerb {
				if r := repoSlug(a); r != "" {
					namedRepo = r
				}
			}
		}
		if namedRepo == "" && !known {
			if pos[0] == "pr" && pos[1] == "merge" {
				return "Refused: the guard cannot tell which repository this merge runs in (a cd it cannot follow): name the PR by its URL or with -R <owner/repo>."
			}
			if h.Upstream.on() {
				return "Refused: the guard cannot tell which repository `gh " + pos[0] + " " + pos[1] + "` runs in (a cd it cannot follow): pass -R <owner/repo> or a URL."
			}
		}
	}

	// gh accepts flags before its API subcommand. Present every downstream
	// reader with the same redirect-free, command-first argv.
	if len(pos) > 0 && pos[0] == ghAPISub && commandIdx > 0 {
		normalized := append([]string{ghAPISub}, args[:commandIdx]...)
		args = append(normalized, args[commandIdx+1:]...)
	}
	return h.ghMergeGo(args, pos, namedRepo, cwd, known)
}

// walkSegments walks the segments of sc and computes the directory each segment
// runs in and whether that directory is known with certainty for a bare git
// push or gh pr merge.
func walkSegments(sc shellScan, cwd string, hasFakeGit bool) []segmentState {
	segs := sc.segments()
	home, _ := os.UserHomeDir()
	variables := map[string]string{homeVar: home}

	plain := isPlain(sc)
	moved := !plain && hasAnyCD(sc, 0)
	if !moved {
		for _, sg := range segs {
			raw := stripRedirects(shellWords(sc.plain[sg.start:sg.end]))
			unwrapped, _ := unwrapWords(raw)
			if len(unwrapped) > 0 && (unwrapped[0] == sourceCmd || unwrapped[0] == ".") {
				moved = true
				break
			}
			for _, w := range raw {
				if strings.HasPrefix(w, "CDPATH=") || strings.HasPrefix(w, "OLDPWD=") {
					moved = true
					break
				}
			}
		}
	}

	type shFrame struct {
		here  string
		old   string
		known bool
		stack []string
	}
	frames := []shFrame{{here: cwd, known: cwd != ""}}
	lost, inList, cond, branchOff := false, false, false, false

	out := make([]segmentState, 0, len(segs))

	for _, sg := range segs {
		op := sg.before
		switch sg.before {
		case "(", "$(", "<(", "`":
			top := frames[len(frames)-1]
			frames = append(frames, shFrame{
				here:  top.here,
				old:   top.old,
				known: top.known,
				stack: slices.Clone(top.stack),
			})
		}

		words := stripRedirects(shellWords(sc.plain[sg.start:sg.end]))
		switch op {
		case "", ";", "\n", "&":
			lost = lost || cond
			inList, cond, branchOff = false, false, false
		case "||":
			if inList || cond {
				branchOff = true
			}
		}

		sh := &frames[len(frames)-1]
		segKnown := sh.known && !moved && !lost && !branchOff
		out = append(out, segmentState{
			seg:   sg,
			dir:   sh.here,
			known: segKnown,
			vars:  maps.Clone(variables),
		})

		if len(words) == 0 {
			if sg.after == ")" || sg.after == "`" {
				if len(frames) > 1 {
					frames = frames[:len(frames)-1]
				}
			}
			continue
		}

		isolated := sg.before == "|" || sg.after == "|" || sg.after == "&"
		// Bind a variable only when the segment contains assignments alone and
		// runs unconditionally; otherwise invalidate any assigned variable name.
		assignWords := words
		for len(assignWords) > 0 && (assignWords[0] == "export" || assignWords[0] == "declare" || assignWords[0] == "local" || assignWords[0] == "readonly" || assignWords[0] == "typeset") {
			assignWords = assignWords[1:]
		}
		if len(assignWords) > 0 && assignWords[0] == unsetCmd {
			for _, a := range assignWords[1:] {
				if !strings.HasPrefix(a, "-") {
					delete(variables, a)
				}
			}
		} else {
			onlyAssigns := len(assignWords) > 0
			for _, a := range assignWords {
				if !envWord.MatchString(a) {
					onlyAssigns = false
					break
				}
			}
			unconditional := (op == "" || op == ";" || op == "\n") && !cond && !lost && !branchOff && !isolated && len(frames) == 1
			for _, a := range assignWords {
				m := envWord.FindStringSubmatch(a)
				if m == nil {
					break
				}
				if onlyAssigns && unconditional {
					if val, ok := expandDirWord(m[2], variables); ok {
						variables[m[1]] = val
					} else {
						delete(variables, m[1])
					}
				} else {
					delete(variables, m[1])
				}
			}
		}
		unwrapped, _ := unwrapWords(words)
		if len(unwrapped) > 0 && (unwrapped[0] == "cd" || unwrapped[0] == pushdCmd || unwrapped[0] == popdCmd) && !isolated {
			cond = cond || op == "&&" || op == "||"
			inList = true
			branchOff = branchOff || op == "||"
			verb := unwrapped[0]
			var args []string
			afterDashDash := false
			badFlag := false
			for _, a := range unwrapped[1:] {
				if afterDashDash {
					args = append(args, a)
					continue
				}
				if a == "--" {
					afterDashDash = true
					continue
				}
				if a == "-" || !strings.HasPrefix(a, "-") || (verb == pushdCmd && (strings.HasPrefix(a, "+") || strings.HasPrefix(a, "-"))) {
					args = append(args, a)
					continue
				}
				if (verb == "cd" || verb == pushdCmd) && strings.Trim(a[1:], "LPn") == "" {
					continue
				}
				if verb == popdCmd && strings.Trim(a[1:], "n") == "" {
					continue
				}
				badFlag = true
			}
			if badFlag || (verb == "cd" && len(args) > 1) {
				sh.known = false
			} else if verb == popdCmd || (len(args) > 0 && args[0] == "-") || (verb == pushdCmd && (len(args) == 0 || strings.HasPrefix(args[0], "+") || strings.HasPrefix(args[0], "-"))) {
				var back *string
				if verb == popdCmd && len(args) == 0 {
					if len(sh.stack) > 0 {
						top := sh.stack[len(sh.stack)-1]
						sh.stack = sh.stack[:len(sh.stack)-1]
						back = &top
					} else {
						empty := ""
						back = &empty
					}
				} else if verb == "cd" && len(args) == 1 && args[0] == "-" {
					if sh.old != "" {
						b := sh.old
						back = &b
					}
				}
				if back != nil && *back != "" {
					sh.old, sh.here = sh.here, *back
				} else if back == nil {
					sh.known = false
				}
			} else {
				rawTarget := ""
				var target string
				var ok bool
				if len(args) == 0 {
					target, ok = variables[homeVar]
					ok = ok && target != ""
				} else {
					rawTarget = args[0]
					target, ok = expandDirWord(rawTarget, variables)
				}
				if !ok || target == "" || (!sh.known && !filepath.IsAbs(target)) {
					sh.known = false
				} else {
					p := target
					if !filepath.IsAbs(p) {
						p = filepath.Join(sh.here, p)
					}
					p = filepath.Clean(p)
					if strings.ContainsAny(rawTarget, "*?[") {
						matches, _ := filepath.Glob(p)
						if len(matches) == 0 {
							sh.known = false
							goto nextSeg
						}
						p = matches[0]
					}
					if !hasFakeGit {
						real, err := filepath.EvalSymlinks(p)
						if err != nil || !isExistingDir(real) {
							sh.known = false
							goto nextSeg
						}
						p = real
					}
					if verb == pushdCmd {
						sh.stack = append(sh.stack, sh.here)
					}
					sh.old, sh.here, sh.known = sh.here, p, true
				}
			}
		}
	nextSeg:
		if sg.after == ")" || sg.after == "`" {
			if len(frames) > 1 {
				frames = frames[:len(frames)-1]
			}
		}
	}
	return out
}

func isPlain(sc shellScan) bool {
	for _, sg := range sc.segments() {
		switch sg.before {
		case "(", ")", "$(", "<(", "`", "|", "&":
			return false
		}
		switch sg.after {
		case "(", ")", "$(", "<(", "`", "|", "&":
			return false
		}
		raw := sc.plain[sg.start:sg.end]
		masked := sc.masked[sg.start:sg.end]
		if strings.Contains(masked, "$(") || strings.Contains(masked, "`") || strings.Contains(raw, unknownMarker) {
			return false
		}
		words := stripRedirects(shellWords(raw))
		if len(words) == 0 {
			continue
		}
		if compoundWords[words[0]] {
			return false
		}
		for _, w := range words {
			if compoundWords[w] {
				return false
			}
		}
		if envSplitCommand(words) != "" || flockCommand(words) != "" {
			return false
		}
		unwrapped, unsure := unwrapWords(words)
		if unsure != "" {
			return false
		}
		if len(unwrapped) > 0 {
			base := path.Base(unwrapped[0])
			if base == evalCmd || shells[base] {
				return false
			}
		}
	}
	return true
}

func hasAnyCD(sc shellScan, depth int) bool {
	if depth > 4 {
		return false
	}
	for _, sg := range sc.segments() {
		words := stripRedirects(shellWords(sc.plain[sg.start:sg.end]))
		for _, w := range words {
			switch w {
			case "cd", pushdCmd, popdCmd:
				return true
			}
		}
		if envCmd := envSplitCommand(words); envCmd != "" && hasAnyCD(scanShell(envCmd), depth+1) {
			return true
		}
		if flockCmd := flockCommand(words); flockCmd != "" && hasAnyCD(scanShell(flockCmd), depth+1) {
			return true
		}
		unwrapped, _ := unwrapWords(words)
		if len(unwrapped) > 0 {
			base := path.Base(unwrapped[0])
			if base == evalCmd && hasAnyCD(scanShell(strings.Join(unwrapped[1:], " ")), depth+1) {
				return true
			}
			if shells[base] {
				for _, a := range unwrapped[1:] {
					if !strings.HasPrefix(a, "-") && hasAnyCD(scanShell(a), depth+1) {
						return true
					}
				}
			}
		}
	}
	return false
}

var dirVarRef = regexp.MustCompile(`\$\{([A-Za-z_]\w*)([^}]*)\}|\$([A-Za-z_]\w*)`)

// expandDirWord expands ~ and bound variables from vars in word. It returns
// false when word contains command/arithmetic substitutions or references a
// variable not bound in vars.
func expandDirWord(word string, vars map[string]string) (string, bool) {
	if strings.Contains(word, "$(") || strings.Contains(word, "`") || strings.Contains(word, unknownMarker) {
		return "", false
	}
	ok := true
	expanded := dirVarRef.ReplaceAllStringFunc(word, func(m string) string {
		sub := dirVarRef.FindStringSubmatch(m)
		name := sub[1]
		if name == "" {
			name = sub[3]
		}
		if sub[2] != "" {
			ok = false
			return ""
		}
		val, exists := vars[name]
		if !exists {
			ok = false
			return ""
		}
		return val
	})
	if !ok || strings.Contains(expanded, "$") {
		return "", false
	}
	if expanded == "~" || strings.HasPrefix(expanded, "~/") {
		home := vars[homeVar]
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		if home == "" {
			return "", false
		}
		expanded = home + expanded[1:]
	}
	return expanded, true
}

func isExistingDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func stripRedirects(words []string) []string {
	var out []string
	for i := 0; i < len(words); i++ {
		w := words[i]
		if bareRedirect.MatchString(w) {
			i++
			continue
		}
		if redirection.MatchString(w) {
			continue
		}
		out = append(out, w)
	}
	return out
}

func hasUnbalancedQuotes(s string) bool {
	sc := scanShell(s)
	p := sc.plain
	// frames: 't' top, 's' $( ), '"' double quotes, '`' backticks.
	stack := []byte{'t'}
	for i := 0; i < len(p); i++ {
		top := stack[len(stack)-1]
		c := p[i]
		if top == '"' {
			switch {
			case c == '\\':
				i++
				if i >= len(p) {
					return true
				}
			case c == '"':
				stack = stack[:len(stack)-1]
			case c == '$' && i+1 < len(p) && p[i+1] == '(':
				stack = append(stack, 's')
				i++
			case c == '`':
				stack = append(stack, '`')
			}
			continue
		}
		if top == '`' {
			switch {
			case c == '\\':
				i++
				if i >= len(p) {
					return true
				}
			case c == '`':
				stack = stack[:len(stack)-1]
			case c == '\'':
				j := strings.IndexByte(p[i+1:], '\'')
				if j < 0 {
					return true
				}
				i += j + 1
			case c == '"':
				stack = append(stack, '"')
			case c == '$' && i+1 < len(p) && p[i+1] == '(':
				stack = append(stack, 's')
				i++
			}
			continue
		}
		switch {
		case c == '\\':
			i++
			if i >= len(p) {
				return true
			}
		case c == '$' && i+1 < len(p) && p[i+1] == '$':
			i++
		case c == '$' && i+1 < len(p) && p[i+1] == '\'':
			i += 2
			for i < len(p) && p[i] != '\'' {
				if p[i] == '\\' {
					i++
				}
				i++
			}
			if i >= len(p) {
				return true
			}
		case c == '\'':
			j := strings.IndexByte(p[i+1:], '\'')
			if j < 0 {
				return true
			}
			i += j + 1
		case c == '"':
			stack = append(stack, '"')
		case c == '$' && i+1 < len(p) && p[i+1] == '(':
			stack = append(stack, 's')
			i++
		case c == '(' && top == 's':
			stack = append(stack, 's')
		case c == ')' && top == 's':
			stack = stack[:len(stack)-1]
		case c == '`':
			stack = append(stack, '`')
		}
	}
	return len(stack) != 1
}

// opaquePlain replaces $(...), $((...)), <(...), >(...) and `...` in s (which
// already has comments and here-document bodies blanked) with placeholders so
// that surrounding words stay intact, and returns the extracted inner command
// strings.
func opaquePlain(s string) (outer string, inners []string, hadSubst bool) {
	var b strings.Builder
	inDouble := false
	for i := 0; i < len(s); {
		c := s[i]
		if !inDouble {
			switch {
			case c == '\\' && i+1 < len(s):
				b.WriteString(s[i : i+2])
				i += 2
				continue
			case c == '$' && i+1 < len(s) && s[i+1] == '$':
				b.WriteString("$$")
				i += 2
				continue
			case c == '$' && i+1 < len(s) && s[i+1] == '\'':
				end := i + 2
				for end < len(s) && s[end] != '\'' {
					if s[end] == '\\' && end+1 < len(s) {
						end++
					}
					end++
				}
				if end < len(s) {
					end++
				}
				b.WriteString(s[i:end])
				i = end
				continue
			case c == '\'':
				j := strings.IndexByte(s[i+1:], '\'')
				if j < 0 {
					b.WriteString(s[i:])
					i = len(s)
				} else {
					b.WriteString(s[i : i+j+2])
					i += j + 2
				}
				continue
			case c == '"':
				inDouble = true
				b.WriteByte('"')
				i++
				continue
			case (c == '<' || c == '>') && i+1 < len(s) && s[i+1] == '(':
				end := findClosingParen(s, i+1)
				inners = append(inners, s[i+2:end])
				hadSubst = true
				b.WriteString(unknownMarker)
				i = min(end+1, len(s))
				continue
			case c == '=' && i+1 < len(s) && s[i+1] == '(':
				end := findClosingParen(s, i+1)
				_, subInners, _ := opaquePlain(s[i+2 : end])
				inners = append(inners, subInners...)
				hadSubst = true
				b.WriteString("=__beekeeper_array__")
				i = min(end+1, len(s))
				continue
			}
		} else {
			switch {
			case c == '\\' && i+1 < len(s):
				b.WriteString(s[i : i+2])
				i += 2
				continue
			case c == '"':
				inDouble = false
				b.WriteByte('"')
				i++
				continue
			}
		}
		if c == '$' && i+1 < len(s) && s[i+1] == '(' {
			end := findClosingParen(s, i+1)
			full := s[i:min(end+1, len(s))]
			hadSubst = true
			if i+2 < len(s) && s[i+2] == '(' && end > i+3 && s[end-1] == ')' {
				_, subInners, _ := opaquePlain(s[i+3 : end-1])
				inners = append(inners, subInners...)
				b.WriteString("0")
			} else {
				inner := s[i+2 : end]
				inners = append(inners, inner)
				if headSubst.MatchString(full) {
					b.WriteString(gitHeadRef)
				} else {
					b.WriteString(unknownMarker)
				}
			}
			i = min(end+1, len(s))
			continue
		}
		if c == '`' {
			end := i + 1
			var innerBuf strings.Builder
			for end < len(s) && s[end] != '`' {
				if s[end] == '\\' && end+1 < len(s) && (s[end+1] == '`' || s[end+1] == '\\' || s[end+1] == '$' || (inDouble && s[end+1] == '"')) {
					end++
				}
				innerBuf.WriteByte(s[end])
				end++
			}
			full := s[i:min(end+1, len(s))]
			inners = append(inners, innerBuf.String())
			hadSubst = true
			if headSubst.MatchString(full) {
				b.WriteString(gitHeadRef)
			} else {
				b.WriteString(unknownMarker)
			}
			i = min(end+1, len(s))
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String(), inners, hadSubst
}

func findClosingParen(s string, openIdx int) int {
	stack := []byte{'s'}
	for i := openIdx + 1; i < len(s); i++ {
		top := stack[len(stack)-1]
		c := s[i]
		if top == '"' {
			switch {
			case c == '\\':
				i++
			case c == '"':
				stack = stack[:len(stack)-1]
			case c == '$' && i+1 < len(s) && s[i+1] == '(':
				stack = append(stack, 's')
				i++
			case c == '`':
				stack = append(stack, '`')
			}
			continue
		}
		if top == '`' {
			switch {
			case c == '\\':
				i++
			case c == '`':
				stack = stack[:len(stack)-1]
			case c == '\'':
				j := strings.IndexByte(s[i+1:], '\'')
				if j < 0 {
					return len(s)
				}
				i += j + 1
			case c == '"':
				stack = append(stack, '"')
			case c == '$' && i+1 < len(s) && s[i+1] == '(':
				stack = append(stack, 's')
				i++
			}
			continue
		}
		switch {
		case c == '\\':
			i++
		case c == '$' && i+1 < len(s) && s[i+1] == '$':
			i++
		case c == '$' && i+1 < len(s) && s[i+1] == '\'':
			for i += 2; i < len(s) && s[i] != '\''; i++ {
				if s[i] == '\\' {
					i++
				}
			}
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return len(s)
			}
			i += j + 1
		case c == '"':
			stack = append(stack, '"')
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			stack = append(stack, 's')
			i++
		case c == '(':
			stack = append(stack, 's')
		case c == ')':
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return i
			}
		case c == '`':
			stack = append(stack, '`')
		}
	}
	return len(s)
}
