//go:build unix

package secret_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

func TestUnwritableDestinationRefusesBeforeProduction(t *testing.T) {
	for _, tc := range []struct{ section, path, shape string }{
		{"stringData", "private-key", "{}"}, {"stringData", "private-key", "[]"},
		{"stringData", "broken.child", "[]"}, {"data", "private-key", "{}"},
	} {
		for _, action := range []string{"keys", "copy", "capture"} {
			// A data node is occupied for SecretKeys; copy/capture write a separate stringData key.
			if tc.section == "data" && action != "keys" {
				continue
			}
			t.Run(action+tc.section+tc.path+tc.shape, func(t *testing.T) {
				dir := t.TempDir()
				file := filepath.Join(dir, "target.sops.yaml")
				key := tc.path
				if key == "broken.child" {
					key = "broken"
				}
				raw := "apiVersion: v1\nkind: Secret\nmetadata:\n  name: example\n" + tc.section + ":\n  " + key + ": " + tc.shape + "\n"
				if err := os.WriteFile(file, []byte(raw), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte(secretRules), 0o600); err != nil {
					t.Fatal(err)
				}
				tools := secrettest.New(nil)
				o := producing(tools)
				ran := fakeAz(t, 0)
				src, err := secret.ParseRef(azGenerate)
				if err != nil {
					t.Fatal(err)
				}
				produced := 0
				ctx := context.Background()
				switch action {
				case "keys":
					_, err = o.SecretKeys(ctx, file, secret.SecretKeysOptions{Keys: []string{tc.path}, Produce: func(context.Context) (map[string]string, error) {
						produced++
						return map[string]string{tc.path: "synthetic"}, nil
					}})
				case "copy":
					_, err = o.CopyValue(ctx, src, secret.Ref{File: file, Path: tc.path})
				case "capture":
					_, err = o.Capture(ctx, secret.Ref{File: file, Path: tc.path}, secret.CaptureOptions{Producer: azGenerateArgs})
				}
				if err == nil || produced != 0 || len(tools.Calls) != 0 {
					t.Fatalf("shape accepted before production: %v, callbacks=%d, calls=%v", err, produced, tools.Calls)
				}
				if _, err := os.Stat(ran); !os.IsNotExist(err) {
					t.Fatal("producer ran")
				}
				after, err := os.ReadFile(file) //nolint:gosec // synthetic fixture
				if err != nil || string(after) != raw {
					t.Fatal("preflight changed destination")
				}
			})
		}
	}
}
