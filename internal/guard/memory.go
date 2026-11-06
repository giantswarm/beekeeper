package guard

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// A harness's own memory is read by that harness on that machine alone: what a
// session keeps there reaches no other session, harness or host. The hook
// refuses a write to it and names the desk, where the guide keeps what lasts
// (guide.desk). Reading stays free: what is there already may move to the desk.

// codexHomeEnv moves codex's folder, its memories with it.
const codexHomeEnv = "CODEX_HOME"

// harnessMemory names the harness whose own memory the absolute path p is in,
// "" for any other path. It reads the path's parts, so a managed home on an
// execution host counts as the person's does.
func harnessMemory(p string) string {
	if home := os.Getenv(codexHomeEnv); home != "" && below(filepath.Join(home, "memories"), p) {
		return harnessCodexName
	}
	parts := strings.Split(filepath.ToSlash(filepath.Clean(p)), "/")
	for i, part := range parts {
		after := parts[i+1:]
		switch part {
		case ".claude":
			if len(after) >= 3 && after[0] == "projects" && after[2] == "memory" || len(after) >= 1 && after[0] == "agent-memory" {
				return harnessClaudeName
			}
		case ".codex":
			if len(after) >= 1 && after[0] == "memories" {
				return harnessCodexName
			}
		case ".gemini":
			if len(after) == 1 && after[0] == "GEMINI.md" || len(after) >= 2 && after[0] == "antigravity" && after[1] == "knowledge" {
				return harnessAgyName
			}
		case ".omp":
			if len(after) >= 2 && after[0] == "agent" && after[1] == "memories" {
				return harnessOmpName
			}
		}
	}
	return ""
}

// The harnesses a refusal names.
const (
	harnessClaudeName = "Claude Code"
	harnessCodexName  = "Codex"
	harnessAgyName    = "agy"
	harnessOmpName    = "omp"
)

// below reports whether p is root or below it, as written.
func below(root, p string) bool {
	r, err := filepath.Rel(root, p)
	return err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}

// memoryRefusal returns why the call writes to a harness's own memory, "" when
// it does not.
func (h Hook) memoryRefusal(ev event) string {
	cwd := ev.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	switch ev.ToolName {
	case writeTool, editTool, multiEditTool, notebookTool:
		if file := toolFile(ev); file != "" {
			return h.memoryTargets([]string{absIn(cwd, file)})
		}
	case bashTool:
		cmd, _ := ev.ToolInput[commandKey].(string)
		return h.memoryTargets(writeTargets(cmd, cwd, 0))
	}
	return ""
}

// memoryTargets is memoryRefusal for absolute write targets.
func (h Hook) memoryTargets(targets []string) string {
	for _, t := range targets {
		if harness := harnessMemory(t); harness != "" {
			return h.memoryReason(t, harness)
		}
	}
	return ""
}

// memoryReason is the refusal of a write to path in harness's memory.
func (h Hook) memoryReason(path, harness string) string {
	desk := "the desk"
	if h.GuideDesk != "" {
		desk = "the desk, `" + h.GuideDesk + "` (guide.desk)"
	}
	return "Refused: `" + path + "` is " + harness + "'s own memory, which no other session, harness or machine reads. " +
		"What is worth keeping goes to " + desk + ", where the guide keeps its memory: the guide writes it there; " +
		"any other session hands it on with `beekeeper agents note`, its report or an issue."
}

// toolFile is the file a file tool's call names, "" when it names none.
func toolFile(ev event) string {
	file, _ := ev.ToolInput[filePathKey].(string)
	if file == "" {
		file, _ = ev.ToolInput[notebookPathKey].(string)
	}
	return file
}

// guideDeskFile reports whether p is a file the guide's session may write:
// inside guide.desk, links resolved, and not git's own. Commits stay the
// desk's: git commit is refused in the guide's session wherever it runs.
func (h Hook) guideDeskFile(session, p string) bool {
	if h.GuideDesk == "" || h.Guide == nil || !withinResolved(expandHome(h.GuideDesk), p) {
		return false
	}
	guide, _ := h.Guide(session)
	return guide
}

// withinResolved reports whether p, a file that may not exist yet, is below
// root once links in both are resolved. A file of git's own is not.
func withinResolved(root, p string) bool {
	if !filepath.IsAbs(root) || !filepath.IsAbs(p) {
		return false
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	p = filepath.Clean(p)
	rest := ""
	for {
		resolved, err := filepath.EvalSymlinks(p)
		if err == nil {
			p = filepath.Join(resolved, rest)
			break
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(p) == p {
			return false
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = filepath.Dir(p)
	}
	rel, err := filepath.Rel(real, p)
	if err != nil || rel == "." || !below(real, p) {
		return false
	}
	return !slices.Contains(strings.Split(rel, string(filepath.Separator)), gitDir)
}
