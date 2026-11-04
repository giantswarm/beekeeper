package secret_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

func TestCopyValueChecksResolvedYAMLKeys(t *testing.T) {
	for _, tc := range []struct {
		name, plain, path, selector string
		allowed                     bool
	}{
		{"terminal", "api.password: old\n", "api.password", "encrypted_regex: '^password$'", false},
		{"intermediate", "api.password:\n  token: old\n", "api.password.token", "encrypted_regex: '^password$'", false},
		{"new descendant", "api.password: {}\n", "api.password.token", "encrypted_regex: '^password$'", false},
		{"null parent", "api.password:\n", "api.password.token", "encrypted_regex: '^password$'", false},
		{"encrypted suffix", "api.password:\n  token: old\n", "api.password.token", "encrypted_suffix: api", false},
		{"unencrypted regex", "api.password: old\n", "api.password", "unencrypted_regex: '^api[.]password$'", false},
		{"unencrypted suffix", "api.password:\n  token: old\n", "api.password.token", "unencrypted_suffix: api.password", false},
		{"embedded refused", "api.password: |\n  token: old\n", "api.password.token", "encrypted_regex: '^password$'", false},
		{"actual dotted key", "api.password: old\n", "api.password", "encrypted_regex: '^api[.]password$'", true},
		{"actual dotted parent", "api.password: {}\n", "api.password.token", "encrypted_regex: '^api[.]password$'", true},
		{"embedded allowed", "api.password: |\n  token: old\n", "api.password.token", "encrypted_regex: '^api[.]password$'", true},
		{"nested new key", "api: {}\n", "api.password", "encrypted_regex: '^password$'", true},
		{"quoted dotted key", "api.password: old\n", `"api.password"`, "encrypted_regex: '^api[.]password$'", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "destination.sops.yaml")
			before := secrettest.Encrypt(tc.plain)
			if err := os.WriteFile(file, before, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte("creation_rules:\n  - "+tc.selector+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			tools := secrettest.New(map[string]string{vaultRef: password})
			_, err := ops(tools).CopyValue(context.Background(), secret.Ref{Op: vaultRef}, secret.Ref{File: file, Path: tc.path})
			reads, encryptions := 0, 0
			for _, call := range tools.Calls {
				args := strings.Fields(call)
				if len(args) > 1 && args[0] == "op" && args[1] == "read" {
					reads++
				}
				if len(args) > 0 && args[0] == "sops" && slices.Contains(args[1:], "encrypt") {
					encryptions++
				}
			}
			if tc.allowed {
				if err != nil {
					t.Fatal(err)
				}
				if reads != 1 || encryptions != 1 {
					t.Fatalf("reads=%d encryptions=%d", reads, encryptions)
				}
				if plain := decrypted(t, tools, file); !strings.Contains(plain, password) {
					t.Fatal("allowed write lost the value")
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "plaintext") {
					t.Fatalf("expected plaintext refusal, got %v", err)
				}
				if reads != 0 || encryptions != 0 {
					t.Fatalf("refused write: reads=%d encryptions=%d", reads, encryptions)
				}
				after, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, after) {
					t.Fatal("refused write changed the file")
				}
			}
		})
	}
}
