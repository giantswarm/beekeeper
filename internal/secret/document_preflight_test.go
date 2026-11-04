package secret

import (
	"bytes"
	"testing"
)

func TestCheckSetKeepsDocumentUnchanged(t *testing.T) {
	for _, tc := range []struct {
		raw, path string
		valid     bool
	}{
		{"stringData:\n", "stringData.token", true},
		{"stringData:\n  config: |\n    token: old # kept\n    other: unchanged\n", "stringData.config.token", true},
		{"stringData:\n  config: |\n    token: {}\n", "stringData.config.token", false},
		{"stringData: []\n", "stringData.token", false},
		{"stringData: {}\n", "stringData.token", true},
	} {
		t.Run(tc.path+tc.raw, func(t *testing.T) {
			doc, err := parseDocument([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			before, err := doc.encode()
			if err != nil {
				t.Fatal(err)
			}
			err = doc.checkSet(tc.path)
			if (err == nil) != tc.valid {
				t.Fatalf("checkSet: %v, valid=%v", err, tc.valid)
			}
			after, err := doc.encode()
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("preflight mutated document: %v", err)
			}
			if tc.valid {
				if err := doc.set(tc.path, "new"); err != nil {
					t.Fatalf("valid set: %v", err)
				}
			}
		})
	}
}
