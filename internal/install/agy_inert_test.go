package install

import (
	"path/filepath"
	"testing"
)

func TestAgyInertDefinitionsPreserveRealGuard(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"case 0: empty matcher", `{"PreToolUse":[{"matcher":"*","hooks":[]}]}`},
		{"case 1: null definition", `null`},
		{"case 2: whitespace command", `{"Stop":[{"command":"   "}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, "hooks.json")
			const exe = "/opt/bin/beekeeper"
			write(t, path, `{"other":`+tc.raw+`,"beekeeper":`+string(AgyHook(exe))+`}`)
			if why := AgyHooksUnguarded(path); why != "" {
				t.Errorf("inert sibling blocks a real guard: %s", why)
			}
			if why := CheckAgyHooks(path, exe, home); why != "" {
				t.Errorf("doctor reports a valid file as broken: %s", why)
			}
			write(t, path, `{"beekeeper":`+tc.raw+`}`)
			if why := AgyHooksUnguarded(path); why == "" {
				t.Error("inert definition cannot supply the guard")
			}
			if why := CheckAgyHooks(path, exe, home); why == "" {
				t.Error("doctor must still require the guard")
			}
		})
	}
}
