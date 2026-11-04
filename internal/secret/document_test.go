package secret

import (
	"errors"
	"strings"
	"testing"
)

func TestParseNodeTrailingDocuments(t *testing.T) {
	const planted = "planted-trailing-secret"
	for _, tc := range []struct {
		name, tail, want string
	}{
		{"EOF", "", ""},
		{"comments before EOF", "# comment\n", ""},
		{"explicit end", "...\n", ""},
		{"second document", "---\nother: value\n", errSeveralDocuments.Error()},
		{"empty second document", "---\n", errSeveralDocuments.Error()},
		{"malformed sequence", "---\n[" + planted + "\n", "invalid trailing YAML content"},
		{"unknown alias", "---\n*" + planted + "\n", "invalid trailing YAML content"},
		{"malformed after end", "...\n" + planted + "\n", "invalid trailing YAML content"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, err := parseNode([]byte("key: value\n" + tc.tail))
			if tc.want == "" {
				if err != nil || n == nil {
					t.Fatalf("expected a document, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("accepted trailing document content")
			}
			if n != nil || err.Error() != tc.want {
				t.Errorf("got node %v, error %v; want %q", n, err, tc.want)
			}
			if strings.Contains(err.Error(), planted) {
				t.Error("error reveals trailing content")
			}
			if tc.want == errSeveralDocuments.Error() && !errors.Is(err, errSeveralDocuments) {
				t.Error("lost multiple-document error")
			}
		})
	}
}
