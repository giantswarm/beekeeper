package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Windows lookup only needs an ordinary file, not a runnable PE image.
func windowsTool(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func windowsToolEnv(t *testing.T) string {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")
	t.Setenv("GOPATH", "")
	t.Setenv("USERPROFILE", t.TempDir())
	gobin := t.TempDir()
	t.Setenv("GOBIN", gobin)
	return gobin
}

func TestLookPathWindowsFallback(t *testing.T) {
	gobin := windowsToolEnv(t)
	want := windowsTool(t, gobin, "kindish.exe")
	for _, name := range []string{"kindish", "kindish.exe"} {
		t.Run(name, func(t *testing.T) {
			if got, err := LookPath(name); err != nil || !strings.EqualFold(got, want) {
				t.Fatalf("LookPath(%q) = %q, %v; want %q", name, got, err, want)
			}
		})
	}
}

func TestLookPathWindowsPATHEXT(t *testing.T) {
	gobin := windowsToolEnv(t)
	t.Setenv("PATHEXT", ".CMD;.EXE")
	want := windowsTool(t, gobin, "kindish.cmd")
	windowsTool(t, gobin, "kindish.exe")
	if got, err := LookPath("kindish"); err != nil || !strings.EqualFold(got, want) {
		t.Fatalf("LookPath = %q, %v; want %q", got, err, want)
	}
}

func TestLookPathWindowsExplicitPaths(t *testing.T) {
	gobin := windowsToolEnv(t)
	windowsTool(t, gobin, filepath.Join("sub", "kindish.exe"))
	// Make the explicit paths absent regardless of the test's working directory.
	t.Chdir(t.TempDir())
	for _, name := range []string{"sub/kindish", `sub\kindish`} {
		want, wantErr := exec.LookPath(name)
		got, err := LookPath(name)
		if wantErr == nil {
			t.Fatalf("fixture unexpectedly resolved %q", name)
		}
		if got != want || err == nil || err.Error() != wantErr.Error() {
			t.Errorf("LookPath(%q) = %q, %v; exec.LookPath = %q, %v", name, got, err, want, wantErr)
		}
	}
}

func TestLookPathWindowsDirectoryOrder(t *testing.T) {
	gobin := windowsToolEnv(t)
	gopath := t.TempDir()
	t.Setenv("GOPATH", gopath)
	windowsTool(t, filepath.Join(gopath, "bin"), "kindish.exe")
	inGOBIN := windowsTool(t, gobin, "kindish.exe")
	if got, err := LookPath("kindish"); err != nil || !strings.EqualFold(got, inGOBIN) {
		t.Fatalf("LookPath = %q, %v; want GOBIN tool %q", got, err, inGOBIN)
	}
	onPATH := windowsTool(t, t.TempDir(), "kindish.exe")
	t.Setenv("PATH", filepath.Dir(onPATH))
	if got, err := LookPath("kindish"); err != nil || !strings.EqualFold(got, onPATH) {
		t.Fatalf("LookPath = %q, %v; want PATH tool %q", got, err, onPATH)
	}
}

func TestLookPathWindowsSkipsDirectories(t *testing.T) {
	gobin := windowsToolEnv(t)
	if err := os.Mkdir(filepath.Join(gobin, "kindish.exe"), 0o750); err != nil {
		t.Fatal(err)
	}
	if got, err := LookPath("kindish"); err == nil {
		t.Fatalf("LookPath resolved a directory: %q", got)
	}
}
