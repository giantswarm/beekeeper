package codex

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateModelEffort(t *testing.T) {
	const catalogHigh = "high"
	cache := filepath.Join(t.TempDir(), "models_cache.json")
	if err := ValidateEffort(cache, "test-model", catalogHigh); err == nil {
		t.Fatal("accepted missing model catalog")
	}
	if err := os.WriteFile(cache, []byte(`{"models":[{"slug":"test-model","supported_reasoning_levels":[{"effort":"high"}]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		model, level string
		invalid      bool
	}{
		{"test-model", catalogHigh, false}, {"test-model", "low", true}, {"unknown", catalogHigh, true}, {"unknown", "", false},
	} {
		if err := ValidateEffort(cache, tc.model, tc.level); (err != nil) != tc.invalid {
			t.Errorf("%+v: %v", tc, err)
		}
	}
}
