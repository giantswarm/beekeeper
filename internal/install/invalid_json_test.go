package install

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/giantswarm/beekeeper/internal/agyhooks"
	"github.com/giantswarm/beekeeper/internal/platform"
)

func TestAgyRejectsIncompleteJSON(t *testing.T) {
	for _, suffix := range []string{"", "} trailing", "} {}"} {
		t.Run(suffix, func(t *testing.T) {
			e, _ := env(t, t.TempDir(), platform.Stub().Setup, nil)
			e.AgyHooks = filepath.Join(e.Spec.Home, ".gemini", "config", "hooks.json")
			raw := `{"beekeeper":` + string(AgyHook(e.Exe)) + suffix
			if agyhooks.Rejects([]byte(raw)) == "" {
				t.Fatal("fixture must be rejected by agy")
			}
			write(t, e.AgyHooks, raw)
			if got := AgyHooksUnguarded(e.AgyHooks); got == "" {
				t.Error("start admission accepted invalid hooks JSON")
			}
			if got := CheckAgyHooks(e.AgyHooks, e.Exe, e.Spec.Home); got == "" {
				t.Error("doctor accepted invalid hooks JSON")
			}
			if err := Install(context.Background(), e); err == nil {
				t.Error("install accepted invalid hooks JSON")
			}
			got, err := os.ReadFile(e.AgyHooks)
			if err != nil || string(got) != raw {
				t.Error("install rewrote invalid settings")
			}
		})
	}
}
