package install

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/platform"
)

func TestAgyInstallKeepsUnrelatedWorkerRulesEntry(t *testing.T) {
	e, _ := env(t, t.TempDir(), platform.Stub().Setup, nil)
	e.AgySkills = filepath.Join(e.Spec.Home, ".gemini", "config", "skills.json")
	customDir := filepath.Join(e.Spec.Home, "custom-skills")
	write(t, filepath.Join(customDir, "worker-rules", "SKILL.md"), "user's worker rules\n")
	entry := `{"path":"` + customDir + `","exclude":["experimental-.*"]}`
	write(t, e.AgySkills, `{"entries":[`+entry+`]}`)
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	s, err := readSettings(e.AgySkills)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := s.top.get("entries")
	if !strings.Contains(string(raw), customDir) || !strings.Contains(string(raw), "experimental-.*") {
		t.Error("install replaced an unrelated skills collection because it contained worker-rules")
	}
}
