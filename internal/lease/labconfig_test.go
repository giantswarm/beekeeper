package lease

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// LabDiff names the dotted keys that differ, a list as one value, and
// sees no change in a comment.
func TestLabDiff(t *testing.T) {
	was := "# v1\nclusterName: agentlab\nplatform:\n  chartVersion: 4.1.0\n  valuesFiles: [a.yaml]\n  devImages: {muster: x}\n"
	for now, want := range map[string][]string{
		"# v2\nclusterName: agentlab\nplatform:\n  chartVersion: 4.1.0\n  valuesFiles: [a.yaml]\n  devImages: {muster: x}\n":        nil,
		"clusterName: agentlab\nplatform:\n  chartVersion: 4.2.0-rc.1\n  valuesFiles: [a.yaml, b.yaml]\n  devImages: {muster: x}\n": {"platform.chartVersion", "platform.valuesFiles"},
		"clusterName: agentlab\nplatform:\n  chartVersion: 4.1.0\n  valuesFiles: [a.yaml]\nextra: 1\n":                              {"extra", "platform.devImages"},
		"clusterName: agentlab\nplatform:\n  chartVersion: 4.1.0\n  valuesFiles: [a.yaml]\n  devImages: {muster: y, kagent: z}\n":   {"platform.devImages.kagent", "platform.devImages.muster"},
	} {
		got, err := LabDiff([]byte(was), []byte(now))
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("LabDiff to %q = %v, %v; want %v", now, got, err, want)
		}
	}
}

// ReadLabConfig records the overlays and a hash a comment does not change.
func TestReadLabConfig(t *testing.T) {
	dir := t.TempDir()
	one, two := filepath.Join(dir, "1.yaml"), filepath.Join(dir, "2.yaml")
	_ = os.WriteFile(one, []byte("# v1\nplatform:\n  valuesFiles: [a.yaml, b.yaml]\n"), 0o600)
	_ = os.WriteFile(two, []byte("# v2\nplatform:\n    valuesFiles:\n        - a.yaml\n        - b.yaml\n"), 0o600)
	a, _, err := ReadLabConfig(one)
	b, _, err2 := ReadLabConfig(two)
	if err != nil || err2 != nil || a.SHA256 != b.SHA256 || !slices.Equal(a.ValuesFiles, []string{"a.yaml", "b.yaml"}) {
		t.Errorf("ReadLabConfig: %+v %v, %+v %v", a, err, b, err2)
	}
}

// Resolving a broken link must fail without replacing it or making a
// released copy: the canonical configuration is unavailable.
func TestRestoreLabConfigBrokenSymlink(t *testing.T) {
	dir := Dir(t.TempDir())
	if _, err := dir.Claim("lab", Holder{Env: "lab"}); err != nil {
		t.Fatal(err)
	}
	if err := dir.KeepLabConfig("lab", []byte("clusterName: lab\n")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "agentlab.yaml")
	if err := os.Symlink("missing.yaml", path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := dir.RestoreLabConfig("lab", path); err == nil {
		t.Fatal("restored over a broken link")
	}
	if link, err := os.Readlink(path); err != nil || link != "missing.yaml" {
		t.Fatalf("link = %q, %v", link, err)
	}
	if _, err := os.Lstat(path + ".released"); !os.IsNotExist(err) {
		t.Fatalf("released copy exists: %v", err)
	}
}
