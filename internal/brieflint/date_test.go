package brieflint

import (
	"strings"
	"testing"
)

func TestLintDateFilenameAndPunctuation(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want bool
	}{
		{"case 0: Markdown filename", "Read 2026-11-04.md for the decision.", false},
		{"case 1: multiple extensions", "Read 2026-11-04.tar.gz for the record.", false},
		{"case 2: numeric extension", "Read 2026-11-04.1.log for the record.", false},
		{"case 3: linked filename", "Read [2026-11-04.md](decisions/2026-11-04.md).", false},
		{"case 4: date at end", "Updated 2026-11-04", true},
		{"case 5: sentence period", "Updated 2026-11-04.", true},
		{"case 6: next sentence", "Updated 2026-11-04. Read the log.", true},
		{"case 7: ellipsis", "Updated 2026-11-04...", true},
		{"case 8: parenthesis", "Updated (2026-11-04).", true},
		{"case 9: comma", "Updated 2026-11-04, read the log.", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Lint("brief.md", strings.NewReader(tc.text))
			if err != nil {
				t.Fatal(err)
			}
			if tc.want {
				if len(got) != 1 || got[0].Rule != ruleDate || got[0].Line != 1 {
					t.Fatalf("Lint(%q) = %+v, want one date finding", tc.text, got)
				}
			} else if len(got) != 0 {
				t.Fatalf("Lint(%q) = %+v, want none", tc.text, got)
			}
		})
	}
}
