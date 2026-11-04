package secret_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

func TestAmbiguousWholeFilePathsRefused(t *testing.T) {
	for _, shape := range []string{
		"config:\n  auth.token: VALUE\n  auth:\n    token: shadow\n",
		"config:\n  auth:\n    token: shadow\n  auth.token: VALUE\n",
		"config:\n  auth.0: VALUE\n  auth:\n    - shadow\n",
	} {
		t.Run(shape, func(t *testing.T) {
			dir, valid := scratch(t)
			var files []string
			originals := map[string][]byte{}
			for i, value := range []string{password, token} {
				file := filepath.Join(dir, string(rune('a'+i))+".sops.yaml")
				raw := secrettest.Encrypt(strings.ReplaceAll(shape, "VALUE", value))
				if err := os.WriteFile(file, raw, 0o600); err != nil {
					t.Fatal(err)
				}
				files = append(files, file)
				originals[file] = raw
			}
			tools := secrettest.New(map[string]string{vaultRef: password})
			o := ops(tools)
			ctx := context.Background()
			check := func(err error) {
				t.Helper()
				if err == nil || !strings.Contains(err.Error(), "ambiguous") {
					t.Fatalf("expected ambiguity refusal, got %v", err)
				}
				noValue(t, "error", err.Error())
			}
			_, err := o.Compare(ctx, secret.Ref{File: files[0]}, secret.Ref{File: files[1]})
			check(err)
			// Check either operand, including a valid first operand.
			_, err = o.Compare(ctx, secret.Ref{File: valid}, secret.Ref{File: files[0]})
			check(err)
			_, err = o.Fingerprints(ctx, secret.Ref{File: files[0]})
			check(err)
			ref := secret.Ref{Op: vaultRef}
			_, err = o.RotateGenerated(ctx, ref, append([]string{valid}, files...), index(t), 32, "alnum")
			check(err)
			ix := index(t)
			ix.Add(vaultRef, password)
			tools.Vault[vaultRef] = issued
			_, err = o.RotateIssued(ctx, ref, append([]string{valid}, files...), ix)
			check(err)
			if tools.Vault[vaultRef] != issued {
				t.Fatal("rotation changed vault")
			}
			for _, call := range tools.Calls {
				if strings.Contains(call, "encrypt") || strings.HasPrefix(call, "op item") {
					t.Fatalf("unexpected write: %s", call)
				}
			}
			for file, original := range originals {
				got, err := os.ReadFile(file) //nolint:gosec // a path of this test
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != string(original) {
					t.Fatal("rotation changed carrier")
				}
			}
		})
	}
}

func TestUnambiguousDottedKeyStillReadable(t *testing.T) {
	dir, _ := scratch(t)
	file := filepath.Join(dir, "dotted.sops.yaml")
	if err := os.WriteFile(file, secrettest.Encrypt("config:\n  auth.token: "+password+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := ops(secrettest.New(map[string]string{vaultRef: password}))
	result, err := o.Compare(context.Background(), secret.Ref{File: file, Path: "config.auth.token"}, secret.Ref{Op: vaultRef})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0].State != secret.Equal {
		t.Fatalf("expected equal dotted key comparison: %+v", result)
	}
	noValue(t, "compare", result)
	fps, err := o.Fingerprints(context.Background(), secret.Ref{File: file})
	if err != nil || len(fps) != 1 {
		t.Fatalf("fingerprints: %v, %v", fps, err)
	}
}
