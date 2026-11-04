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

func TestWholeFileOperationsRefuseCollidingPaths(t *testing.T) {
	tools := secrettest.New(map[string]string{vaultRef: password})
	dir, _ := scratch(t)
	files := []string{filepath.Join(dir, "left.sops.yaml"), filepath.Join(dir, "right.sops.yaml")}
	for i, file := range files {
		value := password
		if i == 1 {
			value = token
		}
		plain := "config:\n  auth.token: " + value + "\n  auth:\n    token: shadow\n"
		if err := os.WriteFile(file, secrettest.Encrypt(plain), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	o := ops(tools)
	ctx := context.Background()
	if _, err := o.Compare(ctx, secret.Ref{File: files[0]}, secret.Ref{File: files[1]}); err == nil || !strings.Contains(err.Error(), "ambiguous dotted path") {
		t.Fatalf("comparison hid a leaf: %v", err)
	}
	if _, err := o.Fingerprints(ctx, secret.Ref{File: files[0]}); err == nil {
		t.Fatal("fingerprints silently omitted a leaf")
	}
	ref, _ := secret.ParseRef(vaultRef)
	if _, err := o.RotateGenerated(ctx, ref, files, index(t), 32, "alnum"); err == nil {
		t.Fatal("rotation accepted ambiguous carriers")
	}
	if tools.Vault[vaultRef] != password {
		t.Error("refused rotation changed vault")
	}
	for _, call := range tools.Calls {
		if strings.Contains(call, "encrypt") || strings.HasPrefix(call, "op item edit") {
			t.Fatalf("refusal wrote a value: %s", call)
		}
	}
}
