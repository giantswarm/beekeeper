package secret

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseNodeRejectsMalformedTrailingYAML(t *testing.T) {
	for _, trailer := range []string{"---\n[hidden-marker", "---\nkey: [hidden-marker", "...\ninvalid: [hidden-marker"} {
		if _, err := parseNode([]byte("key: value\n" + trailer)); err == nil || strings.Contains(err.Error(), "hidden-marker") {
			t.Fatalf("malformed trailer was accepted or exposed: %v", err)
		}
	}
	if _, err := parseNode([]byte("key: value\n---\nother: value\n")); !errors.Is(err, errSeveralDocuments) {
		t.Fatalf("second valid document: %v", err)
	}
	for _, raw := range []string{"key: value\n", "key: value\n# trailing comment\n", "key: value\n...\n"} {
		if _, err := parseNode([]byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMalformedSkeletonRefusesBeforeReadingSource(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "secret.sops.yaml")
	raw := "apiVersion: v1\nkind: Secret\nmetadata:\n  name: example\n---\n[hidden-marker\n"
	if err := os.WriteFile(file, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	o := &Ops{Run: func(context.Context, string, []string, io.Reader, string, ...string) ([]byte, error) {
		calls++
		return nil, errors.New("unexpected tool")
	}}
	if _, err := o.CopyValue(context.Background(), Ref{Op: "op://Shared/example/password"}, Ref{File: file, Path: "password"}); err == nil || strings.Contains(err.Error(), "hidden-marker") {
		t.Fatalf("invalid skeleton: %v", err)
	}
	after, err := os.ReadFile(file) //nolint:gosec // synthetic fixture
	if err != nil || string(after) != raw || calls != 0 {
		t.Fatalf("refusal changed the fixture or ran a tool: %v, calls=%d", err, calls)
	}
}
