package guard

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// TranscriptAccess protects conversation stores separately from credentials.
// Grants are exact files or directories, keyed by the receiving session ID.
// A hook event's transcript_path never authorizes a read.
type TranscriptAccess struct {
	Roots []string
	// ClaudeProjects and AgyBrains also describe layouts whose per-session
	// artifact subtrees belong to the receiving session without a grant.
	ClaudeProjects, AgyBrains []string
	Grants                    map[string][]string
	// Managed distinguishes recorded starts/roles from the person's own sessions.
	// A lookup error fails closed; nil leaves this policy off.
	Managed func(session string) (bool, error)
}

type transcriptAccess struct {
	roots, grants []string
	failed        bool
	session       string
	own           []transcriptOwnRoot
}

type transcriptOwnRoot struct {
	path  string
	depth int
}

func (h Hook) transcriptAccess(session string) transcriptAccess {
	managed := false
	if h.Transcripts.Managed != nil {
		var err error
		managed, err = h.Transcripts.Managed(session)
		if err != nil {
			return transcriptAccess{failed: true}
		}
	}
	if !managed {
		return transcriptAccess{}
	}
	claudeRoots := append([]string{"~/.claude/projects"}, h.Transcripts.ClaudeProjects...)
	if home := os.Getenv(transcriptClaudeConfig); home != "" {
		claudeRoots = append(claudeRoots, filepath.Join(home, "projects"))
	}
	agyRoots := append([]string{"~/.gemini/antigravity-cli/brain"}, h.Transcripts.AgyBrains...)
	if home := os.Getenv(transcriptGeminiHome); home != "" {
		agyRoots = append(agyRoots, filepath.Join(home, ".gemini", "antigravity-cli", "brain"))
	}
	roots := []string{
		"~/.claude/projects", "~/.claude/history.jsonl",
		"~/.codex/sessions", "~/.codex/archived_sessions", "~/.codex/history.jsonl",
		"~/.gemini/antigravity-cli/brain", "~/.gemini/antigravity-cli/conversations",
		"~/.gemini/tmp", "~/.agy", "~/.omp/agent/sessions",
	}
	for _, env := range []struct {
		name   string
		stores []string
	}{
		{"CODEX_HOME", []string{transcriptSessions, "archived_sessions", "history.jsonl"}},
		{transcriptClaudeConfig, []string{"projects", "history.jsonl"}},
		{transcriptGeminiHome, []string{".gemini"}},
		{"PI_CODING_AGENT_DIR", []string{transcriptSessions, "agent.db"}},
		{"OMP_CONFIG_DIR", []string{transcriptSessions, "agent.db"}},
	} {
		if home := os.Getenv(env.name); home != "" {
			for _, store := range env.stores {
				roots = append(roots, filepath.Join(home, store))
			}
		}
	}
	roots = append(roots, h.Transcripts.Roots...)
	roots = append(roots, claudeRoots...)
	roots = append(roots, agyRoots...)
	a := transcriptAccess{}
	if home, err := os.UserHomeDir(); err != nil || !filepath.IsAbs(home) {
		a.failed = true
	}
	for _, root := range roots {
		if p := wordPath(root, ""); filepath.IsAbs(p) {
			a.roots = append(a.roots, p)
		}
	}
	if session != "" && session != "." && session != ".." && !strings.ContainsAny(session, "/\\*?[") {
		a.session = session
		for _, layout := range []struct {
			roots []string
			depth int
		}{{claudeRoots, 2}, {agyRoots, 1}} {
			for _, root := range layout.roots {
				if p := wordPath(root, ""); filepath.IsAbs(p) {
					a.own = append(a.own, transcriptOwnRoot{p, layout.depth})
				}
			}
		}
	}
	if session != "" {
		for _, grant := range h.Transcripts.Grants[session] {
			if p := wordPath(grant, ""); filepath.IsAbs(p) && !strings.ContainsAny(p, "*?[") {
				a.grants = append(a.grants, p)
			}
		}
	}
	return a
}

// Resolve existing ancestors too: a missing file under a symlinked store
// remains protected. No transcript content is opened for this decision.
func transcriptReal(p string) string {
	cur := p
	var tail []string
	for steps := 0; steps < 64; steps++ {
		if target, err := os.Readlink(cur); err == nil {
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(cur), target)
			}
			cur = target
			continue
		}
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				real = filepath.Join(real, tail[i])
			}
			return real
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
	return p
}

func (a transcriptAccess) denied(p string, recursive bool) bool {
	if a.failed {
		return true
	}
	if p == "" || len(a.roots) == 0 {
		return false
	}
	// Never enumerate private files to expand a wildcard or treat a glob as
	// an exact-file grant. Its fixed directory prefix bounds what it can read.
	glob := strings.IndexAny(p, "*?[")
	if glob >= 0 {
		recursive = recursive || strings.Contains(p[glob:], string(filepath.Separator))
		p = filepath.Dir(p[:glob] + "x")
	}
	real := transcriptReal(p)
	guarded := false
	for _, root := range a.roots {
		rr := transcriptReal(root)
		if hasDirPrefix(p, root) || hasDirPrefix(real, rr) || recursive && (hasDirPrefix(root, p) || hasDirPrefix(rr, real)) {
			guarded = true
			break
		}
	}
	if !guarded {
		return false
	}
	if glob < 0 {
		for _, own := range a.own {
			if a.owns(own.path, p, own.depth) && a.owns(transcriptReal(own.path), real, own.depth) {
				return false
			}
		}
		for _, grant := range a.grants {
			// A link in a fixture directory must not grant another conversation.
			if hasDirPrefix(p, grant) && hasDirPrefix(real, transcriptReal(grant)) {
				return false
			}
		}
	}
	return true
}

// The session component must be a complete directory name in the known
// layout. Neither a filename substring nor transcript_path confers ownership.
func (a transcriptAccess) owns(root, p string, depth int) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	return len(parts) >= depth && parts[depth-1] == a.session
}

const (
	transcriptClaudeConfig = "CLAUDE_CONFIG_DIR"
	transcriptGeminiHome   = "GEMINI_CLI_HOME"
	transcriptTouch        = "touch"
	transcriptChmod        = "chmod"
	transcriptReadlink     = "readlink"
	transcriptSessions     = "sessions"
	transcriptBasename     = "basename"
	transcriptRealpath     = "realpath"
	transcriptRmdir        = "rmdir"
)

const transcriptRefusal = "Refused: this call may read another session's private harness transcripts. Use synthetic fixtures for event shapes, or ask the supervisor to grant this session its own transcript or an explicit fixture directory through hooks.transcriptGrants. Narrow broad searches to the task directory. Counts, key filters, copies and redirects do not authorize a read."

// Include literal paths embedded in interpreter code, not just shell words.
var transcriptHomeComponent = regexp.MustCompile(`['"]\.(?:codex|claude|gemini|omp|agy)['"]`)

// Inspect operations outside string data. A harness component may be stored in
// a replacement variable, but grouping or a variable must not hide a path
// operation. We cannot prove the base of such constructions, so fail closed.
var transcriptCodeString = regexp.MustCompile(`[fF]?(?:"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*')`)
var transcriptPathOperation = regexp.MustCompile(`[/+]|\b(?:join|Join|joinpath|home|expanduser|homedir|environ|getenv)\b`)

// Runtime home/environment lookups can reach any harness store without naming
// a harness component at all (for example by globbing, splitting a name, or
// selecting a directory from an environment variable). The command-text guard
// cannot establish the resulting target or apply a path grant to it.
var transcriptRuntimeRoot = regexp.MustCompile(`\b(?:home|homedir|expanduser|getenv|Getenv|UserHomeDir)\s*\(|\b(?:environ|env)\s*[\[.]|\bENV\s*[\[{.]`)

func unresolvedTranscriptPath(code string) bool {
	operations := transcriptCodeString.ReplaceAllStringFunc(code, func(literal string) string {
		// Formatted strings contain executable expressions, not just data.
		if literal[0] == 'f' || literal[0] == 'F' {
			return literal
		}
		return ""
	})
	return transcriptRuntimeRoot.MatchString(operations) ||
		transcriptHomeComponent.MatchString(code) && transcriptPathOperation.MatchString(operations)
}

const transcriptUnresolved = "Refused: the guard cannot resolve a constructed harness path in this script. Use explicit file paths so transcript access can be checked, or ask the supervisor for an explicit fixture grant."

// transcriptTextTools take a regex or script, which reads like a path, among
// their arguments; grepCmds are the others.
var transcriptTextTools = map[string]bool{"sed": true, gsedCmd: true, "awk": true, "gawk": true, "mawk": true}

var transcriptLiteralPath = regexp.MustCompile(`(?:~|\$\{?[A-Z_]+\}?|/)[^\s'"` + "`" + `(){};,<>|]+`)

func (a transcriptAccess) fileLeak(words, dirs []string, helpers map[string]string, inTest bool) *leak {
	if a.failed {
		return &leak{never: true, transcript: true}
	}
	if len(a.roots) == 0 {
		return nil
	}
	k := commandAt(words)
	name := ""
	if k < len(words) {
		name = filepath.Base(words[k])
	}
	text := k < len(words) && isBeekeeperTextSubcommand(name, words[k+1:])
	// Exempt only content-free utilities whose names are not shadowed.
	metadata := name == "ls" || name == statCommand || name == transcriptReadlink || name == transcriptRealpath || name == "dirname" || name == transcriptBasename || name == "rm" || name == transcriptRmdir || name == transcriptChmod || name == transcriptTouch || name == shredCmd
	// A test compares its operands or stats them; a pattern in [[ ]] is not
	// even expanded. Substitutions inside it have their own segments.
	metadata = metadata || inTest || name == "[[" || name == "[" || name == testCommand
	if name == findCmd {
		metadata = true
		for _, arg := range words {
			if strings.HasPrefix(arg, "-exec") || strings.HasPrefix(arg, "-ok") {
				metadata = false
			}
		}
	}
	_, shadow := helpers[name]
	if shadow {
		metadata = false
	}
	// wc, file, grep -c and beekeeper can read contents.
	recursive := name == findCmd || name == "cp" || name == "tar" || name == "rsync"
	for _, arg := range words {
		recursive = recursive || (name == "rg" || name == "ag") && (arg == "--hidden" || arg == "-uu" || arg == "-uuu")
	}
	for _, arg := range words {
		recursive = recursive || grepCmds[name] && grepRecursive.MatchString(arg)
	}
	for i, word := range words {
		input := strings.HasPrefix(word, "<") || i > 0 && (words[i-1] == "<" || words[i-1] == "0<" || words[i-1] == "<>")
		configArg := name == selfCmd && (strings.HasPrefix(word, "--config=") || i > 0 && words[i-1] == flagConfig)
		if (metadata || text) && !input && !configArg {
			continue
		}
		// Substitutions have their own segments in the shell scanner.
		if ((name == echoCommand || name == printfCommand || name == "cd") && !shadow || k == len(words)) && !input {
			continue
		}
		word = strings.TrimPrefix(strings.TrimPrefix(word, "0<"), "<>")
		if _, value, ok := strings.Cut(word, "="); ok {
			word = value
		}
		var candidates []string
		for _, component := range transcriptHomeComponent.FindAllString(word, -1) {
			candidates = append(candidates, strings.Trim(component, `"'`))
		}
		// The shell does not expand a word it has quoted, and whitespace shows
		// this one was; a text tool's pattern or script is quoted as a rule.
		quoted := grepCmds[name] || transcriptTextTools[name] || strings.ContainsAny(word, " \t\n")
		if a.candidateDenied(word, dirs, recursive, quoted) {
			return &leak{never: true, transcript: true}
		}
		for _, candidate := range candidates {
			if a.candidateDenied(candidate, dirs, recursive, false) {
				return &leak{never: true, transcript: true}
			}
		}
		for _, literal := range transcriptLiteralPath.FindAllString(word, -1) {
			// A slash in a jq operator or a shell parameter is not an absolute path.
			if strings.Trim(literal, "/") != "" && !strings.Contains(word, "${") && a.candidateDenied(literal, dirs, recursive, true) {
				return &leak{never: true, transcript: true}
			}
		}
	}
	return nil
}

// candidateDenied checks one path a word may name, from every directory the
// command may run in. embedded marks text the shell does not expand itself.
func (a transcriptAccess) candidateDenied(candidate string, dirs []string, recursive, embedded bool) bool {
	for _, env := range []string{"CODEX_HOME", transcriptClaudeConfig, transcriptGeminiHome, "PI_CODING_AGENT_DIR", "OMP_CONFIG_DIR"} {
		if home := os.Getenv(env); home != "" {
			candidate = strings.ReplaceAll(candidate, "${"+env+"}", home)
			candidate = strings.ReplaceAll(candidate, "$"+env, home)
		}
	}
	explicitHome := strings.Contains(candidate, string(filepath.Separator)+".") || strings.HasPrefix(candidate, ".codex") || strings.HasPrefix(candidate, ".claude") || strings.HasPrefix(candidate, ".gemini") || strings.HasPrefix(candidate, ".omp")
	recurse := recursive || explicitHome && !strings.ContainsAny(candidate, "*?[")
	for _, dir := range dirs {
		p := wordPath(candidate, dir)
		if embedded && strings.ContainsAny(p, "*?[") {
			if a.embeddedGlobDenied(p) {
				return true
			}
		} else if a.denied(p, recurse) {
			return true
		}
	}
	return false
}

// embeddedGlobDenied decides a glob-shaped path the shell does not expand. It
// is mostly a regex (s/.*foo/bar/), whose fixed prefix "/" would bound it to
// the whole filesystem, so it is refused only when its components can reach a
// store. A word the shell may expand keeps the bound of denied.
func (a transcriptAccess) embeddedGlobDenied(p string) bool {
	if a.failed {
		return true
	}
	// A link in the fixed prefix leads the pattern where it really points.
	prefix := filepath.Dir(p[:strings.IndexAny(p, "*?[")] + "x")
	patterns := []string{p, filepath.Join(transcriptReal(prefix), strings.TrimPrefix(p, prefix))}
	for _, root := range a.roots {
		for _, store := range []string{root, transcriptReal(root)} {
			for _, pattern := range patterns {
				if transcriptGlobReaches(pattern, store) {
					return true
				}
			}
		}
	}
	return false
}

// transcriptVariable is a parameter in a pattern, unlike a regex's end anchor.
var transcriptVariable = regexp.MustCompile(`\$[A-Za-z_{(]`)

// transcriptGlobReaches reports whether pattern can name store, a path under
// it or an ancestor of it. Code may build the head of a path at run time, so
// two or more components matching inside store count as well; a single one
// does not, or the common regex .* would match every dot directory.
func transcriptGlobReaches(pattern, store string) bool {
	split := func(p string) []string {
		return strings.FieldsFunc(p, func(r rune) bool { return r == filepath.Separator })
	}
	components, stored := split(pattern), split(store)
	if slices.Contains(components, "**") {
		return true
	}
	for offset := range stored {
		n := min(len(components), len(stored)-offset)
		if offset > 0 && n < 2 {
			continue
		}
		matched := true
		for i := 0; i < n && matched; i++ {
			matched = transcriptComponentMatches(components[i], stored[offset+i])
		}
		if matched {
			return true
		}
	}
	return false
}

// A component the guard cannot evaluate could match anything.
func transcriptComponentMatches(component, name string) bool {
	if transcriptVariable.MatchString(component) {
		return true
	}
	matched, err := filepath.Match(strings.TrimRight(component, `\`), name)
	return matched || err != nil
}

func (h Hook) transcriptFileRefusal(ev event) string {
	if ev.ToolName != readTool && ev.ToolName != grepTool && ev.ToolName != globTool && ev.ToolName != editTool && ev.ToolName != multiEditTool && ev.ToolName != notebookTool && ev.ToolName != writeTool && ev.ToolName != bashTool {
		return ""
	}
	if h.ConfigErr != nil && ev.ToolName != bashTool {
		return "Refused: transcript access policy could not load; failing closed."
	}
	if ev.ToolName == bashTool {
		return ""
	} // checked by the shell scanner
	cwd := ev.CWD
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return transcriptRefusal
		}
	}
	p, _ := ev.ToolInput[filePathKey].(string)
	if p == "" {
		p, _ = ev.ToolInput[pathKey].(string)
	}
	if p == "" {
		p, _ = ev.ToolInput[notebookPathKey].(string)
	}
	if p == "" {
		p = cwd
	}
	access := h.transcriptAccess(ev.Session)
	if ev.ToolName == globTool {
		p = globSearchPath(ev.ToolInput, cwd)
		// Brace alternatives and escapes are glob syntax too. Bound their
		// search by the literal directory prefix, while retaining the full
		// pattern below so wildcards cannot acquire an exact-file grant.
		if access.denied(wordPath(globRoot(p), cwd), true) {
			return transcriptRefusal
		}
	}
	if access.denied(wordPath(p, cwd), ev.ToolName != readTool) {
		return transcriptRefusal
	}
	return ""
}
