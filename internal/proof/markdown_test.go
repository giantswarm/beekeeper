package proof

import (
	"context"
	"fmt"
	"testing"
)

// Alt text must leave the closing image delimiter intact, even after
// brackets are stripped, and must not introduce a paragraph break.
func TestUploadMarkdownAlt(t *testing.T) {
	for _, tc := range []struct {
		name, alt, label string
	}{
		{"trailing backslash", `screenshot\`, `screenshot\\`},
		{"two trailing backslashes", `screenshot\\`, `screenshot\\\\`},
		{"brackets beside backslash", `shot [1\]`, `shot 1\\`},
		{"carriage returns", "shot\r\rnext", "shot  next"},
		{"mixed newlines", "shot\r\nnext\nlast", "shot  next last"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeGH{visibility: public}
			res, err := (Upload{GH: f.run, Repo: repo, Alt: tc.alt, Now: at}).Run(context.Background(), []byte("img"))
			if err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf("![%s](%s)", tc.label, res.URL)
			if res.Markdown != want {
				t.Fatalf("Markdown = %q, want %q", res.Markdown, want)
			}
		})
	}
}
