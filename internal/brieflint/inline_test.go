package brieflint

import (
	"strings"
	"testing"
)

func TestLintClobberInlineBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		line int
	}{
		{"case 0: multiline span", "Capture with `printf ok\n>out.log`.", 2},
		{"case 1: substitution inside double ticks", "Capture with ``echo `printf ok >out.log` ``.", 1},
		{"case 2: substitution on later line", "A paragraph.\nCapture with ``echo\n`printf ok >out.log` ``.", 3},
		{"case 3: three ticks inside four", "Capture with ````printf '```' >out.log````.", 1},
		{"case 4: mismatched ticks", "A marker ` then prose >out.log and ``safe``.", 0},
		{"case 5: unmatched span", "A marker ` then prose >out.log.", 0},
		{"case 6: separate spans", "Compare `n >` with `0`.", 0},
		{"case 7: non-shell outer fence", "````markdown\nCapture with `printf ok >out.log`.\n````", 0},
		{"case 8: shell fence", "```sh\nprintf ok >out.log\n```", 2},
		{"case 9: prose after multiline span", "Use `printf ok\n safely` then prose >out.log.", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Lint("brief.md", strings.NewReader(tc.text))
			if err != nil {
				t.Fatal(err)
			}
			if tc.line == 0 {
				if len(got) != 0 {
					t.Fatalf("Lint(%q) = %+v, want none", tc.text, got)
				}
			} else if len(got) != 1 || got[0].Rule != ruleClobber || got[0].Line != tc.line || got[0].Path != "brief.md" {
				t.Fatalf("Lint(%q) = %+v, want one clobber finding on line %d", tc.text, got, tc.line)
			}
		})
	}
}
