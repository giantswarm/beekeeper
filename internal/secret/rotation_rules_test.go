package secret_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

func TestRotationChecksEveryCarrierEncryptionRule(t *testing.T) {
	for _, tc := range []struct {
		name, expr      string
		dotted, allowed bool
	}{
		{"rule excludes carriers", "^other$", false, false},
		{"rule excludes base64 carrier", "^stringData$", false, false},
		{"all carriers selected", "^(data|stringData)$", false, true},
		{"dotted segments are not keys", "^password$", true, false},
		{"actual dotted key selected", "^api[.]password$", true, true},
	} {
		for _, generated := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/generated=%t", tc.name, generated), func(t *testing.T) {
				tools := secrettest.New(map[string]string{vaultRef: password})
				files := carriersRepo(t, password)
				if tc.dotted {
					if err := os.WriteFile(files[0], secrettest.Encrypt("api.password: "+password+"\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(filepath.Dir(files[0]), ".sops.yaml"), []byte("creation_rules:\n  - encrypted_regex: '"+tc.expr+"'\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				originals := make([][]byte, len(files))
				for i, file := range files {
					var err error
					originals[i], err = os.ReadFile(file) //nolint:gosec // a path of this test
					if err != nil {
						t.Fatal(err)
					}
				} //nolint:gosec // synthetic carriers
				ix := index(t)
				ix.Add(vaultRef, password)
				ref, _ := secret.ParseRef(vaultRef)
				o := ops(tools)
				if !generated {
					tools.Vault[vaultRef] = issued
				}
				before := tools.Vault[vaultRef]
				var err error
				if generated {
					_, err = o.RotateGenerated(context.Background(), ref, files, ix, 32, "alnum")
				} else {
					_, err = o.RotateIssued(context.Background(), ref, files, ix)
				}
				if tc.allowed {
					if err != nil {
						t.Fatal(err)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), "plaintext") {
					t.Fatalf("excluded carrier accepted: %v", err)
				}
				if tools.Vault[vaultRef] != before {
					t.Fatal("preflight changed vault")
				}
				for i, file := range files {
					after, err := os.ReadFile(file) //nolint:gosec // a path of this test
					if err != nil || string(after) != string(originals[i]) {
						t.Fatal("preflight changed carrier")
					}
				} //nolint:gosec // synthetic carriers
				for _, call := range tools.Calls {
					if strings.HasPrefix(call, "op item edit") || slices.Contains(strings.Fields(call), "encrypt") {
						t.Fatalf("excluded carrier reached a write: %s", call)
					}
				}
			})
		}
	}
}
