package install

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/platform"
)

func TestCodexRepairsQuotedHook(t *testing.T) {
	for _, exe := range []string{"/opt/desk tools/beekeeper", "/opt/desk's tools/beekeeper"} {
		t.Run(exe, func(t *testing.T) {
			e, _ := env(t, t.TempDir(), platform.Stub().Setup, nil)
			e.Exe, e.Spec.Exe = exe, exe
			e.CodexHooks = filepath.Join(e.Spec.Home, ".codex", "hooks.json")
			narrowed := strings.Replace(string(CodexHook(exe)), `"matcher":".*"`, `"matcher":"Bash"`, 1)
			write(t, e.CodexHooks, `{"hooks":{"PreToolUse":[`+narrowed+`]}}`)
			if err := Install(context.Background(), e); err != nil {
				t.Fatal(err)
			}
			s, err := readSettings(e.CodexHooks)
			if err != nil {
				t.Fatal(err)
			}
			list, err := s.hooks("PreToolUse")
			if err != nil {
				t.Fatal(err)
			}
			if len(list) != 1 || !sameJSON(list[0], CodexHook(exe)) {
				t.Errorf("repair added a second guard instead of updating it: %s", list)
			}
			if got := CheckCodexHooks(e.CodexHooks, "", e.Spec.Home); got != "" {
				t.Error(got)
			}
		})
	}
}
