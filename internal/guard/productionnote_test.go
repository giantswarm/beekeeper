package guard

import (
	"strings"
	"testing"
)

const noteProduction = "production"

func TestProductionNoteCommandBoundary(t *testing.T) {
	command := "kubectl --context production delete job stale --namespace monitoring"
	for _, suffix := range []string{"; kubectl --context production delete job other --namespace monitoring", " --all", " > output", "\ntrue", " $(echo other)"} {
		called := false
		h := Hook{Production: noteProduction, ProductionNote: func(int, string) error { called = true; return nil }}
		if r := h.kubeRefusal("BEEKEEPER_PROD_NOTE=42 " + command + suffix); r == "" || called {
			t.Fatalf("unsafe suffix %q passed: %s", suffix, r)
		}
	}
	h := Hook{Production: noteProduction}
	if r := h.kubeRefusal(command); !strings.Contains(r, "a production cluster") {
		t.Fatal(r)
	}
	// Off, the marker means nothing: the deletion meets the ordinary refusal and
	// a command that only names the marker passes.
	if r := h.kubeRefusal("BEEKEEPER_PROD_NOTE=42 " + command); !strings.Contains(r, "a production cluster") {
		t.Fatal(r)
	}
	if r := h.kubeRefusal("grep BEEKEEPER_PROD_NOTE README.md"); r != "" {
		t.Fatal(r)
	}
}

// A note covers the production installation, not a context that
// kube.protectedContexts protects, whether or not it is production too: that
// write stays refused and spends no note.
func TestProductionNoteLeavesProtectedContextsRefused(t *testing.T) {
	for _, tc := range []struct{ production, context string }{
		{"", "protected"},
		{noteProduction, "protected"},
		{noteProduction, noteProduction},
	} {
		called := false
		h := Hook{Production: tc.production, ProtectedContexts: []string{tc.context}, ProductionNote: func(int, string) error { called = true; return nil }}
		command := "BEEKEEPER_PROD_NOTE=42 kubectl --context " + tc.context + " delete job stale --namespace monitoring"
		if r := h.kubeRefusal(command); !strings.Contains(r, "explicitly targeted production write") || called {
			t.Fatalf("%+v: refusal %q, note spent %v", tc, r, called)
		}
	}
}

// An argument that runs later is checked, and a note never covers it: nobody is
// there when it runs. Text nobody runs may name the marker, note on or off.
func TestProductionNoteCoversNoLaterCommand(t *testing.T) {
	marked := "BEEKEEPER_PROD_NOTE=42 kubectl --context production delete job stale --namespace monitoring"
	called := false
	h := Hook{Production: noteProduction, ProductionNote: func(int, string) error { called = true; return nil }}
	if r := h.kubeRefusal(`beekeeper timer add 1m "later" --probe '` + marked + `'`); !strings.Contains(r, "a production cluster") || called {
		t.Fatalf("refusal %q, note spent %v", r, called)
	}
	if r := h.kubeRefusal(`beekeeper note add "does BEEKEEPER_PROD_NOTE work?"`); r != "" || called {
		t.Fatalf("refusal %q, note spent %v", r, called)
	}
}
