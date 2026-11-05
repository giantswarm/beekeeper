package install

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	"github.com/giantswarm/beekeeper/internal/platform"
)

const hookMatchExe = "/opt/desk tools/beekeeper"

const hookMatchQuotedExe = "/opt/desk's tools/beekeeper"

func TestClaudeInstallsGuardWithQuotedOrRenamedBinary(t *testing.T) {
	for _, exe := range []string{hookMatchExe, hookMatchQuotedExe, "/opt/bin/bk"} {
		t.Run(exe, func(t *testing.T) {
			e, _ := env(t, t.TempDir(), platform.Stub().Setup, nil)
			e.Exe, e.Spec.Exe = exe, exe
			write(t, e.Settings, priorSettings)
			if err := Install(context.Background(), e); err != nil {
				t.Fatal(err)
			}
			s, err := readSettings(e.Settings)
			if err != nil {
				t.Fatal(err)
			}
			list, err := s.hooks("PreToolUse")
			if err != nil {
				t.Fatal(err)
			}
			want := Hooks(exe)[0].Entry
			if len(list) != 2 || !slices.ContainsFunc(list, func(raw json.RawMessage) bool { return sameJSON(raw, want) }) {
				t.Errorf("install omitted the Claude guard for %s alongside an unrelated hook", filepath.Base(exe))
			}
		})
	}
}

func TestClaudePreservesOtherQuotedGuard(t *testing.T) {
	e, _ := env(t, t.TempDir(), platform.Stub().Setup, nil)
	other := Hooks("/opt/other tools/beekeeper")[0].Entry
	write(t, e.Settings, `{"hooks":{"PreToolUse":[`+string(other)+`]}}`)
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	s, err := readSettings(e.Settings)
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.hooks("PreToolUse")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || !sameJSON(list[0], other) {
		t.Error("install duplicated another binary's quoted guard")
	}
}
