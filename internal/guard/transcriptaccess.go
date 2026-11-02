package guard

import (
	"os"
	"path/filepath"
	"regexp"
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

// A component alone may be ordinary replacement text in a repository edit.
// Operators and join calls, however, construct a path whose base is unknown.
var transcriptPathConstruction = regexp.MustCompile(`(?:[/+=]\s*['"]\.(?:codex|claude|gemini|omp|agy)['"]|['"]\.(?:codex|claude|gemini|omp|agy)['"]\s*[/+]|(?:join|Join|joinpath)\s*\([^)]*['"]\.(?:codex|claude|gemini|omp|agy)['"])`)

const transcriptUnresolved = "Refused: the guard cannot resolve a constructed harness path in this script. Use explicit file paths so transcript access can be checked, or ask the supervisor for an explicit fixture grant."

var transcriptLiteralPath = regexp.MustCompile(`(?:~|\$\{?[A-Z_]+\}?|/)[^\s'"` + "`" + `(){};,<>|]+`)

func (a transcriptAccess) fileLeak(words, dirs []string, helpers map[string]string) *leak {
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
		// A program that constructs a harness home from a quoted component
		// has no statically provable transcript target: fail closed.
		if ip, ok := interpreters[name]; ok && ip.code != nil && transcriptHomeComponent.MatchString(word) && transcriptPathConstruction.MatchString(word) {
			return &leak{never: true, transcript: true, unresolvedTranscript: true}
		}
		word = strings.TrimPrefix(strings.TrimPrefix(word, "0<"), "<>")
		if _, value, ok := strings.Cut(word, "="); ok {
			word = value
		}
		candidates := []string{word}
		for _, component := range transcriptHomeComponent.FindAllString(word, -1) {
			candidates = append(candidates, strings.Trim(component, `"'`))
		}
		for _, literal := range transcriptLiteralPath.FindAllString(word, -1) {
			// A slash in a jq operator or a shell parameter is not an absolute path.
			if strings.Trim(literal, "/") != "" && !strings.Contains(word, "${") {
				candidates = append(candidates, literal)
			}
		}
		for _, candidate := range candidates {
			for _, env := range []string{"CODEX_HOME", transcriptClaudeConfig, transcriptGeminiHome, "PI_CODING_AGENT_DIR", "OMP_CONFIG_DIR"} {
				if home := os.Getenv(env); home != "" {
					candidate = strings.ReplaceAll(candidate, "${"+env+"}", home)
					candidate = strings.ReplaceAll(candidate, "$"+env, home)
				}
			}
			for _, dir := range dirs {
				p := wordPath(candidate, dir)
				explicitHome := strings.Contains(candidate, string(filepath.Separator)+".") || strings.HasPrefix(candidate, ".codex") || strings.HasPrefix(candidate, ".claude") || strings.HasPrefix(candidate, ".gemini") || strings.HasPrefix(candidate, ".omp")
				recurse := recursive || explicitHome && !strings.ContainsAny(candidate, "*?[")
				if a.denied(p, recurse) {
					return &leak{never: true, transcript: true}
				}
			}
		}
	}
	return nil
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
	if h.transcriptAccess(ev.Session).denied(wordPath(p, cwd), ev.ToolName != readTool) {
		return transcriptRefusal
	}
	return ""
}
