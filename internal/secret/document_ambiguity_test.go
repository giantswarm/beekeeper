package secret

import (
	"strings"
	"testing"
)

func TestParseDocumentPathAmbiguity(t *testing.T) {
	for _, tc := range []struct {
		name      string
		raw       string
		ambiguous bool
	}{
		{"empty path", "'': first\n'': second\n", true},
		{"nested empty path", "'':\n  '': first\n'': second\n", true},
		{"sequence index", "items.0: first\nitems: [second]\n", true},
		{"ordinary dotted key", "config:\n  auth.token: value\n", false},
		{"ordinary nested key", "config:\n  auth:\n    token: value\n", false},
		{"single empty key", "'': value\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseDocument([]byte(tc.raw))
			if tc.ambiguous {
				if err == nil || !strings.Contains(err.Error(), "ambiguous dotted path") {
					t.Fatalf("expected ambiguity refusal, got %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
