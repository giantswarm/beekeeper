package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	claudeDir  = ".claude"
	memoryRel  = "projects/-work/memory"
	linkedFile = "x.md"
)

// linkHome is a scratch home, its path free of links.
func linkHome(t *testing.T) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv(executionHomeEnv, "")
	return home
}

func mkdir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func link(t *testing.T, target, name string) string {
	t.Helper()
	if err := os.Symlink(target, name); err != nil {
		t.Fatal(err)
	}
	return name
}

// A link into a memory folder is the folder: the write lands in the same files.
func TestMemoryRefusalFollowsDirectoryLinks(t *testing.T) {
	home := linkHome(t)
	memory := mkdir(t, filepath.Join(home, claudeDir, memoryRel))
	notes := link(t, memory, filepath.Join(home, "notes"))
	if err := os.WriteFile(filepath.Join(memory, "old.md"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]map[string]any{
		"write a new file": toolEvent(writeTool, map[string]any{filePathKey: filepath.Join(notes, "new.md")}),
		"edit a file":      toolEvent(editTool, map[string]any{filePathKey: filepath.Join(notes, "old.md")}),
		"deeper":           toolEvent(writeTool, map[string]any{filePathKey: filepath.Join(notes, "a/b/new.md")}),
		"redirect":         bashCall("echo x >> " + filepath.Join(notes, "new.md")),
		"tee":              bashCall("echo x | tee " + filepath.Join(notes, "new.md")),
	} {
		t.Run(name, func(t *testing.T) {
			if out := memoryAnswer(t, hook(), call); !strings.Contains(out, decisionDeny) || !strings.Contains(out, "own memory") {
				t.Fatalf("got %s, want the memory refusal", out)
			}
		})
	}
}

func TestMemoryRefusalFollowsAFileLink(t *testing.T) {
	home := linkHome(t)
	memory := mkdir(t, filepath.Join(home, claudeDir, memoryRel))
	target := filepath.Join(memory, linkedFile)
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := memoryWrite(link(t, target, filepath.Join(home, "memo.md"))); got != harnessClaudeName {
		t.Fatalf("memoryWrite = %q", got)
	}
}

// A harness folder kept elsewhere behind a link: the real path of its memory
// names no harness, and a write there is still a write to the memory.
func TestMemoryRefusalReachesALinkedHarnessFolder(t *testing.T) {
	home := linkHome(t)
	kept := filepath.Join(home, "dotfiles", "claude")
	memory := mkdir(t, filepath.Join(kept, memoryRel))
	link(t, kept, filepath.Join(home, claudeDir))
	if got := memoryWrite(filepath.Join(memory, linkedFile)); got != harnessClaudeName {
		t.Fatalf("memoryWrite = %q", got)
	}
	if got := memoryWrite(filepath.Join(kept, "settings.json")); got != "" {
		t.Fatalf("an ordinary file of the folder: %q", got)
	}
}

func TestMemoryRefusalReachesALinkedCodexHome(t *testing.T) {
	home := linkHome(t)
	kept := filepath.Join(home, "kept")
	memories := mkdir(t, filepath.Join(kept, "memories"))
	t.Setenv(codexHomeEnv, link(t, kept, filepath.Join(home, "codex-home")))
	if got := memoryWrite(filepath.Join(memories, linkedFile)); got != harnessCodexName {
		t.Fatalf("memoryWrite = %q", got)
	}
}

func TestMemoryRefusalLeavesOrdinaryLinksAlone(t *testing.T) {
	home := linkHome(t)
	other := mkdir(t, filepath.Join(home, "other"))
	elsewhere := link(t, other, filepath.Join(home, "elsewhere"))
	if got := memoryWrite(filepath.Join(elsewhere, linkedFile)); got != "" {
		t.Fatalf("memoryWrite = %q", got)
	}
}
