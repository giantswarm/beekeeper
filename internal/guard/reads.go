package guard

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// ReadsBudget bounds the context one repository's reads add to a call, in
// bytes.
const ReadsBudget = 10000

// readsKeep is how long a session's markers are kept after its last write.
const readsKeep = 7 * 24 * time.Hour

// The writing tools, their path keys and the instruction file Claude Code
// reads.
const (
	bashTool        = "Bash"
	editTool        = "Edit"
	writeTool       = "Write"
	notebookTool    = "NotebookEdit"
	multiEditTool   = "MultiEdit"
	readTool        = "Read"
	grepTool        = "Grep"
	globTool        = "Glob"
	commandKey      = "command"
	filePathKey     = "file_path"
	notebookPathKey = "notebook_path"
	pathKey         = "path"
	patternKey      = "pattern"
	contentKey      = "content"
	outputModeKey   = "output_mode"
	claudeMD        = "CLAUDE.md"
)

// importDepth is how deep @path imports are followed, as Claude Code does.
const importDepth = 3

var (
	// gitCommit: git commit at a command position, with the global options
	// that may stand before the subcommand.
	gitCommit = regexp.MustCompile(`(?m)` + pos + `git((?:\s+(?:-C|-c)\s+\S+|\s+--no-pager|\s+--git-dir=\S+|\s+--work-tree=\S+)*)\s+commit\b`)
	gitDashC  = regexp.MustCompile(`-C\s+(\S+)`)
	// importRe: a Claude Code @path import, never an email or a mention of a
	// team (which names no file and is dropped).
	importRe = regexp.MustCompile(`(?:^|\s)@((?:\.{1,2}/)?[\w.-]+(?:/[\w.-]+)*)`)
	// mandatoryLine: a line that makes the files it names required reading.
	mandatoryLine = regexp.MustCompile(`(?i)\b(?:must\s+(?:be\s+)?read|mandatory|required\s+reading|always\s+read|read\s+(?:\S+\s+){0,3}(?:first|before))\b`)
	mdLink        = regexp.MustCompile(`\]\(([^)\s#]+)`)
	backticked    = regexp.MustCompile("`([^`\\s]+)`")
	// secretName: file names that hold, or may hold, credentials or a
	// person's local setup; never read.
	secretName = regexp.MustCompile(`(?i)(?:^\.env|^\.netrc$|^\.npmrc$|^\.pypirc$|^\.git-credentials$|^id_(?:rsa|dsa|ecdsa|ed25519)|` +
		`\.(?:pem|key|p12|pfx|jks|keystore|kdbx|gpg|asc|age)$|\.sops\.|secret|credential|password|passwd|token|kubeconfig|` +
		`^settings\.local\.json$|\.local\.md$)`)
)

// ReadsMarker records which repositories a session was given, one empty
// file per session and repository under Dir.
type ReadsMarker struct {
	Dir string
}

// First reports whether this is the session's first call for repo, and
// records it: the marker is created exclusively, so of two concurrent calls
// only one is first. Any error reads as not first: the reads are extra.
func (m ReadsMarker) First(session, repo string) bool {
	if session == "" || strings.ContainsAny(session, `/\`) || session == "." || session == ".." {
		return false
	}
	dir := filepath.Join(m.Dir, session)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		m.prune(time.Now())
	}
	if os.MkdirAll(dir, 0o700) != nil {
		return false
	}
	sum := sha256.Sum256([]byte(repo))
	f, err := os.OpenFile(filepath.Join(dir, hex.EncodeToString(sum[:8])), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // a name beekeeper derives
	if err != nil {
		return false
	}
	_, _ = f.WriteString(repo + "\n")
	_ = f.Close()
	now := time.Now()
	_ = os.Chtimes(dir, now, now)
	return true
}

// prune removes the markers of sessions that wrote nothing for readsKeep.
func (m ReadsMarker) prune(now time.Time) {
	entries, err := os.ReadDir(m.Dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && e.IsDir() && now.Sub(info.ModTime()) > readsKeep {
			_ = os.RemoveAll(filepath.Join(m.Dir, e.Name()))
		}
	}
}

// writeTarget is the path a writing tool call changes: the file of an Edit,
// Write or NotebookEdit, the directory a Bash git commit runs in; "" for
// every other call.
func writeTarget(tool string, input map[string]any, cwd string) string {
	var p string
	switch tool {
	case editTool, writeTool, multiEditTool:
		p, _ = input[filePathKey].(string)
	case notebookTool:
		p, _ = input[notebookPathKey].(string)
	case bashTool:
		cmd, _ := input[commandKey].(string)
		m := gitCommit.FindStringSubmatchIndex(cmd)
		if m == nil {
			return ""
		}
		p = cwd
		if cds := cdArg.FindAllStringSubmatch(cmd[:m[0]], -1); len(cds) > 0 {
			p = within(p, cds[len(cds)-1][1])
		}
		for _, c := range gitDashC.FindAllStringSubmatch(cmd[m[2]:m[3]], -1) {
			p = within(p, c[1])
		}
	}
	if strings.TrimSpace(p) == "" {
		return ""
	}
	return within(cwd, p)
}

// within resolves p, quoted or starting with ~/, against dir.
func within(dir, p string) string {
	p = expandHome(strings.Trim(p, `'"`))
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(dir, p)
}

// RepoRoot is the top of the git repository or worktree holding p, "" when
// p is in none. It reads only the file system, never runs git.
func RepoRoot(p string) string {
	if p == "" {
		return ""
	}
	for d := filepath.Clean(p); ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			if r, err := filepath.EvalSymlinks(d); err == nil {
				return r
			}
			return d
		}
		if d == filepath.Dir(d) {
			return ""
		}
	}
}

// repoReads is the context for a writing call's first write in a repository
// other than the session's project, "" otherwise.
func (h Hook) repoReads(ev event) string {
	if h.Reads == nil || ev.Session == "" {
		return ""
	}
	cwd := ev.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	repo := RepoRoot(writeTarget(ev.ToolName, ev.ToolInput, cwd))
	project := h.Project
	if project == "" {
		project = cwd
	}
	if repo == "" || repo == RepoRoot(project) {
		return ""
	}
	who := ev.Session
	if ev.Agent != "" {
		who += "." + ev.Agent // a subagent's context is its own
	}
	if !h.Reads(who, repo) {
		return ""
	}
	return MandatoryReads(repo, ReadsBudget)
}

// MandatoryReads gathers repo's instructions for an agent that did not start
// in it: CLAUDE.md and AGENTS.md with their @imports, .claude/rules, the
// hooks and permissions of .claude/settings.json, and the files these name
// as mandatory reading; within budget bytes, "" when the repository has
// none. Only regular text files inside repo are read, never a secret name;
// repo is a RepoRoot, its symlinks resolved.
func MandatoryReads(repo string, budget int) string {
	r := reader{repo: repo, seen: map[string]bool{}}
	for _, name := range []string{claudeMD, "AGENTS.md", filepath.Join(".claude", claudeMD)} {
		r.add(filepath.Join(repo, name), importDepth)
	}
	rules := filepath.Join(repo, ".claude", "rules")
	_ = filepath.WalkDir(rules, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".md") {
			r.add(p, importDepth)
		}
		return nil
	})
	if s := settingsDigest(r.safe(filepath.Join(repo, ".claude", "settings.json"))); s != "" {
		r.files = append(r.files, read{rel: filepath.Join(".claude", "settings.json"), note: " (hooks and permissions; they do not run in your session)", body: s})
	}
	for _, f := range slices.Clone(r.files) {
		for _, p := range f.mandatory {
			r.add(p, 0)
		}
	}
	if len(r.files) == 0 {
		return ""
	}
	return r.render(budget)
}

type read struct {
	rel, note, body string
	mandatory       []string
}

type reader struct {
	repo  string
	seen  map[string]bool
	files []read
}

// add reads p, then the files its @imports name, depth levels deep.
func (r *reader) add(p string, depth int) {
	real := r.safe(p)
	if real == "" || r.seen[real] {
		return
	}
	r.seen[real] = true
	raw, err := os.ReadFile(real) //nolint:gosec // inside the repository, checked by safe
	if err != nil || bytes.IndexByte(raw, 0) >= 0 || !utf8.Valid(raw) {
		return
	}
	rel, _ := filepath.Rel(r.repo, real)
	f := read{rel: rel, body: string(raw)}
	var imports []string
	for line := range strings.SplitSeq(f.body, "\n") {
		for _, m := range importRe.FindAllStringSubmatch(line, -1) {
			imports = append(imports, filepath.Join(filepath.Dir(real), m[1]))
		}
		if mandatoryLine.MatchString(line) {
			f.mandatory = append(f.mandatory, r.named(filepath.Dir(real), line)...)
		}
	}
	r.files = append(r.files, f)
	if depth > 0 {
		for _, p := range imports {
			r.add(p, depth-1)
		}
	}
}

// named are the existing files a line links or quotes, relative to dir or
// to the repository's root.
func (r *reader) named(dir, line string) []string {
	var out []string
	for _, re := range []*regexp.Regexp{mdLink, backticked} {
		for _, m := range re.FindAllStringSubmatch(line, -1) {
			ref := strings.TrimPrefix(m[1], "/")
			if strings.Contains(ref, "://") || !strings.ContainsAny(ref, "./") {
				continue
			}
			for _, base := range []string{dir, r.repo} {
				if p := filepath.Join(base, ref); r.safe(p) != "" {
					out = append(out, p)
					break
				}
			}
		}
	}
	return out
}

// safe is p with its symlinks resolved when it is a regular file inside the
// repository whose name is no secret's, "" otherwise.
func (r *reader) safe(p string) string {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(r.repo, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if secretName.MatchString(part) || part == ".git" {
			return ""
		}
	}
	if info, err := os.Stat(real); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return real
}

// settingsDigest is the hooks and permissions of a settings file, "" when it
// has neither or cannot be read. The rest (env, credentials helpers) stays
// out.
func settingsDigest(p string) string {
	if p == "" {
		return ""
	}
	raw, err := os.ReadFile(p) //nolint:gosec // inside the repository, checked by safe
	if err != nil {
		return ""
	}
	var s map[string]json.RawMessage
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	keep := map[string]json.RawMessage{}
	for _, k := range []string{"hooks", "permissions"} {
		if v, ok := s[k]; ok {
			keep[k] = v
		}
	}
	if len(keep) == 0 {
		return ""
	}
	out, err := json.MarshalIndent(keep, "", "  ")
	if err != nil {
		return ""
	}
	return string(out)
}

// leftRoom is what render keeps free for naming the files left out.
const leftRoom = 500

// render lays the files out within budget bytes: a file that does not fit is
// cut with a note, and the ones after it are named for the agent to read.
func (r *reader) render(budget int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "beekeeper: your first change in the repository %s this session. Its instructions follow; they apply to "+
		"your work in it, beside your own project's. Read the files they name before you go on.\n", r.repo)
	var left []string
	for _, f := range r.files {
		head := "\n=== " + f.rel + f.note + " ===\n"
		room := budget - leftRoom - b.Len() - len(head)
		switch {
		case len(left) > 0 || room < 200:
			left = append(left, f.rel)
		case len(f.body) <= room:
			b.WriteString(head + strings.TrimRight(f.body, "\n") + "\n")
		default:
			note := fmt.Sprintf("\n[truncated at %%d of %d bytes: read %s for the rest]\n", len(f.body), filepath.Join(r.repo, f.rel))
			cut := cutAt(f.body, room-len(note)-8)
			b.WriteString(head + cut + fmt.Sprintf(note, len(cut)))
		}
	}
	if len(left) > 0 {
		b.WriteString(cutAt("\nLeft out for size, read them yourself: "+strings.Join(left, ", "), budget-b.Len()-1) + "\n")
	}
	return b.String()
}

// cutAt is s cut to at most n bytes at a rune boundary, at the last line end
// when there is one.
func cutAt(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	s = s[:n]
	if i := strings.LastIndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}
