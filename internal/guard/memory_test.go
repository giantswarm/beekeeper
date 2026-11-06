package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const memoryFile = "/home/a/.claude/projects/-work/memory/MEMORY.md"

// memoryAnswer is the hook's raw answer to call.
func memoryAnswer(t *testing.T, h Hook, call map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	return string(h.Decide(raw))
}

func TestHarnessMemoryIsRefusedAndNamesTheDesk(t *testing.T) {
	h := hook()
	h.GuideDesk = "~/desk"
	for name, call := range map[string]map[string]any{
		"claude write":   toolEvent(writeTool, map[string]any{filePathKey: memoryFile}),
		"claude edit":    toolEvent(editTool, map[string]any{filePathKey: "/home/a/.claude/agent-memory/x.md"}),
		"bash redirect":  bashCall("echo x >> " + memoryFile),
		"codex memories": toolEvent(writeTool, map[string]any{filePathKey: "/home/a/.codex/memories/x.md"}),
		"agy global":     toolEvent(writeTool, map[string]any{filePathKey: "/home/a/.gemini/GEMINI.md"}),
		"agy knowledge":  toolEvent(writeTool, map[string]any{filePathKey: "/home/a/.gemini/antigravity/knowledge/k/x.md"}),
		"omp memories":   toolEvent(writeTool, map[string]any{filePathKey: "/home/a/.omp/agent/memories/x.md"}),
	} {
		t.Run(name, func(t *testing.T) {
			out := memoryAnswer(t, h, call)
			if !strings.Contains(out, decisionDeny) || !strings.Contains(out, "~/desk") {
				t.Fatalf("got %s, want a refusal naming the desk", out)
			}
		})
	}
}

func TestHarnessMemoryLeavesOtherCallsAlone(t *testing.T) {
	for name, call := range map[string]map[string]any{
		"read":          toolEvent(readTool, map[string]any{filePathKey: "/tmp/x/memory/MEMORY.md"}),
		"plain memory":  toolEvent(writeTool, map[string]any{filePathKey: "/tmp/x/memory/MEMORY.md"}),
		"project rules": toolEvent(writeTool, map[string]any{filePathKey: "/tmp/x/GEMINI.md"}),
		"claude rules":  toolEvent(writeTool, map[string]any{filePathKey: "/tmp/.claude/settings.json"}),
	} {
		t.Run(name, func(t *testing.T) {
			if out := memoryAnswer(t, hook(), call); strings.Contains(out, "own memory") {
				t.Fatalf("got %s, want no memory refusal", out)
			}
		})
	}
}

func TestCodexHomeMemoriesCount(t *testing.T) {
	t.Setenv(codexHomeEnv, "/srv/codex-home")
	if got := harnessMemory("/srv/codex-home/memories/x.md"); got != harnessCodexName {
		t.Fatalf("harnessMemory = %q", got)
	}
	if got := harnessMemory("/srv/codex-home/rules.md"); got != "" {
		t.Fatalf("harnessMemory = %q", got)
	}
}

func TestExecutionMemoryRefusalNamesTheDesk(t *testing.T) {
	dir := t.TempDir()
	h := Hook{GuideDesk: "~/desk"}
	raw, err := json.Marshal(map[string]any{executionToolName: writeTool, sessionKey: "s", executionWorkingDir: dir, executionToolInput: map[string]any{filePathKey: memoryFile}})
	if err != nil {
		t.Fatal(err)
	}
	if out := string(h.DecideExecution(raw, "s", dir, nil)); !strings.Contains(out, "~/desk") {
		t.Fatalf("got %s, want the refusal naming the desk", out)
	}
}

func deskHook(t *testing.T) (Hook, string) {
	t.Helper()
	desk := filepath.Join(t.TempDir(), "desk")
	if err := os.MkdirAll(filepath.Join(desk, gitDir), 0o700); err != nil {
		t.Fatal(err)
	}
	h := guideHook()
	h.GuideDesk = desk
	return h, desk
}

func sessionEvent(call map[string]any, session string) map[string]any {
	call[sessionKey] = session
	return call
}

func TestGuideWritesInItsDeskAlone(t *testing.T) {
	h, desk := deskHook(t)
	outside := filepath.Join(filepath.Dir(desk), "link")
	if err := os.Symlink("/tmp", filepath.Join(desk, "out")); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		file    string
		refused bool
	}{
		"new file":          {filepath.Join(desk, "memory/today.md"), false},
		"dotdot":            {filepath.Join(desk, "../other.md"), true},
		"beside the desk":   {outside, true},
		"through a link":    {filepath.Join(desk, "out/x.md"), true},
		"git's own files":   {filepath.Join(desk, gitDir, "hooks/post-commit"), true},
		"the folder itself": {desk, true},
	} {
		t.Run(name, func(t *testing.T) {
			out := memoryAnswer(t, h, sessionEvent(toolEvent(writeTool, map[string]any{filePathKey: c.file}), guideSession))
			if got := strings.Contains(out, decisionDeny); got != c.refused {
				t.Fatalf("refused = %v, want %v: %s", got, c.refused, out)
			}
		})
	}
	if out := memoryAnswer(t, h, sessionEvent(bashCall("git -C "+desk+" commit -am x"), guideSession)); !strings.Contains(out, decisionDeny) {
		t.Fatalf("a commit in the desk stays the desk's: %s", out)
	}
}

func TestDeskFilesPassThePersonFolderGuardForTheGuideOnly(t *testing.T) {
	h, desk := deskHook(t)
	h.PersonFolders = []string{filepath.Dir(desk)}
	file := filepath.Join(desk, "notes.md")
	if out := memoryAnswer(t, h, sessionEvent(toolEvent(writeTool, map[string]any{filePathKey: file}), guideSession)); strings.Contains(out, decisionDeny) {
		t.Fatalf("the guide's desk file: %s", out)
	}
	if out := memoryAnswer(t, h, sessionEvent(toolEvent(writeTool, map[string]any{filePathKey: file}), "worker")); !strings.Contains(out, decisionDeny) {
		t.Fatalf("another session's file in the desk checkout: %s", out)
	}
}

func TestGuideRefusalNamesItsDesk(t *testing.T) {
	h, desk := deskHook(t)
	out := memoryAnswer(t, h, sessionEvent(toolEvent(editTool, map[string]any{filePathKey: scratch}), guideSession))
	if !strings.Contains(out, desk) {
		t.Fatalf("got %s, want the desk named", out)
	}
}
