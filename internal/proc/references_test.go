package proc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	fixtureEnvironment = "environ"
	fixtureMappings    = "maps"
)

func TestReferencesReadsOpenFilesMappingsAndBuildDirectories(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "42")
	if err := os.MkdirAll(filepath.Join(dir, "fd"), 0700); err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(root, "work"), filepath.Join(root, "output"), filepath.Join(root, "mapped"), filepath.Join(root, "tmp")}
	if err := os.Symlink(paths[0], filepath.Join(dir, "cwd")); err != nil {
		t.Skip(err)
	}
	if err := os.Symlink(paths[1]+" (deleted)", filepath.Join(dir, "fd", "3")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fixtureMappings), []byte("00-10 r--p 0 0:0 1 "+paths[2]+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fixtureEnvironment), []byte("GOTMPDIR="+paths[3]+"\x00UNRELATED=value\x00"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := referencesAt(root, 42)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if !slices.Contains(got, p) {
			t.Errorf("missing reference %s", filepath.Base(p))
		}
	}
	// A file masquerading as /proc/<pid>/fd is not an empty set of files.
	if err := os.Remove(filepath.Join(dir, "fd", "3")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "fd")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fd"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := referencesAt(root, 42); err == nil || strings.TrimSpace(err.Error()) == "" {
		t.Fatal("unreadable references looked unused")
	}
}

func TestUserReferencesDistinguishesProtectedHelpersFromUnreadableProcesses(t *testing.T) {
	for _, tc := range []struct {
		name, status, denied string
		wantErr              bool
	}{
		{"non-dumpable helper", "Dumpable: 0\n", fixtureEnvironment, false},
		{"credential-changing helper", "setuid", fixtureMappings, false},
		{"kernel-owned helper", "kernel-owned", fixtureEnvironment, false},
		{"dumpable environment", "", fixtureEnvironment, true},
		{"dumpable mappings", "", fixtureMappings, true},
		{"dumpable descriptors", "", "fd", true},
		{"dumpable cwd", "", "cwd", true},
		{"dumpable descriptor target", "", "3", true},
		{"unknown ownership", "", "status", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "42")
			if err := os.MkdirAll(filepath.Join(dir, "fd"), 0700); err != nil {
				t.Fatal(err)
			}
			uid := os.Getuid()
			if tc.status == "kernel-owned" {
				uid++
			}
			status := fmt.Sprintf("Uid: %d %d %d %d\n", uid, uid, uid, uid) + tc.status
			if tc.status == "setuid" {
				status = fmt.Sprintf("Uid: %d %d %d %d\n", uid, uid+1, uid, uid)
			}
			for name, data := range map[string]string{"status": status, "comm": "go", fixtureEnvironment: "", fixtureMappings: "", "fd/3": ""} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			files := referenceFiles()
			files.readFile = func(p string) ([]byte, error) {
				if filepath.Base(p) == tc.denied {
					return nil, os.ErrPermission
				}
				return os.ReadFile(filepath.Clean(p))
			}
			files.readDir = func(p string) ([]os.DirEntry, error) {
				if filepath.Base(p) == tc.denied {
					return nil, os.ErrPermission
				}
				return os.ReadDir(p)
			}
			files.readlink = func(p string) (string, error) {
				if filepath.Base(p) == tc.denied {
					return "", os.ErrPermission
				}
				return "/scratch/output", nil
			}
			refs, commands, err := userReferencesAt(root, uid, files)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v, want error=%v", err, tc.wantErr)
			}
			if !tc.wantErr && (!slices.Contains(commands, "go") || len(refs) == 0) {
				t.Fatal("lost readable helper references or build command")
			}
			if err != nil && strings.Contains(err.Error(), "process references unavailable:") {
				t.Fatal("duplicate error wrapper")
			}
		})
	}
}

func TestUserReferencesFailsClosedWithoutProcessRootOrOwnership(t *testing.T) {
	root := t.TempDir()
	if _, _, err := userReferencesAt(filepath.Join(root, "missing"), os.Getuid(), referenceFiles()); err == nil {
		t.Fatal("missing process root looked idle")
	}
	if _, _, err := referenceOwner([]byte("Name: helper\n")); err == nil {
		t.Fatal("missing Uid accepted")
	}
	files := referenceFiles()
	files.readDir = func(string) ([]os.DirEntry, error) { return nil, os.ErrPermission }
	if _, _, err := userReferencesAt(root, os.Getuid(), files); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("root error=%v", err)
	}
	files = referenceFiles()
	files.readlink = func(string) (string, error) { return "", errors.New("link read failed") }
	if _, err := referencesWith(root, 42, true, files); err == nil {
		t.Fatal("helper hid an unexpected readlink failure")
	}
}
