package secret

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestStdinRejectsJSONNullBeforeReadingOrWriting(t *testing.T) {
	for _, template := range []string{"null", " \nnull\t"} {
		t.Run(template, func(t *testing.T) {
			in := Stdin{Template: template, Field: ageItemField}
			if err := in.Check(); err == nil {
				t.Fatal("null was accepted as a JSON object")
			}
			if _, err := in.input("synthetic-value"); err == nil {
				t.Fatal("null was accepted while constructing stdin")
			}
			calls := 0
			o := &Ops{Vault: "Shared", Token: "synthetic-token", Run: func(context.Context, string, []string, io.Reader, string, ...string) ([]byte, error) {
				calls++
				return nil, errors.New("no tool should run")
			}}
			consumer := []string{"gh", "secret", "set", "EXAMPLE"}
			if _, _, err := o.CopyToConsumer(context.Background(), Ref{Op: "op://Shared/example/password"}, consumer, in); err == nil {
				t.Fatal("copy accepted null")
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte("creation_rules:\n  - encrypted_regex: '^password$'\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(dir, "example.yaml")
			if _, err := o.Set(context.Background(), Ref{File: file, Path: ageItemField}, SetOptions{Literal: "public-value", Consumer: consumer, Stdin: in}); err == nil {
				t.Fatal("set accepted null")
			}
			if calls != 0 {
				t.Fatalf("invalid stdin ran %d tools", calls)
			}
			if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid stdin changed the destination: %v", err)
			}
		})
	}
}
