//go:build scenario

package scenario

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Mutation is a historical defect put back into the candidate's source for
// one build: Before, found exactly once in File (relative to the
// repository), is replaced with After through a build overlay, the source
// tree untouched. A scenario that must catch the defect runs against such
// a build (a mutation control, controls_linux_test.go), and a recovery
// scenario installs one as the broken candidate.
type Mutation struct {
	File, Before, After string
}

// Mutations are the defects a build can carry, by name.
var Mutations = map[string]Mutation{
	// The pull parser dropped the base branch: a reviewed pull request's
	// merge must refuse before any merge. internal/loopworkflow built it
	// for TestHistoricalParserRegression and its recovery tests.
	"pull-parser-drops-the-base": {File: "cmd/supervisorloop.go", Before: "Base: j.Base,", After: `Base: "",`},
}

// MutationNames are the names of Mutations, sorted.
func MutationNames() []string {
	var out []string
	for name := range Mutations {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// Overlay writes the build overlay of m for the repository at repo into
// dir and returns the overlay file, for go build -overlay.
func (m Mutation) Overlay(repo, dir string) (string, error) {
	repo, err := filepath.Abs(repo)
	if err != nil {
		return "", err
	}
	source := filepath.Join(repo, m.File)
	original, err := os.ReadFile(source) //nolint:gosec // a source file of this repository
	if err != nil {
		return "", err
	}
	if n := strings.Count(string(original), m.Before); n != 1 {
		return "", fmt.Errorf("%s: the mutation's text %q is there %d times, not once", m.File, m.Before, n)
	}
	replacement := filepath.Join(dir, "mutated-"+filepath.Base(m.File))
	if err := os.WriteFile(replacement, []byte(strings.Replace(string(original), m.Before, m.After, 1)), 0o600); err != nil {
		return "", err
	}
	raw, err := json.Marshal(map[string]any{"Replace": map[string]string{source: replacement}})
	if err != nil {
		return "", err
	}
	overlay := filepath.Join(dir, "overlay.json")
	return overlay, os.WriteFile(overlay, raw, 0o600)
}
