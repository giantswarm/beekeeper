package github

import (
	"os"
	"path/filepath"
	"testing"
)

// Resolving a stored-gh executable must honor Windows executable extensions
// before resolving symlinks. The fixture is never executed or used as a token.
func TestBudgetGHFindsWindowsExecutable(t *testing.T) {
	bin := t.TempDir()
	exe := filepath.Join(bin, "gh.exe")
	if err := os.WriteFile(exe, []byte("synthetic executable lookup fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("PATHEXT", ".EXE")
	got, err := budgetGH()
	if err != nil || got != exe {
		t.Fatalf("budgetGH = %q, %v; want %q", got, err, exe)
	}
}
