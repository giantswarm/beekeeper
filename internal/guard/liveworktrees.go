package guard

import (
	"os"
	"path"
	"path/filepath"
	"strings"

	worktreefs "github.com/giantswarm/beekeeper/internal/worktrees"
)

// The live-worktree guard refuses a Bash command that moves, trashes or
// deletes a folder that is, or holds, a linked git worktree a live session
// or process works in (mv, rm, rmdir, trash, trash-put, gio trash|remove|
// move|rename, git worktree remove|move): a cleanup takes only the worktrees
// whose session ended, one by one, and a folder of worktrees goes only once
// none of them is in use. A move inside a worktree, or of a folder no live
// session works under, passes.

// liveWorktreeFix is what every refusal offers instead.
const liveWorktreeFix = "A cleanup takes only the worktrees whose session ended: `beekeeper worktrees` removes the finished ones, " +
	"and a single finished worktree goes on its own (`git worktree remove <path>`). Leave this one until its session ends."

// trash-cli's commands, and gio's trash subcommand.
const (
	trashCmd    = "trash"
	trashPutCmd = "trash-put"
)

// liveWorktreeDepth bounds how deep shell strings (sh -c '…') are followed.
const liveWorktreeDepth = 4

// movedPath is a folder a command moves or removes, as written and as an
// absolute path.
type movedPath struct {
	written, abs string
}

// liveWorktreeRefusal returns why cmd, run in cwd, moves or removes a
// folder holding a worktree a live session or process works in, "" when it
// does not. The live folders are read only for a command that moves or
// removes an existing directory.
func (h Hook) liveWorktreeRefusal(cmd, cwd string) string {
	if h.LiveDirs == nil {
		return ""
	}
	moved := movedDirs(cmd, cwd, 0)
	if len(moved) == 0 {
		return ""
	}
	live := h.LiveDirs()
	for _, m := range moved {
		if wt, dir := LiveWorktreeUnder(m.abs, live); wt != "" {
			what := "`" + m.written + "`"
			if m.written != m.abs {
				what += " (" + m.abs + ")"
			}
			return "Refused: this command moves or removes " + what + ", which holds the worktree `" + wt +
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

// movedDirs lists the existing directories (not symlinks) cmd moves or
// removes. A target whose value the line does not tell (a command
// substitution, a variable neither assigned nor in the environment) is
// passed over: the delete guard refuses the dangerous ones.
func movedDirs(cmd, cwd string, depth int) []movedPath {
	if depth > liveWorktreeDepth {
		return nil
	}
	sc := scanShell(cmd)
	st := deleteState{assigned: map[string]string{}, env: os.Getenv}
	dir := cwd
	var out []movedPath
	for _, raw := range rawCommands(sc.plain) {
		for len(raw) > 0 && keywords[raw[0]] {
			raw = raw[1:]
		}
		words := make([]string, len(raw))
		for i, r := range raw {
			words[i] = strings.Join(shellWords(r), "")
		}
		k := commandAt(words)
		if k == len(words) {
			st.assign(raw, words)
			continue
		}
		name, args := path.Base(words[k]), raw[k+1:]
		switch {
		case name == "cd" && len(args) > 0:
			if ps := st.paths(args[0], dir, false); len(ps) == 1 {
				dir = ps[0]
			}
			continue
		case declarations[name]:
			st.assign(raw[k+1:], words[k+1:])
			continue
		case shells[name]:
			for _, a := range words[k+1:] {
				if strings.ContainsAny(a, " \t\n;|&") {
					out = append(out, movedDirs(a, dir, depth+1)...)
				}
			}
			continue
		}
		for _, op := range movedOperands(name, args) {
			base := dir
			if op.base != "" {
				if ps := st.paths(op.base, dir, false); len(ps) == 1 {
					base = ps[0]
				} else {
					continue
				}
			}
			for _, p := range st.paths(op.word, base, true) {
				if fi, err := os.Lstat(p); err == nil && fi.IsDir() {
					out = append(out, movedPath{written: strings.Join(shellWords(op.word), ""), abs: p})
				}
			}
		}
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
		ops, target := operands(args, map[string]bool{"-S": true, "--suffix": true}, map[string]bool{"-t": true, "--target-directory": true})
		if target {
			return ops
		}
		if len(ops) < 2 {
			return nil
		}
		return ops[:len(ops)-1]
	case rmCmd, transcriptRmdir, trashCmd, trashPutCmd, "gio-trash":
		ops, _ := operands(args, nil, nil)
		return ops
	case "gio":
		if len(args) == 0 {
			return nil
		}
		sub := strings.Join(shellWords(args[0]), "")
		ops, _ := operands(args[1:], nil, nil)
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
	case "git":
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
	if i+1 >= len(args) || strings.Join(shellWords(args[i]), "") != "worktree" {
		return nil
	}
	if s := strings.Join(shellWords(args[i+1]), ""); s != "remove" && s != "move" {
		return nil
	}
	ops, _ := operands(args[i+2:], nil, nil)
	if len(ops) == 0 {
		return nil
	}
	ops[0].base = base
	return ops[:1]
}

// operands splits a command's arguments into its operands, skipping its
// options and the values of those in values; an option in targets takes the
// target directory, and target reports that one was given.
func operands(args []string, values, targets map[string]bool) (ops []operand, target bool) {
	rest := false
	for i := 0; i < len(args); i++ {
		a := strings.Join(shellWords(args[i]), "")
		switch {
		case rest || a == "-" || !strings.HasPrefix(a, "-"):
			ops = append(ops, operand{word: args[i]})
		case a == "--":
			rest = true
		case targets[a]:
			target = true
			i++
		case targets[strings.SplitN(a, "=", 2)[0]] || strings.HasPrefix(a, "-t") && targets["-t"]:
			target = true
		case values[a]:
			i++
		}
	}
	return ops, target
}

// paths resolves a word to the absolute paths it names in dir: its
// variables by the values the line assigned or the environment holds, ~ as
// the home directory, a glob by its matches when glob is set. A word whose
// value is unknown names nothing.
func (st *deleteState) paths(word, dir string, glob bool) []string {
	lit, exps := st.expansions(word)
	home, _ := os.UserHomeDir()
	for i, e := range exps {
		v, ok := st.known(e.token)
		if !ok {
			return nil
		}
		exps[i].value = v
	}
	p := strings.ReplaceAll(st.resolve(lit, exps), homePart, home)
	if p == "" || strings.Contains(p, opaquePart) || strings.Contains(p, homePart) {
		return nil
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	p = filepath.Clean(p)
	if !glob || !strings.ContainsAny(p, globChar) {
		return []string{p}
	}
	matches, _ := filepath.Glob(p)
	return matches
}

// known is the value of a parameter expansion when the line or the
// environment tells it: $NAME, ${NAME} and ${NAME:?…} of an assigned or
// exported variable, $HOME; never a command substitution or another
// operator.
func (st *deleteState) known(token string) (string, bool) {
	if token[0] == '`' || strings.HasPrefix(token, "$(") {
		return "", false
	}
	name, op := token[1:], ""
	if inner, ok := strings.CutPrefix(token, "${"); ok {
		inner = strings.TrimSuffix(inner, "}")
		n := len(inner) - len(strings.TrimLeft(inner, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_"))
		name, op = inner[:n], inner[n:]
	}
	if op != "" && !strings.HasPrefix(op, ":?") && !strings.HasPrefix(op, "?") {
		return "", false
	}
	if name == homeVar {
		return homePart, true
	}
	if v, ok := st.assigned[name]; ok {
		return v, v != ""
	}
	if v := st.environ(name); v != "" {
		return v, true
	}
	return "", false
}
