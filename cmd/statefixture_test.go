package cmd

import (
	"fmt"
	"os"
	"path/filepath"
)

// Generated fixture briefs must stay inside their private state directory,
// including when a tested command accidentally supplies an escaping path.
func readStateFixture(dir, path string) ([]byte, error) {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return nil, err
	}
	if !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("fixture path escapes state directory")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return root.ReadFile(rel)
}
