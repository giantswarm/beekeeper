package proc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLookToolWindowsGoToolExecutables(t *testing.T) {
	gobin := windowsToolDirs(t)
	want := filepath.Join(gobin, "kindish.exe")
	// LookPath inspects the filename and metadata; it does not execute
	// this synthetic file. Windows files need no Unix execute bits.
	if err := os.WriteFile(want, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"kindish", "kindish.exe"} {
		if got, err := LookTool(name); err != nil || got != want {
			t.Errorf("LookTool(%q) = %q, %v; want %q", name, got, err, want)
		}
	}
}

func TestLookToolWindowsLeavesBothPathSeparatorsToExec(t *testing.T) {
	gobin := windowsToolDirs(t)
	dir := filepath.Join(gobin, "proc-lookpath-missing")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kindish.exe"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{`proc-lookpath-missing/kindish`, `proc-lookpath-missing\kindish`} {
		if got, err := LookTool(name); err == nil || got != "" || strings.Contains(err.Error(), "nor in") {
			t.Errorf("explicit path %q reached fallback: %q, %v", name, got, err)
		}
	}
}

func windowsToolDirs(t *testing.T) string {
	t.Helper()
	gobin := t.TempDir()
	t.Setenv("PATH", t.TempDir())
	t.Setenv("GOBIN", gobin)
	t.Setenv("GOPATH", "")
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("PATHEXT", ".EXE;.CMD")
	return gobin
}
