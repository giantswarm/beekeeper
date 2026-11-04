package install

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstallRetainsOwnershipAfterFailedWrite(t *testing.T) {
	const editedFile = "edited file"
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("requires Unix directory permissions for an offline write failure")
	}
	for _, recovery := range []string{"retry", "uninstall", editedFile, "partial retry", "partial uninstall"} {
		t.Run(recovery, func(t *testing.T) {
			f := &fakeSetup{}
			e, _ := env(t, t.TempDir(), f, f.run)
			e.AgyHooks = filepath.Join(e.Spec.Home, ".gemini", "config", "hooks.json")
			e.CodexHooks = filepath.Join(e.Spec.Home, ".codex", "hooks.json")
			if err := Install(context.Background(), e); err != nil {
				t.Fatal(err)
			}
			previousExe := e.Exe
			e.Exe, e.Spec.Exe = replacementExe, replacementExe
			dir := filepath.Dir(e.Settings)
			if strings.HasPrefix(recovery, "partial ") {
				dir = filepath.Dir(CodexWorkerHooks(e.StateDir))
			}
			if err := os.Chmod(dir, 0o500); err != nil { // #nosec G302 -- directory traversal is required to inject only a write failure.
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) // #nosec G302 -- restore owner access to a temporary directory.
			err := Install(context.Background(), e)
			if restore := os.Chmod(dir, 0o700); restore != nil { // #nosec G302 -- restore owner access to a temporary directory.
				t.Fatal(restore)
			}
			if err == nil {
				t.Fatal("write failure was not injected")
			}
			unit := filepath.Join(e.Spec.ConfigDir, "units", "notify.service")
			if recovery == editedFile {
				write(t, unit, "the person's service definition\n")
			}
			if recovery == "retry" || recovery == "partial retry" {
				if err := Install(context.Background(), e); err != nil {
					t.Fatal(err)
				}
				m, err := readManifest(e.manifest())
				if err != nil {
					t.Fatal(err)
				}
				if m.hasPrevious() {
					t.Error("successful recovery did not clear previous ownership")
				}
				for _, path := range []string{e.Settings, e.AgyHooks, e.CodexHooks, CodexWorkerHooks(e.StateDir), filepath.Join(e.Spec.ConfigDir, "units", "notify.service")} {
					raw, err := os.ReadFile(filepath.Clean(path))
					if err != nil || strings.Contains(string(raw), previousExe) || !strings.Contains(string(raw), e.Exe) {
						t.Errorf("retry lost ownership of %s: %v", path, err)
					}
				}
			}
			if err := Uninstall(context.Background(), e, false); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{e.Settings, e.AgyHooks, e.CodexHooks, CodexWorkerHooks(e.StateDir), filepath.Join(e.Spec.ConfigDir, "units", "notify.service")} {
				if recovery == editedFile && path == unit {
					raw, err := os.ReadFile(filepath.Clean(path))
					if err != nil || string(raw) != "the person's service definition\n" {
						t.Error("recovery removed a file edited after the failure")
					}
				} else if exists(path) {
					t.Errorf("uninstall lost ownership of %s", path)
				}
			}
		})
	}
}
