package guard

import (
	"strings"
	"testing"
)

func TestProductionNoteCommandBoundary(t *testing.T) {
	command := "kubectl --context production delete job stale --namespace monitoring"
	for _, suffix := range []string{"; kubectl --context production delete job other --namespace monitoring", " --all", " > output", "\ntrue", " $(echo other)"} {
		called := false
		h := Hook{Production: "production", ProductionNote: func(int, string) error { called = true; return nil }}
		if r := h.kubeRefusal("BEEKEEPER_PROD_NOTE=42 " + command + suffix); r == "" || called {
			t.Fatalf("unsafe suffix %q passed: %s", suffix, r)
		}
	}
	h := Hook{Production: "production"}
	if r := h.kubeRefusal(command); !strings.Contains(r, "a production cluster") {
		t.Fatal(r)
	}
	if r := h.kubeRefusal("BEEKEEPER_PROD_NOTE=42 " + command); !strings.Contains(r, "unavailable") {
		t.Fatal(r)
	}
}
