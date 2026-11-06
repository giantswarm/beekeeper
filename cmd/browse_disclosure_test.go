package cmd

import (
	"strings"
	"testing"
)

// A browse turn gets the same UI-only description that the hook accepts,
// before it attempts its first click on a permissions section.
func TestBrowseDisclosureInstructions(t *testing.T) {
	for name, text := range map[string]string{
		"prompt": browsePrompt("inspect permissions", "", nil, false, nil),
		litHelp:  (&app{}).browseCmd().Long,
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(text, "Expands the Organization permissions section") ||
				!strings.Contains(text, "submitting a grant") && !strings.Contains(text, "grant record") {
				t.Fatalf("missing disclosure instructions in %s", name)
			}
		})
	}
}
