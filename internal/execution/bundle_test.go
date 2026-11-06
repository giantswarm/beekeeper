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
			b.Artifacts[0].Content = []byte("-----BEGIN PRIVATE KEY-----")
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
func TestCoordinatorBundlePreparePreservesHost(t *testing.T) {
	f := testFixture(t)
	root, src := bundleFixture(t)
	f.s.Config.Harnesses[HarnessCodex] = HarnessConfig{Executable: "/usr/bin/codex", Version: "0.165.0", ConfigDir: t.TempDir()}
	b, err := BuildCoordinatorBundle(root, src, map[string]string{"model_reasoning_effort": "high"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	pinned := f.s.Policy()
	originalRules := string(f.s.Rules)
	originalConfig := string(mustJSON(f.s.Config))
	unrelated := filepath.Join(f.s.Root, "operator.txt")
	if err := os.WriteFile(unrelated, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetCoordinatorBundle(b); err != nil {
		t.Fatal(err)
	}
	b.Artifacts[0].Content[0] = 'X' // Caller mutation must not change the snapshot.
	if _, err := f.s.Prepare(pinned); err == nil {
		t.Fatal("accepted old trust pin")
	}
	if _, err := f.s.Prepare(f.s.Policy()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.InstalledPolicy(); err != nil {
		t.Fatal(err)
	}
	if string(f.s.Rules) != originalRules || string(mustJSON(f.s.Config)) != originalConfig {
		t.Fatal("host policy mutated")
	}
	data, err := os.ReadFile(filepath.Clean(unrelated))
	if err != nil || string(data) != "keep" {
		t.Fatal("unrelated file changed", err)
	}
	rules, err := os.ReadFile(filepath.Join(f.s.codexHomeDir(), "AGENTS.md"))
	if err != nil || !strings.HasPrefix(string(rules), originalRules) || !strings.Contains(string(rules), "Follow the reviewed process.") {
		t.Fatal("rules not materialized", err)
	}
	if _, err := os.Stat(filepath.Join(f.s.codexHomeDir(), "skills/review/SKILL.md")); err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(filepath.Join(f.s.codexHomeDir(), "config.toml"))
	if err != nil || !strings.Contains(string(config), `model_reasoning_effort = "high"`) || !strings.Contains(string(config), "hooks = true") || !strings.Contains(string(config), "plugins = false") {
		t.Fatal("supported settings or security features missing", err)
	}
	hooks, err := os.ReadFile(filepath.Join(f.s.codexHomeDir(), hooksJSONFile))
	if err != nil || !strings.Contains(string(hooks), "execution-guard") {
		t.Fatal("host guard missing", err)
	}
	want := f.s.Policy()
	f.s.coordinatorBundle = nil
	if err := f.s.LoadCoordinatorBundle(); err != nil {
		t.Fatal(err)
	}
	if f.s.Policy() != want {
		t.Fatal("restored policy differs")
	}
	if err := os.Remove(filepath.Join(f.s.codexHomeDir(), "skills/review/SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.InstalledPolicy(); err == nil {
		t.Fatal("accepted missing installed artifact")
	}
}

func TestCoordinatorBundleInstallRejectsSymlinkParent(t *testing.T) {
	f := testFixture(t)
	root, src := bundleFixture(t)
	f.s.Config.Harnesses[HarnessCodex] = HarnessConfig{Executable: "/usr/bin/codex", Version: "0.165.0", ConfigDir: t.TempDir()}
	b, err := BuildCoordinatorBundle(root, src, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetCoordinatorBundle(b); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.s.codexHomeDir(), 0700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(f.s.codexHomeDir(), "skills")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Prepare(f.s.Policy()); err == nil {
		t.Fatal("installed through symlink")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("wrote outside managed home", err)
	}
}
