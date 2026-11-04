package secret_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

const encryptionAPIPassword = "api.password"

const encryptionOldLine = "api.password: old\n"

func TestCreationRuleChecksResolvedDottedKeys(t *testing.T) {
	for _, tc := range []struct {
		name, plain, path, rule string
		allowed                 bool
	}{
		{"terminal regex", encryptionOldLine, encryptionAPIPassword, "encrypted_regex: '^password$'", false},
		{"intermediate regex", "api.password:\n  value: old\n", "api.password.value", "encrypted_regex: '^password$'", false},
		{"suffix", "api_secret.password: old\n", "api_secret.password", "encrypted_suffix: _secret", false},
		{"unencrypted regex", encryptionOldLine, encryptionAPIPassword, "unencrypted_regex: '^api[.]password$'", false},
		{"new suffix", "api.password: {}\n", "api.password.value", "encrypted_regex: '^password$'", false},
		{"actual key selected", encryptionOldLine, encryptionAPIPassword, "encrypted_regex: '^api[.]password$'", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools := secrettest.New(map[string]string{vaultRef: password})
			dir := t.TempDir()
			file := filepath.Join(dir, "app.sops.yaml")
			raw := secrettest.Encrypt(tc.plain)
			if err := os.WriteFile(file, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte("creation_rules:\n  - "+tc.rule+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			o := ops(tools)
			run := o.Run
			reads, writes := 0, 0
			o.Run = func(ctx context.Context, dir string, env []string, in io.Reader, name string, args ...string) ([]byte, error) {
				if name == "op" {
					reads++
				}
				if name == "sops" && slices.Contains(args, "encrypt") {
					writes++
				}
				return run(ctx, dir, env, in, name, args...)
			}
			_, err := o.CopyValue(context.Background(), secret.Ref{Op: vaultRef}, secret.Ref{File: file, Path: tc.path})
			if tc.allowed {
				if err != nil || reads != 1 || writes != 1 {
					t.Fatalf("selected actual key refused: %v, reads=%d, writes=%d", err, reads, writes)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "plaintext") || reads != 0 || writes != 0 {
				t.Fatalf("preflight: %v, reads=%d, writes=%d", err, reads, writes)
			}
			after, err := os.ReadFile(file) //nolint:gosec // synthetic fixture
			if err != nil || string(after) != string(raw) {
				t.Fatal("refused write changed destination")
			}
		})
	}
}
