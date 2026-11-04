package guard

import (
	"os"
	"path/filepath"
	"strings"
)

// ProtectedFolders discovers org folders without treating clones or the
// designated worktree places as orgs. A linked checkout has a .git file.
func ProtectedFolders(home, projects string, places ...string) []string {
	out := []string{home, projects}
	children, _ := os.ReadDir(projects)
	for _, child := range children {
		dir := filepath.Join(projects, child.Name())
		allowed := false
		for _, place := range places {
			if isUnderDir(dir, place) {
				allowed = true
				break
			}
		}
		if allowed || !child.IsDir() || checkoutAt(dir) {
			continue
		}
		repos, _ := os.ReadDir(dir)
		for _, repo := range repos {
			if checkoutAt(filepath.Join(dir, repo.Name())) {
				out = append(out, dir)
				break
			}
		}
	}
	return out
}

func checkoutAt(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, gitDir))
	return err == nil
}

// EntryRules is shared by the hook and the rules handed to a worker.
func EntryRules(home, projects string, worktrees, places []string, scratch string) string {
	return "Protected folders: " + strings.Join(ProtectedFolders(home, projects, worktrees...), ", ") +
		". Create no new entry directly in these folders. Drafts go in the session scratchpad, " + scratch +
		"; state, logs and backups in home.places: " + strings.Join(places, ", ") +
		". Worktrees and scratch checkouts go in projects.worktrees: " + strings.Join(worktrees, ", ") +
		", with layout <repo>/<branch>. Use `git -C <clone> worktree add <place>/<repo>/<branch> -b <branch> origin/main`. " +
		"A path the person named for the task is created by the person or with their go-ahead. " +
		"Stop on an empty directory variable: `cd \"${dir:?}\"`."
}

func (h Hook) entryRefusal(ev event) string {
	if h.ProjectsDir == "" {
		return ""
	}
	home, _ := guardHomeDir()
	cwd := ev.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	var targets []string
	switch ev.ToolName {
	case writeTool, notebookTool:
		file, _ := ev.ToolInput[filePathKey].(string)
		if file == "" {
			file, _ = ev.ToolInput[notebookPathKey].(string)
		}
		if file != "" {
			targets = append(targets, absIn(cwd, strings.ReplaceAll(strings.ReplaceAll(file, "${HOME}", home), "$HOME", home)))
		}
	case bashTool:
		cmd, _ := ev.ToolInput[commandKey].(string)
		targets = collectTargets(cmd, cwd, 0, false, true)
	}
	folders := ProtectedFolders(home, h.ProjectsDir, h.WorktreePlaces...)
	for _, target := range targets {
		if _, err := os.Lstat(target); err == nil {
			continue
		}
		allowed := false
		for _, place := range h.WorktreePlaces {
			if isUnderDir(target, place) {
				allowed = true
				break
			}
		}
		if allowed {
			continue
		}
		for _, folder := range folders {
			if filepath.Dir(target) == folder {
				return "Refused: creating a new entry directly in " + folder + ". " + EntryRules(home, h.ProjectsDir, h.WorktreePlaces, h.HomePlaces, h.scratchpad(ev))
			}
		}
	}
	return ""
}

// entryArgs skips options with values, retaining operands as shell words.
func entryArgs(args []string, short string, long ...string) []string {
	var out []string
	rest := false
	for i := 0; i < len(args); i++ {
		a := strings.Join(shellWords(args[i]), "")
		if rest || !strings.HasPrefix(a, "-") || a == "-" {
			out = append(out, args[i])
			continue
		}
		if a == "--" {
			rest = true
			continue
		}
		takes := false
		for _, opt := range long {
			if a == opt {
				takes = true
			}
		}
		if !strings.HasPrefix(a, "--") {
			for j := 1; j < len(a); j++ {
				if strings.ContainsRune(short, rune(a[j])) {
					takes = j == len(a)-1
					break
				}
			}
		}
		if takes {
			i++
		}
	}
	return out
}

func (w *targetWalk) addEntries(st *deleteState, dir, name string, args []string) {
	switch name {
	case litMkdir, "touch":
		short := "m"
		longs := []string{"--mode"}
		if name == "touch" {
			short = "drt"
			longs = []string{"--date", litReference, "--time"}
		}
		for _, a := range entryArgs(args, short, longs...) {
			w.add(st, dir, a)
		}
	case "cp", "mv":
		ops := entryArgs(args, "St", "--suffix", "--target-directory")
		target := ""
		for i, a := range args {
			a = strings.Join(shellWords(a), "")
			switch {
			case (a == "-t" || a == "--target-directory") && i+1 < len(args):
				target = args[i+1]
			case strings.HasPrefix(a, "--target-directory="):
				target = strings.TrimPrefix(a, "--target-directory=")
			case strings.HasPrefix(a, "-t") && len(a) > 2:
				target = strings.TrimPrefix(a, "-t")
			}
		}
		if target == "" {
			if len(ops) < 2 {
				return
			}
			target, ops = ops[len(ops)-1], ops[:len(ops)-1]
		}
		p, ok := st.path(target)
		if !ok {
			return
		}
		dest := absIn(dir, p)
		if isDir(dest) {
			for _, src := range ops {
				if p, ok := st.path(src); ok {
					w.out = append(w.out, filepath.Join(dest, filepath.Base(p)))
				}
			}
		} else {
			w.add(st, dir, target)
		}
	case gitCmd:
		for len(args) > 0 {
			a := strings.Join(shellWords(args[0]), "")
			if a == "-C" && len(args) > 1 {
				if p, ok := st.path(args[1]); ok {
					dir = absIn(dir, p)
				} else {
					return
				}
				args = args[2:]
			} else if (a == "-c" || a == litGitDir || a == "--work-tree") && len(args) > 1 {
				args = args[2:]
			} else if strings.HasPrefix(a, "-") {
				args = args[1:]
			} else {
				break
			}
		}
		if len(args) == 0 {
			return
		}
		sub := strings.Join(shellWords(args[0]), "")
		if sub == litClone {
			w.cloneEntry(st, dir, args[1:], false)
		}
		if sub == "worktree" && len(args) > 1 && strings.Join(shellWords(args[1]), "") == worktreeAdd {
			ops := entryArgs(args[2:], "bB", "--branch", "--reason", "--orphan")
			if len(ops) > 0 {
				w.add(st, dir, ops[0])
			}
		}
	case "gh":
		if len(args) > 1 && args[0] == "repo" && args[1] == "clone" {
			w.cloneEntry(st, dir, args[2:], true)
		}
	}
}

func (w *targetWalk) cloneEntry(st *deleteState, dir string, args []string, gh bool) {
	short := "boucj"
	longs := []string{"--branch", "--origin", "--upload-pack", "--config", "--jobs", "--depth", "--shallow-since", "--shallow-exclude", "--reference", "--reference-if-able", "--separate-git-dir", "--template", "--filter", "--revision"}
	if gh {
		short = "opu"
		longs = []string{"--origin", "--protocol", "--upstream-remote-name"}
		for i, a := range args {
			if a == "--" {
				args = args[:i]
				break
			}
		}
	}
	ops := entryArgs(args, short, longs...)
	if len(ops) > 1 {
		w.add(st, dir, ops[1])
		return
	}
	if len(ops) == 0 {
		return
	}
	url, ok := st.word(ops[0])
	if !ok {
		return
	}
	repo := filepath.Base(strings.TrimRight(url, "/"))
	if i := strings.LastIndex(repo, ":"); i >= 0 {
		repo = repo[i+1:]
	}
	repo = strings.TrimSuffix(repo, ".git")
	if repo != "" {
		w.out = append(w.out, absIn(dir, repo))
	}
}
