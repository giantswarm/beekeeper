package gocache

import (
	"os"
	"path/filepath"
	"testing"
)

const goSettingsOff = "settings off"

func TestCacheDirectoriesUseGoEnvSettings(t *testing.T) {
	dir := t.TempDir()
	build := filepath.Join(dir, "build")
	modules := filepath.Join(dir, "modules")
	settings := filepath.Join(dir, "go.env")
	if err := os.WriteFile(settings, []byte("GOCACHE="+build+"\nGOMODCACHE="+modules+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOENV", settings)
	t.Setenv("GOCACHE", "")
	t.Setenv("GOMODCACHE", "")
	if got := Dir(); got != build {
		t.Error("Dir ignored the build cache configured in the synthetic Go settings file")
	}
	if got := ModDir(); got != modules {
		t.Error("ModDir ignored the module cache configured in the synthetic Go settings file")
	}
}

func TestCacheDirectoriesSettingsPrecedence(t *testing.T) {
	const missingSettings = "missing settings"
	for _, name := range []string{"process overrides", "persisted GOPATH", "cache off", goSettingsOff, missingSettings} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("GOCACHE", "")
			t.Setenv("GOMODCACHE", "")
			t.Setenv("GOPATH", "")
			// Isolate the OS defaults too, so no real Go settings are read.
			t.Setenv("HOME", filepath.Join(dir, "home"))
			t.Setenv("USERPROFILE", filepath.Join(dir, "home"))
			t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
			t.Setenv("LocalAppData", filepath.Join(dir, "cache"))
			settings := filepath.Join(dir, "go.env")
			t.Setenv("GOENV", settings)
			build, modules := filepath.Join(dir, "build"), filepath.Join(dir, "modules")
			content := "GOCACHE=" + build + "\nGOMODCACHE=" + modules + "\n"
			switch name {
			case "process overrides":
				build, modules = filepath.Join(dir, "override build"), filepath.Join(dir, "override modules")
				t.Setenv("GOCACHE", build)
				t.Setenv("GOMODCACHE", modules)
			case "persisted GOPATH":
				path := filepath.Join(dir, "first")
				content = "GOCACHE=" + build + "\nGOPATH=" + path + string(os.PathListSeparator) + filepath.Join(dir, "second") + "\n"
				modules = filepath.Join(path, "pkg", "mod")
			case "cache off":
				content = "GOCACHE=off\nGOMODCACHE=" + modules + "\n"
				build = ""
			case goSettingsOff, missingSettings:
				cache, err := os.UserCacheDir()
				if err != nil {
					t.Fatal(err)
				}
				build = filepath.Join(cache, "go-build")
				modules = filepath.Join(dir, "home", "go", "pkg", "mod")
				if name == goSettingsOff {
					t.Setenv("GOENV", "off")
				}
			}
			if name != missingSettings {
				if err := os.WriteFile(settings, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got := Dir(); got != build {
				t.Errorf("Dir = %q, want %q", got, build)
			}
			if got := ModDir(); got != modules {
				t.Errorf("ModDir = %q, want %q", got, modules)
			}
		})
	}
}

func TestCacheDirectoriesDefaultSettingsFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOENV", "")
	t.Setenv("GOCACHE", "")
	t.Setenv("GOMODCACHE", "")
	t.Setenv("HOME", filepath.Join(dir, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("AppData", filepath.Join(dir, "config"))
	config, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(config, "go", "env")
	if err := os.MkdirAll(filepath.Dir(settings), 0o750); err != nil {
		t.Fatal(err)
	}
	build, modules := filepath.Join(dir, "build with spaces"), filepath.Join(dir, "modules=cache")
	// cmd/go ignores malformed lines and uses the last value for a key.
	content := "# GOCACHE=ignored\ninvalid\nGOCACHE=old\nGOCACHE=" + build + "\nGOMODCACHE=" + modules
	if err := os.WriteFile(settings, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Dir(); got != build {
		t.Errorf("Dir = %q, want %q", got, build)
	}
	if got := ModDir(); got != modules {
		t.Errorf("ModDir = %q, want %q", got, modules)
	}
}
