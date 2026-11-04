//go:build unix

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

func TestMissingRuleRefusesBeforeAnyProducer(t *testing.T) {
	for _, rules := range []string{"creation_rules: []\n", "creation_rules:\n  - path_regex: '^other/'\n"} {
		for _, action := range []string{"keys", "capture", "copy", "copies", "docker"} {
			t.Run(action+rules, func(t *testing.T) {
				dir := t.TempDir()
				file := filepath.Join(dir, "target.sops.yaml")
				if err := os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte(rules), 0o600); err != nil {
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
					_, err = o.SecretKeys(ctx, file, secret.SecretKeysOptions{New: fluxPull, Keys: []string{"private-key"}, Produce: func(context.Context) (map[string]string, error) {
						produced++
						return map[string]string{"private-key": "synthetic"}, nil
					}})
				case "capture":
					_, err = o.Capture(ctx, secret.Ref{File: file, Path: pwPath}, secret.CaptureOptions{Producer: azGenerateArgs})
				case "copy":
					_, err = o.CopyValue(ctx, src, secret.Ref{File: file, Path: pwPath})
				case "copies":
					_, err = o.CopyValues(ctx, []secret.Pair{{Src: src, Path: pwPath}}, file, fluxPull)
				case "docker":
					_, err = o.CopyDocker(ctx, src, file, secret.DockerConfig{Registry: registry, Username: pullUser}, fluxPull)
				}
				if err == nil || !strings.Contains(err.Error(), "no creation rule") || produced != 0 || len(tools.Calls) != 0 {
					t.Fatalf("missing rule: %v, callbacks=%d, calls=%v", err, produced, tools.Calls)
				}
				for _, path := range []string{ran, file} {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatalf("producer or destination was touched: %s, %v", path, err)
					}
				}
			})
		}
	}
}
