package install

import (
	"context"
	"encoding/json"
	"github.com/giantswarm/beekeeper/internal/platform"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFatalClaudeGuardCheck(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "settings.json")
	const exe = "/opt/bin/beekeeper"
	hooks := map[string][]json.RawMessage{}
	for _, h := range Hooks(exe) {
		hooks[h.Event] = append(hooks[h.Event], h.Entry)
	}
	raw, err := json.Marshal(map[string]any{"hooks": hooks})
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, string(raw))
	if got := CheckClaudeHooks(path, exe, home); got != "" {
		t.Fatal(got)
	}
	for _, bad := range []string{`{"hooks":{}}`, `{"hooks":`, `{"hooks":{"PreToolUse":"invalid"}}`} {
		write(t, path, bad)
		if got := CheckClaudeHooks(path, exe, home); got == "" {
			t.Fatal("invalid guard accepted")
		}
	}
}

type fatalOnlySetup struct{ *fakeSetup }

func (f fatalOnlySetup) Files(s platform.SetupSpec) ([]platform.File, []string) {
	files, skipped := f.fakeSetup.Files(s)
	for _, name := range []string{"beekeeper-fatal.service", "beekeeper-fatal.timer", "beekeeper-timers.service"} {
		files = append(files, platform.File{Path: filepath.Join(s.ConfigDir, "systemd", "user", name), Content: []byte("# independent recovery unit\n"), Service: !strings.HasSuffix(name, "fatal.service")})
	}
	return files, skipped
}

func TestFatalRecoveryOnlyInstallLeavesDisabledComponentsAlone(t *testing.T) {
	f := &fakeSetup{}
	e, _ := env(t, t.TempDir(), fatalOnlySetup{f}, f.run)
	e.RecoveryOnly = true
	e.AgyHooks = filepath.Join(e.Spec.Home, "agy-hooks.json")
	// Broken harness settings must neither block the recovery installation nor
	// be replaced as a side effect of enabling the independent services.
	for _, path := range []string{e.Settings, e.AgyHooks} {
		write(t, path, "{")
	}
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{e.Settings, e.AgyHooks} {
		b, err := os.ReadFile(path)
		if err != nil || string(b) != "{" {
			t.Fatal("touched deliberately disabled hooks", path, err)
		}
	}
	for _, command := range f.ran {
		if strings.HasPrefix(command, start+" ") && !strings.Contains(command, "beekeeper-fatal.timer") && !strings.Contains(command, "beekeeper-timers.service") {
			t.Fatal("started another component", command)
		}
	}
	if len(f.ran) != 3 {
		t.Fatalf("expected reload and two recovery starts: %v", f.ran)
	}
}
