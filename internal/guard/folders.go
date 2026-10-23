package guard

import (
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The folders guard keeps sessions' files out of the person's folders
// (Hook.PersonFolders): a Write, Edit, MultiEdit or NotebookEdit, or a Bash
// command's output redirection or tee, is refused when it would create a
// file directly in one of them (or write one there that is no dotfile), or
// write a file in the main working tree of a git checkout that sits directly
// in one of them which git neither tracks nor ignores: a draft there
// overwrites the person's own untracked file of the same name. A linked
// worktree (its .git a file) is a session's own and passes.

// gitDir is a checkout's git directory, or in a linked worktree the file
// naming it.
const gitDir = ".git"

// redirEnd are the bytes that end a redirection's target word in the
// masked command line.
const redirEnd = " \t\n;&|()<>"

// projectChars are the characters Claude Code keeps of a project's path in
// its directory names; every other one becomes a dash.
var projectChars = regexp.MustCompile(`[^a-zA-Z0-9]`)

// Scratchpad is the scratchpad Claude Code gives the session id started in
// the project directory: <tmp>/claude-<uid>/<project>/<session>/scratchpad.
func Scratchpad(project, session string) string {
	return filepath.Join(os.TempDir(), "claude-"+strconv.Itoa(os.Getuid()),
		projectChars.ReplaceAllString(project, "-"), session, "scratchpad")
}

// SessionDirs are the directories Claude Code keeps for the session id
// under <tmp>/claude-<uid>/<project>/, whichever project it runs in.
func SessionDirs(session string) []string {
	if session == "" || strings.ContainsAny(session, `/\*?[`) {
		return nil
	}
	dirs, _ := filepath.Glob(filepath.Join(os.TempDir(), "claude-"+strconv.Itoa(os.Getuid()), "*", session))
	return dirs
}

// scratchpad is the session's scratchpad: the one Claude Code made, else
// the one it makes for the project.
func (h Hook) scratchpad(ev event) string {
	for _, d := range SessionDirs(ev.Session) {
		if pad := filepath.Join(d, "scratchpad"); isDir(pad) {
			return pad
		}
	}
	project := h.Project
	if project == "" {
		project = ev.CWD
	}
	return Scratchpad(project, ev.Session)
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// folderRefusal returns why the call writes into the person's folders, ""
// when it does not.
func (h Hook) folderRefusal(ev event) string {
	if len(h.PersonFolders) == 0 {
		return ""
	}
	cwd := ev.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	var targets []string
	switch ev.ToolName {
	case writeTool, editTool, multiEditTool, notebookTool:
		file, _ := ev.ToolInput[filePathKey].(string)
		if file == "" {
			file, _ = ev.ToolInput[notebookPathKey].(string)
		}
		if file != "" {
			targets = []string{absIn(cwd, file)}
		}
	case bashTool:
		cmd, _ := ev.ToolInput[commandKey].(string)
		targets = writeTargets(cmd, cwd, 0)
	}
	for _, t := range targets {
		if what := h.personFile(t); what != "" {
			return "Refused: `" + t + "` is " + what + ", where no session creates or overwrites a file unasked " +
				"(the person's folders, hooks.personFolders). Scratch files go in this session's scratchpad, " +
				h.scratchpad(ev) + ", or in ~/.local/state/<program>/; a change to a checkout is made in a worktree of its own. " +
				"A path built from a variable stops on an empty one: `cd \"${dir:?}\"`, `\"${dir:?}\"/file`."
		}
	}
	return ""
}

func absIn(dir, p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		p = expandHome(p)
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return filepath.Clean(p)
}

// personFile names what the file p is when a write to it lands in the
// person's folders, "" when it does not. The path is judged as written: a
// configuration file reached through a link into a checkout is the
// configuration's.
func (h Hook) personFile(p string) string {
	dir := filepath.Dir(p)
	for d := dir; ; d = filepath.Dir(d) {
		fi, err := os.Lstat(filepath.Join(d, gitDir))
		if err == nil {
			if !fi.IsDir() || !h.personFolder(filepath.Dir(d)) {
				return "" // a linked worktree, or a checkout of nobody's folder
			}
			rel, _ := filepath.Rel(d, p)
			if hidden(rel) || gitSays(d, "ls-files", "--error-unmatch", "--", rel) || gitSays(d, "check-ignore", "-q", "--", rel) {
				return "" // configuration, git's own files, a tracked or ignored file
			}
			return "a file in the main working tree of the checkout " + d + " that git neither tracks nor ignores"
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	if !h.personFolder(dir) {
		return ""
	}
	if _, err := os.Lstat(p); err == nil && strings.HasPrefix(filepath.Base(p), ".") {
		return "" // the person's dotfile, written in place
	}
	return "a file directly in " + dir
}

// hidden reports whether a relative path has a part that starts with a dot:
// a dotfile or a file in a dot-directory (.git, .config, .claude).
func hidden(rel string) bool {
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if strings.HasPrefix(part, ".") && part != "." && part != ".." {
			return true
		}
	}
	return false
}

// personFolder reports whether dir is one of the person's folders.
func (h Hook) personFolder(dir string) bool {
	for _, f := range h.PersonFolders {
		if ok, _ := filepath.Match(f, dir); ok {
			return true
		}
	}
	return false
}

// gitSafe are the options that keep a checkout's own configuration from
// running a program in the hook, which runs outside the agent sandbox: no
// file system monitor, no hooks, no external diff or filters.
var gitSafe = []string{"-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "-c", "diff.external=", "-c", "core.untrackedCache=false"}

// gitSays reports whether the git command in dir exits 0; one that fails to
// run says no. It writes no lock or index.
func gitSays(dir string, args ...string) bool {
	c := exec.Command("git", append(append(slices.Clone(gitSafe), "-C", dir), args...)...) //nolint:gosec // fixed git subcommands
	c.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_CONFIG_NOSYSTEM=1")
	return c.Run() == nil
}

// writeTargets are the files a Bash command line writes through an output
// redirection or tee, resolved against its cd's, its assignments and the
// environment (st.word); a target that stays unknown is left out. A shell's
// -c string and a here-document a shell reads are command lines of their
// own, where a variable that is neither assigned there nor in the
// environment is unknown: the outer shell may have set it.
func writeTargets(cmd, cwd string, depth int) []string {
	if depth > 4 {
		return nil
	}
	sc := scanShell(cmd)
	segs := sc.segments()
	w := targetWalk{cmd: cmd, sc: sc, segs: segs, nested: depth > 0, branches: 1}
	st := deleteState{assigned: map[string]string{}, env: os.Getenv, nounset: depth > 0}
	w.walk(0, st, cwd)
	out := w.out
	for _, cs := range shellStrings(cmd) {
		if sc.masked[cs.at] == cmd[cs.at] {
			out = append(out, writeTargets(cs.text, cwd, depth+1)...)
		}
	}
	for _, hd := range sc.heredocs {
		if shellReads(sc, segs, hd.op) {
			out = append(out, writeTargets(cmd[hd.start:hd.end], cwd, depth+1)...)
		}
	}
	return out
}

// pwdVar is the shell's working directory; writeTargets assigns it while
// the directory is known.
const pwdVar = "PWD"

// plainVar: a parameter expansion without an operator, $NAME or ${NAME}.
var plainVar = regexp.MustCompile(`^\$(?:\w+|\{\w+\})$`)

// word resolves a word as the shell would: its literal text, ~ as the home
// placeholder, a variable by its assignment or the environment, an unset
// one ($NAME, ${NAME}) as empty; false when it stays unknown (a command
// substitution, an expansion with an operator, a value assigned from one).
func (st *deleteState) word(raw string) (string, bool) {
	lit, exps := st.expansions(raw)
	for i, e := range exps {
		if pwd, ok := st.assigned[pwdVar]; ok && (e.token == "$"+pwdVar || e.token == "${"+pwdVar+"}") {
			exps[i].value, e.value = pwd, pwd
		}
		if strings.Contains(e.value, opaquePart) || e.maybeEmpty && e.value == "" && !plainVar.MatchString(e.token) {
			return "", false
		}
	}
	return st.resolve(lit, exps), true
}

// setVars records the assignments among the words as written (NAME=value),
// each value resolved (st.word), or unknown; the last one is unknown when a
// command substitution follows (after, the segment's operator: x=$(…)).
func (st *deleteState) setVars(raw []string, after string) {
	for i, r := range raw {
		name, val, ok := strings.Cut(r, "=")
		if !ok || !varName.MatchString(name) {
			continue
		}
		v, known := st.word(val)
		if !known || i == len(raw)-1 && (after == "$(" || after == "`") {
			v = opaquePart
		}
		st.assigned[name] = v
	}
}

// varName is a shell variable's name.
var varName = regexp.MustCompile(`^[A-Za-z_]\w*$`)

// maxBranches bounds the branches a command line's `|| cd` fallbacks open.
const maxBranches = 8

// targetWalk walks a command line's simple commands in order, following
// where the shell stands and what its variables hold, and collects the
// files they write.
type targetWalk struct {
	cmd  string
	sc   shellScan
	segs []segment
	// nested: the command line is a shell's -c string or here-document.
	nested   bool
	branches int
	out      []string
}

// walk follows the segments from i on with the variables st and the
// directory dir ("" unknown). A cd after || may not run: the walk goes on
// both ways.
func (w *targetWalk) walk(i int, st deleteState, dir string) {
	st.chdir(dir)
	for ; i < len(w.segs); i++ {
		sg := w.segs[i]
		w.redirections(sg, &st, dir)
		cmds := rawCommands(w.sc.plain[sg.start:sg.end])
		if len(cmds) == 0 {
			continue
		}
		raw := cmds[0]
		for len(raw) > 0 && keywords[raw[0]] {
			raw = raw[1:]
		}
		words := make([]string, len(raw))
		for j, r := range raw {
			words[j] = strings.Join(shellWords(r), "")
		}
		k := commandAt(words)
		if k == len(words) {
			st.setVars(raw, sg.after)
			continue
		}
		switch name := path.Base(words[k]); {
		case declarations[name]:
			st.setVars(raw[k+1:], sg.after)
		case name == "cd" || name == "pushd":
			if sg.before == "||" && w.branches < maxBranches {
				w.branches++
				w.walk(i+1, st.clone(), dir)
			}
			if sg.after == "$(" || sg.after == "`" {
				dir = "" // cd into a command substitution's output
			} else {
				dir = st.cd(dir, raw[k+1:], words[k+1:])
			}
			st.chdir(dir)
		case name == "tee":
			for j, word := range words[k+1:] {
				if strings.HasPrefix(word, ">") || strings.HasPrefix(word, "<") || strings.HasPrefix(word, "2>") {
					break
				}
				if !strings.HasPrefix(word, "-") {
					w.add(&st, dir, raw[k+1+j])
				}
			}
		}
	}
}

// redirections adds the targets of the segment's output redirections.
func (w *targetWalk) redirections(sg segment, st *deleteState, dir string) {
	masked := w.sc.masked[sg.start:sg.end]
	for i := 0; i < len(masked); i++ {
		if masked[i] != '>' {
			continue
		}
		before := masked[:i]
		j := i + 1
		if j < len(masked) && (masked[j] == '>' || masked[j] == '|') {
			j++
		}
		switch {
		case i > 0 && masked[i-1] == '<', // <> opens for reading too
			strings.Contains(before, "[[") || strings.Contains(before, "(("), // a comparison
			j < len(masked) && masked[j] == '&':                              // a descriptor duplicated
			i = j
			continue
		}
		for j < len(masked) && (masked[j] == ' ' || masked[j] == '\t') {
			j++
		}
		k := j
		for k < len(masked) && strings.IndexByte(redirEnd, masked[k]) < 0 {
			k++
		}
		if k > j {
			w.add(st, dir, w.cmd[sg.start+j:sg.start+k])
		}
		i = k - 1
	}
}

// add records the file the word names, from dir.
func (w *targetWalk) add(st *deleteState, dir, word string) {
	if w.nested && strings.ContainsAny(word, "$`") {
		return // the outer shell may have expanded it before
	}
	if p, ok := st.path(word); ok && (dir != "" || filepath.IsAbs(p)) {
		w.out = append(w.out, absIn(dir, p))
	}
}

// chdir records dir as $PWD, or $PWD as unknown.
func (st *deleteState) chdir(dir string) {
	delete(st.assigned, pwdVar)
	if dir != "" {
		st.assigned[pwdVar] = dir
	}
}

// clone is a copy of st whose assignments change apart.
func (st deleteState) clone() deleteState {
	st.assigned = maps.Clone(st.assigned)
	return st
}

// path resolves a word to the path it names (st.word): false when it stays
// unknown or is empty.
func (st *deleteState) path(word string) (string, bool) {
	p, ok := st.word(word)
	if !ok || p == "" {
		return "", false
	}
	if rest, ok := strings.CutPrefix(p, homePart); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		p = home + rest
	}
	return p, true
}

// cd is the directory a cd with the given arguments (as written and as
// words) leaves the shell in, from dir; "" when unknown. An argument that
// resolves to nothing (an empty variable) leaves it where it was, as cd ""
// does.
func (st *deleteState) cd(dir string, raw, words []string) string {
	for len(words) > 0 && strings.HasPrefix(words[0], "-") && words[0] != "-" {
		raw, words = raw[1:], words[1:]
	}
	if len(words) == 0 {
		home, _ := os.UserHomeDir()
		return home
	}
	if words[0] == "-" {
		return ""
	}
	switch p, ok := st.word(raw[0]); {
	case !ok:
		return ""
	case p == "":
		return dir
	}
	if p, ok := st.path(raw[0]); ok && (dir != "" || filepath.IsAbs(p)) {
		return absIn(dir, p)
	}
	return ""
}
