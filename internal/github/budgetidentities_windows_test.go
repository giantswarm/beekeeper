package github

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBudgetGHResolvesWindowsExecutableExtension(t *testing.T) {
	bin := t.TempDir()
	want := filepath.Join(bin, "gh.exe")
	// Deliberately not an executable image: resolution must never run gh or
	// request a credential.
	if err := os.WriteFile(want, []byte("offline resolution fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("PATHEXT", ".EXE")
	got, err := budgetGH()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("budgetGH() = %q, want %q", got, want)
	}
}
