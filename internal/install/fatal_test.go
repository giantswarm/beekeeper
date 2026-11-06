package install

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestFatalClaudeGuardCheck(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "settings.json")
	exe := "/opt/bin/beekeeper"
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
