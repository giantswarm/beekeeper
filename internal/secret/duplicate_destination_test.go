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

const duplicatePassword = "password"

func TestCopyValuesRefusesResolvedDuplicateDestinations(t *testing.T) {
	for _, tc := range []struct{ a, b string }{
		{duplicatePassword, `duplicatePassword`}, {`"credentials.json"`, `credentials\.json`}, {"nested.password", `nested.duplicatePassword`},
	} {
		for _, skeleton := range []bool{false, true} {
			t.Run(tc.a+tc.b+string(rune('0'+boolNumber(skeleton))), func(t *testing.T) { //nolint:gosec // small test index
				tools := secrettest.New(map[string]string{vaultRef: password})
				dir := t.TempDir()
				file := filepath.Join(dir, "new.sops.yaml")
				if err := os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte("creation_rules:\n  - path_regex: '.*'\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				raw := "apiVersion: v1\nkind: Secret\nmetadata:\n  name: example\nstringData: {}\n"
				if skeleton {
					if err := os.WriteFile(file, []byte(raw), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				pairs := []secret.Pair{{Src: secret.Ref{Op: vaultRef}, Path: tc.a}, {Src: secret.Ref{Op: vaultRef}, Path: tc.b}}
				if _, err := ops(tools).CopyValues(context.Background(), pairs, file, nil); err == nil || !strings.Contains(err.Error(), "named twice") {
					t.Fatalf("duplicate accepted: %v", err)
				}
				if len(tools.Calls) != 0 {
					t.Fatalf("duplicate read a source: %v", tools.Calls)
				}
				after, err := os.ReadFile(file) //nolint:gosec // synthetic fixture
				if skeleton {
					if err != nil || string(after) != raw {
						t.Fatal("skeleton changed")
					}
				} else if !os.IsNotExist(err) {
					t.Fatal("destination created")
				}
			})
		}
	}
}

func boolNumber(v bool) int {
	if v {
		return 1
	}
	return 0
}
