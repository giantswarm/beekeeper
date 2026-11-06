package brieflint

import (
	"slices"
	"strings"
	"testing"
)

func TestLintShellRedirectSyntax(t *testing.T) {
	for _, tc := range []struct {
		name  string
		text  string
		lines []int
	}{
		{"case 0: combined streams", "```bash\nmake test &>gate.log\n```", []int{2}},
		{"case 1: digit-prefixed file", "```sh\nmake test >1.log\n```", []int{2}},
		{"case 2: numeric file in shell fence", "```sh\nprintf ok >123\n```", []int{2}},
		{"case 3: quoted text", "```sh\nprintf '%s\\n' 'a > b'\n```", nil},
		{"case 4: comment", "```sh\n# use >out to capture output\nprintf ok\n```", nil},
		{"case 5: escaped operator", "```sh\nprintf '%s\\n' a\\>b\n```", nil},
		{"case 6: quoted here-document", "```sh\ncat <<'EOF'\na > b\nEOF\n```", nil},
		{"case 7: here-document and real redirect", "```sh\ncat >out <<'EOF'\na > b\nEOF\n```", []int{2}},
		{"case 8: multiline string", "```sh\nprintf '%s' 'a\n>out\nb'\n```", nil},
		{"case 9: nested command substitution", "```bash\nprintf '%s' \"$(printf ok >out)\"\n```", []int{2}},
		{"case 10: real redirect after comment", "```sh\n# quote >out\nprintf ok >out\n```", []int{3}},
		{"case 11: one finding per line", "```sh\nprintf ok >out 2>err\n```", []int{2}},
		{"case 12: safe redirects", "```bash\nprintf ok &>>out\nprintf ok >|out 2>&1\nprintf ok >/dev/null\n```", nil},
		{"case 13: safe process substitution", "```bash\ntee >(gzip >|log.gz)\n```", nil},
		{"case 14: unsafe process substitution", "```bash\ntee >(gzip >log.gz)\n```", []int{2}},
		{"case 15: incomplete here-document", "```sh\ncat >out <<EOF\n```", []int{2}},
		{"case 16: unterminated fence", "Before.\n```sh\nprintf ok >out", []int{3}},
		{"case 17: non-shell fence", "```go\nif a > b {\n```", nil},
		{"case 18: zsh quotes and redirect", "```zsh\nprint 'a > b'\nprint ok >out\n```", []int{3}},
		{"case 19: inline quoted text", "Print `printf '%s' 'a > b'`.", nil},
		{"case 20: inline digit-prefixed file", "Capture `printf ok >1.log`.", []int{1}},
		{"case 21: shell numeric comparison", "```bash\n[[ $a > b ]]\n(( n > 0 ))\n```", nil},
		{"case 22: combined redirect shorthand", "```bash\nprintf ok >&out.log\n```", []int{2}},
		{"case 23: descriptor duplication and closure", "```bash\nprintf ok >&1\nprintf ok 2>&1\nprintf ok >&-\nprintf ok 2>&\"$fd\"\n```", nil},
		{"case 24: digit-prefixed combined target", "Capture `printf ok >&1.log`.", []int{1}},
		{"multiline input and output redirects", "```sh\ncat <input\nprintf ok>out\n```", []int{3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Lint("brief.md", strings.NewReader(tc.text))
			if err != nil {
				t.Fatal(err)
			}
			var lines []int
			for _, f := range got {
				if f.Rule != ruleClobber || f.Path != "brief.md" || f.Why == "" {
					t.Fatalf("unexpected finding: %+v", f)
				}
				lines = append(lines, f.Line)
			}
			if !slices.Equal(lines, tc.lines) {
				t.Fatalf("Lint(%q) = %+v, want clobber lines %v", tc.text, got, tc.lines)
			}
		})
	}
}
