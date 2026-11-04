package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/sandbox"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

func TestSecretRotateSandboxFiles(t *testing.T) {
	const readable = "readable"
	for _, generated := range []bool{false, true} {
		for _, access := range []string{"denied", readable, "writable", "outside-sandbox"} {
			t.Run(strings.Join([]string{map[bool]string{false: "issued", true: "generated"}[generated], access}, "/"), func(t *testing.T) {
				a, tools, repo := secretApp(t)
				base := t.TempDir()
				home := filepath.Join(base, "home")
				if err := os.Mkdir(home, 0o700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("HOME", home)
				t.Setenv("TMPDIR", filepath.Join(base, "tmp"))
				t.Chdir(repo)
				t.Setenv(sandbox.Env, "1")
				t.Setenv(sandbox.Brokered, "1")
				if access == "outside-sandbox" {
					t.Setenv(sandbox.Env, "")
				}
				if access == readable {
					a.cfg.Sandbox.AllowRead = []string{home}
				}
				if access == "writable" {
					a.cfg.Sandbox.AllowWrite = []string{home}
				}
				if err := os.WriteFile(filepath.Join(home, ".sops.yaml"), []byte("creation_rules: []\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				closed := filepath.Join(home, "credentials.sops.yaml")
				original := secrettest.Encrypt("password: " + secretValue + "\n")
				if err := os.WriteFile(closed, original, 0o600); err != nil {
					t.Fatal(err)
				}
				local := filepath.Join(repo, "db.sops.yaml")
				localBefore, err := os.ReadFile(local) //nolint:gosec // synthetic file created by secretApp in its temporary repository
				if err != nil {
					t.Fatal(err)
				}
				a.cfg.Scan.SOPS = []string{local, closed}
				args := []string{"rotate", dbRef}
				if generated {
					args = append(args, generateFlag)
				} else {
					ix, err := guard.OpenIndex(a.scanDir())
					if err != nil {
						t.Fatal(err)
					}
					ix.Add(dbRef, secretValue)
					if err := ix.Save(a.now); err != nil {
						t.Fatal(err)
					}
					tools.Vault[dbRef] = "replacement-fixture-value-578631"
				}
				vaultBefore := tools.Vault[dbRef]
				out, err := runSecret(a, args...)
				if access == "denied" || access == readable {
					if err == nil || Code(err) != ExitRefused || !strings.Contains(err.Error(), "agent sandbox does not let") {
						t.Errorf("rotation outside the sandbox's write list: %v", err)
					}
					if len(tools.Calls) != 0 {
						t.Error("a refused rotation reached the secret tools")
					}
					if tools.Vault[dbRef] != vaultBefore {
						t.Error("a refused rotation changed the vault")
					}
					for file, before := range map[string][]byte{closed: original, local: localBefore} {
						after, rerr := os.ReadFile(file) //nolint:gosec // synthetic files owned by this test
						if rerr != nil || !bytes.Equal(before, after) {
							t.Errorf("a refused rotation changed %s (read error %v)", filepath.Base(file), rerr)
						}
					}
				} else if err != nil || !strings.Contains(out, closed+"#password") || !strings.Contains(out, local+"#data.password") {
					t.Errorf("rotation of permitted files: %v", err)
				}
				noSecret(t, "rotation output", out)
			})
		}
	}
}
