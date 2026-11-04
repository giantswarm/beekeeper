package guard

import (
	"os"
	"path/filepath"
	"strings"

	worktreefs "github.com/giantswarm/beekeeper/internal/worktrees"
)

// The live-worktree guard refuses a Bash command that moves, trashes or
// deletes a folder that is, or holds, a linked git worktree a live session
// or process works in (mv, rm, rmdir, trash, trash-put, gio trash|remove|
// move|rename, git worktree remove|move, also over a for loop's variable):
// a cleanup takes only the worktrees whose session ended, one by one, and a
// folder of worktrees goes only once none of them is in use. A move inside
// a worktree, or of a folder no live session works under, passes. The
// command line is read as the folder guard reads it (targetWalk): where the
// shell stands, what its variables hold, shell strings and here-documents.
// It is a best effort: paths a command reads from its input (xargs), finds
// itself (find -exec) or builds by brace expansion are not followed.

// liveWorktreeFix is what every refusal offers instead.
const liveWorktreeFix = "A cleanup takes only the worktrees whose session ended: `beekeeper worktrees` removes the finished ones, " +
	"and a single finished worktree goes on its own (`git worktree remove <path>`). Leave this one until its session ends."

// trash-cli's commands, and gio's trash subcommand.
const (
	trashCmd    = "trash"
	trashPutCmd = "trash-put"
)

// targetDirOpt is mv's and cp's long option naming the target directory.
const targetDirOpt = "--target-directory"

// liveWorktreeRefusal returns why cmd, run in cwd, moves or removes a
// folder holding a worktree a live session or process works in, "" when it
// does not. The live folders are read only for a command that moves or
// removes an existing directory.
func (h Hook) liveWorktreeRefusal(cmd, cwd string) string {
	if h.LiveDirs == nil {
		return ""
	}
	moved := walkTargets(cmd, cwd, 0, true)
	if len(moved) == 0 {
		return ""
	}
	live := h.LiveDirs()
	for _, m := range moved {
		if wt, dir := LiveWorktreeUnder(m, live); wt != "" {
			return "Refused: this command moves or removes `" + m + "`, which holds the worktree `" + wt +
				"` a live session or process works in (`" + dir + "`). " + liveWorktreeFix
		}
	}
	return ""
}

// LiveWorktreeUnder returns the linked worktree at or under target that one
// of the live folders lies in, and that folder; "", "" for none. A live
// folder inside target but in no worktree under it (a worktree that holds
// target, a plain folder) does not count.
func LiveWorktreeUnder(target string, live []string) (worktree, dir string) {
	target = canonicalPath(target)
	for _, l := range live {
		if l == "" {
			continue
		}
		l = canonicalPath(l)
		if l != target && !strings.HasPrefix(l, target+string(filepath.Separator)) {
			continue
		}
		for p := l; ; p = filepath.Dir(p) {
			if worktreefs.Linked(p) {
				return p, l
			}
			if p == target || p == filepath.Dir(p) {
				break
			}
		}
	}
	return "", ""
}

// canonicalPath resolves symlinks in p, or in its longest existing parent.
func canonicalPath(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	if parent := filepath.Dir(p); parent != p {
		return filepath.Join(canonicalPath(parent), filepath.Base(p))
	}
	return p
}

// addMoved records the existing directories (not symlinks) the command
// moves or removes, its operands resolved from dir.
func (w *targetWalk) addMoved(st *deleteState, dir, name string, args []string) {
	for _, op := range movedOperands(name, withoutRedirections(args)) {
		base := dir
		if op.base != "" {
			b, ok := w.resolve(st, dir, op.base)
			if len(b) != 1 || !ok {
				continue
			}
			base = b[0]
		}
		ps, _ := w.resolve(st, base, strings.TrimPrefix(op.word, "file://"))
		for _, p := range ps {
			if fi, err := os.Lstat(p); err == nil && fi.IsDir() {
				w.out = append(w.out, p)
			}
		}
	}
}

// loop records what a for loop's variable takes: for NAME in WORDS, each
// word resolved and its glob expanded. A loop over anything unknown takes
// nothing.
func (w *targetWalk) loop(st *deleteState, dir string, args []string) {
	if len(args) < 2 || strings.Join(shellWords(args[1]), "") != "in" {
		return
	}
	name := strings.Join(shellWords(args[0]), "")
	var vals []string
	for _, a := range args[2:] {
		ps, ok := w.resolve(st, dir, a)
		if !ok {
			return
		}
		vals = append(vals, ps...)
	}
	if w.loops == nil {
		w.loops = map[string][]string{}
	}
	w.loops[name] = vals
}

// resolve returns the absolute paths a word names from dir: a for loop's
// variable by what it takes, else the word as the folder guard resolves it
// (st.path) with its glob expanded; false when it stays unknown.
func (w *targetWalk) resolve(st *deleteState, dir, word string) ([]string, bool) {
	if v := strings.Join(shellWords(word), ""); plainVar.MatchString(v) {
		if vals, ok := w.loops[strings.Trim(v, "${}")]; ok {
			return vals, true
		}
	}
	if w.nested && strings.ContainsAny(word, "$`") {
		return nil, false // the outer shell may have expanded it before
	}
	p, ok := st.path(word)
	if !ok || dir == "" && !filepath.IsAbs(p) {
		return nil, false
	}
	p = absIn(dir, p)
	if !strings.ContainsAny(p, globChar) {
		return []string{p}, true
	}
	matches, _ := filepath.Glob(p)
	return matches, true
}

// withoutRedirections drops the redirections among a command's arguments
// (as written): 2>/dev/null, > file, 2>&1, <<< word.
func withoutRedirections(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		w := strings.Join(shellWords(args[i]), "")
		if redirection.MatchString(args[i]) {
			if bareRedirect.MatchString(w) {
				i++ // the target is the next word
			}
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// operand is a word a command moves or removes; base, when set, is the
// word naming the folder it is relative to (git -C).
type operand struct {
	word, base string
}

// movedOperands returns the operands (as written) the command moves away or
// removes: every source of mv and gio move, every operand of rm, rmdir,
// trash, trash-put and gio trash|remove, the first of gio rename and git
// worktree remove|move. Any other command moves nothing.
func movedOperands(name string, args []string) []operand {
	switch name {
	case "mv":
		ops, target := operands(args, "S", "t")
		if target {
			return ops
		}
		if len(ops) < 2 {
			return nil
		}
		return ops[:len(ops)-1]
	case rmCmd, transcriptRmdir, trashCmd, trashPutCmd, "gio-trash":
		ops, _ := operands(args, "", "")
		return ops
	case "gio":
		if len(args) == 0 {
			return nil
		}
		sub := strings.Join(shellWords(args[0]), "")
		ops, _ := operands(args[1:], "", "")
		switch sub {
		case trashCmd, "remove":
			return ops
		case "move":
			if len(ops) < 2 {
				return nil
			}
			return ops[:len(ops)-1]
		case "rename":
			if len(ops) > 0 {
				return ops[:1]
			}
		}
	case gitCmd:
		return gitWorktreeOperands(args)
	}
	return nil
}

// gitWorktreeOperands returns the worktree git worktree remove or move
// takes away, relative to git's -C folder.
func gitWorktreeOperands(args []string) []operand {
	base := ""
	i := 0
	for ; i < len(args); i++ {
		a := strings.Join(shellWords(args[i]), "")
		if !strings.HasPrefix(a, "-") {
			break
		}
		if (a == "-C" || a == "-c") && i+1 < len(args) {
			i++
			if a == "-C" {
				base = args[i]
			}
		}
	}
	if i+1 >= len(args) || strings.Join(shellWords(args[i]), "") != litWorktree {
		return nil
	}
	if s := strings.Join(shellWords(args[i+1]), ""); s != "remove" && s != "move" {
		return nil
	}
	ops, _ := operands(args[i+2:], "", "")
	if len(ops) == 0 {
		return nil
	}
	ops[0].base = base
	return ops[:1]
}

// operands splits a command's arguments into its operands, skipping its
// options. value and target are the short options (letters) that take a
// value; target's value is the target directory (mv -t, also clustered as
// -vt DIR or -tDIR, and --target-directory), and target reports that one
// was given.
func operands(args []string, value, target string) (ops []operand, hasTarget bool) {
	rest := false
	for i := 0; i < len(args); i++ {
		a := strings.Join(shellWords(args[i]), "")
		switch {
		case rest || a == "-" || !strings.HasPrefix(a, "-"):
			ops = append(ops, operand{word: args[i]})
		case a == "--":
			rest = true
		case strings.HasPrefix(a, "--"):
			long, _, inline := strings.Cut(a, "=")
			if target != "" && long == targetDirOpt {
				hasTarget = true
			}
			if !inline && (target != "" && long == targetDirOpt || value != "" && long == "--suffix") {
				i++
			}
		default:
			// A cluster of short options; the first that takes a value
			// takes the rest of the word, or the next word.
			for j := 1; j < len(a); j++ {
				c := a[j : j+1]
				if !strings.Contains(value+target, c) {
					continue
				}
				if strings.Contains(target, c) {
					hasTarget = true
				}
				if j == len(a)-1 {
					i++
				}
				break
			}
		}
	}
	return ops, hasTarget
}
