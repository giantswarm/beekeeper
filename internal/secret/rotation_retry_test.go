package secret_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

func TestPartialRotationResumesAfterIndexReopen(t *testing.T) {
	for _, generated := range []bool{false, true} {
		for _, failAll := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{false: "issued", true: "generated"}[generated], map[bool]string{false: "partial", true: "all"}[failAll]}, "/"), func(t *testing.T) {
				tools := secrettest.New(map[string]string{vaultRef: password})
				files := carriersRepo(t, password)
				if err := os.WriteFile(files[1], secrettest.Encrypt("stringData:\n  password: "+password+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				dir := t.TempDir()
				ix, err := guard.OpenIndex(dir)
				if err != nil {
					t.Fatal(err)
				}
				ix.Add(vaultRef, password)
				if !generated {
					tools.Vault[vaultRef] = issued
				}
				ref, _ := secret.ParseRef(vaultRef)
				o := ops(tools)
				run := o.Run
				fail := true
				encrypts := 0
				o.Run = func(ctx context.Context, dir string, env []string, in io.Reader, name string, args ...string) ([]byte, error) {
					if name == "sops" && slices.Contains(args, "encrypt") {
						encrypts++
						i := slices.Index(args, "--filename-override")
						if fail && (failAll || i >= 0 && args[i+1] == filepath.Base(files[1])) {
							return nil, errors.New("injected encryption failure")
						}
					}
					return run(ctx, dir, env, in, name, args...)
				}
				rotate := func() (secret.Rotation, error) {
					if generated {
						return o.RotateGenerated(context.Background(), ref, files, ix, 32, "alnum")
					}
					return o.RotateIssued(context.Background(), ref, files, ix)
				}
				first, err := rotate()
				if err == nil || first.Ref == "" {
					t.Fatalf("missing partial result: %+v, %v", first, err)
				}
				replacement := tools.Vault[vaultRef]
				if err := ix.Save(time.Now()); err != nil {
					t.Fatal(err)
				}
				ix, err = guard.OpenIndex(dir)
				if err != nil {
					t.Fatal(err)
				}
				if _, _, pending := ix.PendingRotation(vaultRef); !pending {
					t.Fatal("recovery metadata was lost")
				}
				raw, err := os.ReadFile(filepath.Join(dir, "index.json")) //nolint:gosec // synthetic index
				if err != nil || strings.Contains(string(raw), replacement) || strings.Contains(string(raw), password) {
					t.Fatal("index stored a credential")
				}
				// A third value must not supersede the partially written replacement.
				tools.Vault[vaultRef] = unrelated
				before := encrypts
				if _, err := rotate(); err == nil || encrypts != before {
					t.Fatal("changed replacement was accepted")
				}
				tools.Vault[vaultRef] = replacement
				tools.Calls = nil
				fail = false
				before = encrypts
				if _, err := rotate(); err != nil {
					t.Fatal(err)
				}
				want := 1
				if failAll {
					want = 2
				}
				if encrypts-before != want {
					t.Fatalf("rewrote completed carriers: encryptions=%d, want %d", encrypts-before, want)
				}
				for _, file := range files {
					if leaf(t, tools, file, "password") != replacement {
						t.Fatal("old carrier remains")
					}
				}
				if tools.Vault[vaultRef] != replacement {
					t.Fatal("retry generated another replacement")
				}
				for _, call := range tools.Calls {
					if strings.HasPrefix(call, "op item edit") {
						t.Fatal("retry rewrote the vault")
					}
				}
				if _, _, pending := ix.PendingRotation(vaultRef); pending {
					t.Fatal("completed rotation remains pending")
				}
				if fp, _ := ix.ValueFingerprint(vaultRef); fp != ix.Fingerprint(strings.TrimSpace(replacement)) {
					t.Fatal("completion did not advance fingerprint")
				}
			})
		}
	}
}
