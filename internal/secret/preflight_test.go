//go:build unix

package secret_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

func TestDestinationShapeRefusesBeforeProducing(t *testing.T) {
	for _, shape := range []string{"stringData:\n  token: {}\n", "stringData:\n  token: []\n", "stringData: []\n"} {
		for _, operation := range []string{"keys", "copy", "capture"} {
			t.Run(operation+"/"+strings.TrimSpace(shape), func(t *testing.T) {
				dir, _ := scratch(t)
				file := filepath.Join(dir, "target.sops.yaml")
				raw := []byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: target\n" + shape)
				if err := os.WriteFile(file, raw, 0o600); err != nil {
					t.Fatal(err)
				}
				ran := fakeAz(t, 0)
				tools := secrettest.New(nil)
				o := producing(tools)
				dst := secret.Ref{File: file, Path: "token"}
				callbacks := 0
				var err error
				switch operation {
				case "keys":
					_, err = o.SecretKeys(context.Background(), file, secret.SecretKeysOptions{Keys: []string{"token"}, Replace: true, Produce: func(context.Context) (map[string]string, error) {
						callbacks++
						return map[string]string{"token": "value"}, nil
					}})
				case "copy":
					src, parseErr := secret.ParseRef(azGenerate)
					if parseErr != nil {
						t.Fatal(parseErr)
					}
					_, err = o.CopyValue(context.Background(), src, dst)
				case "capture":
					_, err = o.Capture(context.Background(), dst, secret.CaptureOptions{Producer: azGenerateArgs})
				}
				if err == nil {
					t.Fatal("destination shape accepted")
				}
				if !strings.Contains(err.Error(), "mapping") && !strings.Contains(err.Error(), "list") {
					t.Fatalf("unexpected refusal: %v", err)
				}
				if callbacks != 0 || len(tools.Calls) != 0 {
					t.Fatalf("callbacks=%d tools=%v", callbacks, tools.Calls)
				}
				if _, err := os.Stat(ran); !os.IsNotExist(err) {
					t.Fatalf("producer ran: %v", err)
				}
				after, err := os.ReadFile(file)
				if err != nil || !bytes.Equal(raw, after) {
					t.Fatalf("destination changed: %v", err)
				}
			})
		}
	}
}

func TestSecretKeysNonScalarDataRequiresReplace(t *testing.T) {
	dir, _ := scratch(t)
	file := filepath.Join(dir, "target.sops.yaml")
	raw := []byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: target\ndata:\n  token: {}\n")
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	tools := secrettest.New(nil)
	callbacks := 0
	_, err := ops(tools).SecretKeys(context.Background(), file, secret.SecretKeysOptions{Keys: []string{"token"}, Produce: func(context.Context) (map[string]string, error) {
		callbacks++
		return map[string]string{"token": "value"}, nil
	}})
	if err == nil || !strings.Contains(err.Error(), "--replace") {
		t.Fatalf("occupancy refusal: %v", err)
	}
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	rules := secretRules + "    age: " + identity.Recipient().String() + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := secret.PlanSecretKeys(file, []string{"token"}, false); err == nil || !strings.Contains(err.Error(), "--replace") {
		t.Fatalf("plan occupancy refusal: %v", err)
	}
	if callbacks != 0 || len(tools.Calls) != 0 {
		t.Fatal("producer or tool ran")
	}
	after, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatalf("destination changed: %v", err)
	}
}
