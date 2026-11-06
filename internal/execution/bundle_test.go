package execution

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func bundleFixture(t *testing.T) (string, []BundleSource) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sources := []BundleSource{{"AGENTS.md", "rules/project/AGENTS.md"}, {"SKILL.md", "skills/review/SKILL.md"}}
	for _, s := range sources {
		if err := os.WriteFile(filepath.Join(root, s.Source), []byte("Follow the reviewed process."), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root, sources
}
func TestCoordinatorBundleReproducibleChanges(t *testing.T) {
	root, src := bundleFixture(t)
	build := func() CoordinatorBundle {
		t.Helper()
		b, err := BuildCoordinatorBundle(root, src, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	b := build()
	src[0], src[1] = src[1], src[0]
	if b.Digest() != build().Digest() {
		t.Fatal("order affects digest")
	}
	for _, s := range src {
		old := build().Digest()
		if err := os.WriteFile(filepath.Join(root, s.Source), []byte("Changed reviewed process."), 0600); err != nil {
			t.Fatal(err)
		}
		if old == build().Digest() {
			t.Fatal("content change absent from manifest")
		}
	}
	changed := build()
	changed.Settings = map[string]string{"model_reasoning_effort": "high"}
	if changed.Digest() == build().Digest() {
		t.Fatal("settings absent from manifest")
	}
}
func TestCoordinatorBundleRefusals(t *testing.T) {
	root, src := bundleFixture(t)
	good, err := BuildCoordinatorBundle(root, src, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*CoordinatorBundle)
	}{
		{"schema", func(b *CoordinatorBundle) { b.Schema++ }},
		{"size", func(b *CoordinatorBundle) {
			b.Artifacts[0].Content = make([]byte, maxArtifactBytes+1)
			b.Artifacts[0].Digest = Digest(b.Artifacts[0].Content)
		}},
		{missingOperation, func(b *CoordinatorBundle) { b.Artifacts[0].Content = nil }},
		{"digest", func(b *CoordinatorBundle) { b.Artifacts[0].Digest = "invalid-bundle-digest" }},
		{"traversal", func(b *CoordinatorBundle) { b.Artifacts[0].Artifact = "../escape" }},
		{"skill entry", func(b *CoordinatorBundle) { b.Artifacts[1].Artifact = "skills/review/other.md" }},
		{"credential", func(b *CoordinatorBundle) {
			b.Artifacts[0].Content = []byte(strings.Join([]string{"-----BEGIN", "PRIVATE", "KEY-----"}, " "))
			b.Artifacts[0].Digest = Digest(b.Artifacts[0].Content)
		}},
		{"setting", func(b *CoordinatorBundle) { b.Settings = map[string]string{"hooks": "false"} }},
		{"plugin", func(b *CoordinatorBundle) {
			b.Capabilities = []BundleCapability{{Name: "plugin:required", Required: true}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b CoordinatorBundle
			if err := Decode(mustJSON(good), &b); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&b)
			if b.Validate() == nil {
				t.Fatal("accepted invalid bundle")
			}
		})
	}
	if err := os.Remove(filepath.Join(root, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildCoordinatorBundle(root, src, nil, nil, nil); err == nil {
		t.Fatal("accepted missing source")
	}
	if err := os.Symlink(filepath.Join(root, "AGENTS.md"), filepath.Join(root, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildCoordinatorBundle(root, src, nil, nil, nil); err == nil {
		t.Fatal("accepted symlink")
	}
	if _, err := BuildCoordinatorBundle(root, src[:1], nil, nil, []string{filepath.Join(root, "AGENTS.md")}); err == nil {
		t.Fatal("accepted excluded source")
	}
}
