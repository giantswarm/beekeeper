package policycontent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRulesAllowedPreservesExcludedFileAliases(t *testing.T) {
	root := t.TempDir()
	excluded := filepath.Join(root, "excluded.json")
	if err := os.WriteFile(excluded, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias.md")
	if err := os.Link(excluded, alias); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.md")
	if err := os.Symlink(excluded, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{excluded, alias, link} {
		if RulesAllowed(path, []string{filepath.Join(root, "*.json")}) {
			t.Fatalf("allowed an excluded file alias: %s", path)
		}
	}
	ordinary := filepath.Join(root, "ordinary.md")
	if err := os.WriteFile(ordinary, []byte("rules"), 0600); err != nil {
		t.Fatal(err)
	}
	if !RulesAllowed(ordinary, []string{excluded}) {
		t.Fatal("refused an unrelated rules file")
	}
}
