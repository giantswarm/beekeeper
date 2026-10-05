package secret_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

func TestSetupCreatesTheVaultAndWritesTheTokenOnlyToItsFile(t *testing.T) {
	tools := secrettest.New(nil)
	tools.Signed = true
	file := filepath.Join(t.TempDir(), "beekeeper", "op-token")
	o := &secret.Ops{Run: tools.Run, Vault: shared}
	s, err := o.Setup(context.Background(), "beekeeper-lab", file)
	if err != nil {
		t.Fatal(err)
	}
	tok := secrettest.AccountToken("beekeeper-lab")
	if !s.VaultCreated || s.VaultID != "vid-Shared" || s.TokenBytes != len(tok) {
		t.Errorf("setup = %+v", s)
	}
	noValue(t, "setup", s)
	if strings.Contains(strings.Join(tools.Calls, "\n"), tok) {
		t.Error("a command line carries the token")
	}
	if got := tools.Accounts["beekeeper-lab"]; got != "Shared:read_items,write_items" {
		t.Errorf("service account grants = %q", got)
	}
	raw, err := os.ReadFile(file) //nolint:gosec // the test's own temp file
	if err != nil || strings.TrimSpace(string(raw)) != tok {
		t.Fatalf("token file = %v", err)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
		t.Errorf("token file mode %v", fi.Mode().Perm())
	}
	// A second setup refuses the file that holds a token.
	if _, err := o.Setup(context.Background(), "beekeeper-lab-2", file); !errors.Is(err, secret.ErrSetUp) {
		t.Errorf("second setup = %v", err)
	}
	if _, ok := tools.Accounts["beekeeper-lab-2"]; ok {
		t.Error("a second service account was created")
	}
}

func TestSetupKeepsAnExistingVaultAndNeedsTheSession(t *testing.T) {
	tools := secrettest.New(nil)
	tools.Vaults = map[string]string{shared: "vid-existing"}
	o := &secret.Ops{Run: tools.Run, Vault: shared}
	file := filepath.Join(t.TempDir(), "op-token")
	if _, err := o.Setup(context.Background(), "sa", file); !errors.Is(err, secret.ErrVault) {
		t.Errorf("signed out: %v", err)
	}
	if slices.ContainsFunc(tools.Calls, func(c string) bool { return strings.HasPrefix(c, "op vault create") }) {
		t.Error("a signed-out session tried to create the vault")
	}
	tools.Signed = true
	s, err := o.Setup(context.Background(), "sa", file)
	if err != nil || s.VaultCreated || s.VaultID != "vid-existing" {
		t.Errorf("setup = %+v, %v", s, err)
	}
	if slices.ContainsFunc(tools.Calls, func(c string) bool { return strings.HasPrefix(c, "op vault create") }) {
		t.Error("an existing vault was created again")
	}
}

const employeeRef = "op://Employee/lab-key/credential"

func TestImportReadsAsThePersonAndWritesAsTheServiceAccount(t *testing.T) {
	tools := secrettest.New(map[string]string{employeeRef: password})
	tools.Signed = true
	o := ops(tools)
	n, err := o.Import(context.Background(), secret.Ref{Op: employeeRef}, secret.Ref{Op: "op://Shared/lab-key/credential"})
	if err != nil || n != len(password) {
		t.Fatalf("import = %d, %v", n, err)
	}
	if tools.Vault["op://Shared/lab-key/credential"] != password {
		t.Error("the shared vault does not hold the value")
	}
	if strings.Contains(strings.Join(tools.Calls, "\n"), password) {
		t.Error("a command line carries the value")
	}
	if slices.ContainsFunc(tools.Tokens, func(s string) bool { return s != saToken }) {
		t.Errorf("tokens = %q", tools.Tokens)
	}
	for _, bad := range [][2]string{
		{"op://Shared/lab-key/credential", "op://Shared/x/credential"},
		{employeeRef, "op://Other/x/credential"},
	} {
		if _, err := o.Import(context.Background(), secret.Ref{Op: bad[0]}, secret.Ref{Op: bad[1]}); err == nil {
			t.Errorf("import %s to %s passes", bad[0], bad[1])
		}
	}
}

func TestSessionModeReadsAndWritesAsThePerson(t *testing.T) {
	tools := secrettest.New(map[string]string{employeeRef: password})
	o := &secret.Ops{Run: tools.Run, Vault: "Employee", Session: true, Fingerprint: func(string) string { return "fp" }}
	ref := secret.Ref{Op: employeeRef}
	if _, err := o.Fingerprints(context.Background(), ref); !errors.Is(err, secret.ErrVault) {
		t.Errorf("signed out: %v", err)
	}
	tools.Signed = true
	ps, err := o.Fingerprints(context.Background(), ref)
	if err != nil || len(ps) != 1 {
		t.Fatalf("fingerprints = %v, %v", ps, err)
	}
	if len(tools.Tokens) != 0 {
		t.Errorf("op got a service account token: %q", tools.Tokens)
	}
	if _, err := o.Fingerprints(context.Background(), secret.Ref{Op: "op://Other/x/y"}); err == nil {
		t.Error("session mode reads outside secret.vault")
	}
	if _, err := o.Setup(context.Background(), "sa", filepath.Join(t.TempDir(), "tok")); !errors.Is(err, secret.ErrSetUp) {
		t.Errorf("setup in session mode = %v", err)
	}
}
