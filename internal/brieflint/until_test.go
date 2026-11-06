package brieflint

import (
	"strings"
	"testing"
)

func TestLintUntilShipsDottedNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want bool
	}{
		{"case 0: release version", "Use the script until devctl v8.82.8 ships.", true},
		{"case 1: dotted command", "Use the script until `rollout.wait` is installed.", true},
		{"case 2: dotted filename", "Use the script until rollout.yaml lands.", true},
		{"case 3: next sentence", "Use the script until morning. The release ships tonight.", false},
		{"case 4: semicolon", "Use the script until morning; the release ships tonight.", false},
		{"case 5: next line", "Use the script until morning\nthe release ships tonight.", false},
		{"case 6: bounded clause", "Use the script until " + strings.Repeat("v1.2.3 ", 20) + "ships.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Lint("brief.md", strings.NewReader(tc.text))
			if err != nil {
				t.Fatal(err)
			}
			if tc.want {
				if len(got) != 1 || got[0].Rule != ruleUntil || got[0].Line != 1 {
					t.Fatalf("Lint(%q) = %+v, want one until-ships finding", tc.text, got)
				}
			} else if len(got) != 0 {
				t.Fatalf("Lint(%q) = %+v, want none", tc.text, got)
			}
		})
	}
}
