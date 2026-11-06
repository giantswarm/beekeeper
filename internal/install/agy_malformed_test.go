package install

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestAgyMalformedSiblingBlocksWorker(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"case 0: string timeout", `{"PreInvocation":[{"command":"/usr/bin/true","timeout":"30"}]}`},
		{"case 1: unsupported type", `{"Stop":[{"type":"http","url":"https://example.invalid/hook"}]}`},
		{"case 2: numeric matcher", `{"PreToolUse":[{"matcher":17,"hooks":[{"command":"/usr/bin/true"}]}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, "hooks.json")
			const exe = "/opt/bin/beekeeper"
			write(t, path, `{"beekeeper":`+string(AgyHook(exe))+`,"other":`+tc.raw+`}`)
			for _, why := range []string{AgyHooksUnguarded(path), CheckAgyHooks(path, exe, home)} {
				if !strings.Contains(why, "agy rejects the other entry") {
					t.Errorf("a malformed sibling must refuse a worker and name the entry: %q", why)
				}
			}
		})
	}
}
