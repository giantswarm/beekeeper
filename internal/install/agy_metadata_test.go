package install

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgyMetadataPreservesGuardState(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata string
		guarded  bool
	}{
		{"case 0: enabled guard", `"enabled":true,`, true},
		{"case 1: disabled guard", `"enabled":false,`, false},
		{"case 2: default enabled guard", `"enabled":null,`, true},
		{"case 3: ignored field", `"FutureEvent":17,`, true},
		{"case 4: disabled case variant", `"Enabled":false,`, false},
		{"case 5: disabled then null", `"enabled":false,"enabled":null,`, false},
		{"case 6: later case variant disables", `"enabled":true,"ENABLED":false,`, false},
		{"case 7: later case variant enables", `"enabled":false,"Enabled":true,`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, "hooks.json")
			const exe = "/opt/bin/beekeeper"
			entry := `{` + tc.metadata + strings.TrimPrefix(string(AgyHook(exe)), "{")
			write(t, path, `{"beekeeper":`+entry+`}`)
			if why := AgyHooksUnguarded(path); (why == "") != tc.guarded {
				t.Errorf("AgyHooksUnguarded = %q, want guarded %v", why, tc.guarded)
			}
			if why := CheckAgyHooks(path, exe, home); (why == "") != tc.guarded {
				t.Errorf("CheckAgyHooks = %q, want guarded %v", why, tc.guarded)
			}
		})
	}
}

func TestAgyMetadataInstallPreservesOtherDefinition(t *testing.T) {
	home := t.TempDir()
	f := &fakeSetup{}
	e, _ := env(t, home, f, f.run)
	e.AgyHooks = filepath.Join(home, ".gemini", "config", "hooks.json")
	const other = `{"enabled":false,"FutureEvent":17,"PreInvocation":[{"command":"/usr/bin/true"}]}`
	write(t, e.AgyHooks, `{"other":`+other+`}`)
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	s, err := readSettings(e.AgyHooks)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.top.get("other"); !sameJSON(got, []byte(other)) {
		t.Errorf("install changed the other definition: %s", got)
	}
	if why := AgyHooksUnguarded(e.AgyHooks); why != "" {
		t.Fatalf("installed guard is unavailable: %s", why)
	}
}

func TestAgyMetadataGuardRequiresKnownEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	entry := strings.Replace(string(AgyHook("/opt/bin/beekeeper")), `"PreToolUse"`, `"pretooluse"`, 1)
	write(t, path, `{"beekeeper":`+entry+`}`)
	if why := AgyHooksUnguarded(path); why == "" {
		t.Fatal("a noncanonical event cannot supply the required guard")
	}
}
