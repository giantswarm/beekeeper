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
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("requires Unix directory permissions for an offline write failure")
	}
	for _, recovery := range []string{"retry", "uninstall", "edited file"} {
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
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o750) })
			err := Install(context.Background(), e)
			if restore := os.Chmod(dir, 0o750); restore != nil {
				t.Fatal(restore)
			}
			if err == nil {
				t.Fatal("write failure was not injected")
			}
			unit := filepath.Join(e.Spec.ConfigDir, "units", "notify.service")
			if recovery == "edited file" {
				write(t, unit, "the person's service definition\n")
			}
			if recovery == "retry" {
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
					raw, err := os.ReadFile(path)
					if err != nil || strings.Contains(string(raw), previousExe) || !strings.Contains(string(raw), e.Exe) {
						t.Errorf("retry lost ownership of %s: %v", path, err)
					}
				}
			}
			if err := Uninstall(context.Background(), e, false); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{e.Settings, e.AgyHooks, e.CodexHooks, CodexWorkerHooks(e.StateDir), filepath.Join(e.Spec.ConfigDir, "units", "notify.service")} {
				if recovery == "edited file" && path == unit {
					raw, err := os.ReadFile(path)
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
