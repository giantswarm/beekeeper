//go:build unix

package proc

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// tool writes an executable name into dir and answers its path.
func tool(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // an executable the test runs
		t.Fatal(err)
	}
	return p
}

// noTools leaves PATH, $GOBIN, $GOPATH and the home directory empty.
func noTools(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", "")
	t.Setenv("HOME", t.TempDir())
}

func TestLookToolPrefersPATH(t *testing.T) {
	noTools(t)
	onPath := tool(t, t.TempDir(), "kindish")
	t.Setenv("PATH", filepath.Dir(onPath))
	gobin := t.TempDir()
	tool(t, gobin, "kindish")
	t.Setenv("GOBIN", gobin)
	if got, err := LookTool("kindish"); err != nil || got != onPath {
		t.Errorf("LookPath = %q, %v; want %q", got, err, onPath)
	}
}

func TestLookToolFallsBackToTheGoToolDirs(t *testing.T) {
	for name, place := range map[string]func(t *testing.T) string{
		"GOBIN": func(t *testing.T) string {
			dir := t.TempDir()
			t.Setenv("GOBIN", dir)
			return dir
		},
		"GOPATH/bin": func(t *testing.T) string {
			dir := t.TempDir()
			t.Setenv("GOPATH", t.TempDir()+string(os.PathListSeparator)+dir)
			return filepath.Join(dir, "bin")
		},
		"~/go/bin":  func(*testing.T) string { return filepath.Join(os.Getenv("HOME"), "go", "bin") },
		"~/.go/bin": func(*testing.T) string { return filepath.Join(os.Getenv("HOME"), ".go", "bin") },
	} {
		t.Run(name, func(t *testing.T) {
			noTools(t)
			want := tool(t, place(t), "kindish")
			if got, err := LookTool("kindish"); err != nil || got != want {
				t.Errorf("LookPath = %q, %v; want %q", got, err, want)
			}
		})
	}
}

func TestLookToolSkipsWhatIsNotExecutable(t *testing.T) {
	noTools(t)
	gobin := t.TempDir()
	t.Setenv("GOBIN", gobin)
	if err := os.WriteFile(filepath.Join(gobin, "kindish"), []byte("#!/bin/sh\n"), 0o644); err != nil { //nolint:gosec // a file the test must not run
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(gobin, "dirish"), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"kindish", "dirish"} {
		if _, err := LookTool(name); !errors.Is(err, exec.ErrNotFound) {
			t.Errorf("LookTool(%s) = %v", name, err)
		}
	}
}

func TestLookToolNamesTheDirsLookedIn(t *testing.T) {
	noTools(t)
	gobin := t.TempDir()
	t.Setenv("GOBIN", gobin)
	_, err := LookTool("kindish")
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("LookPath = %v", err)
	}
	for _, dir := range []string{gobin, filepath.Join(os.Getenv("HOME"), "go", "bin"), filepath.Join(os.Getenv("HOME"), ".go", "bin")} {
		if !strings.Contains(err.Error(), "$PATH, nor in ") || !strings.Contains(err.Error(), dir) {
			t.Errorf("LookPath = %v; want %s named", err, dir)
		}
	}
}

func TestLookToolLeavesAPathToExec(t *testing.T) {
	noTools(t)
	gobin := t.TempDir()
	t.Setenv("GOBIN", gobin)
	if _, err := LookTool(filepath.Join(t.TempDir(), "kindish")); err == nil || strings.Contains(err.Error(), "nor in") {
		t.Errorf("an absent path = %v", err)
	}
	p := tool(t, t.TempDir(), "kindish")
	if got, err := LookTool(p); err != nil || got != p {
		t.Errorf("LookTool(%s) = %q, %v", p, got, err)
	}
}
