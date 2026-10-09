package secret_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"filippo.io/age"

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

func TestImportAgeStoresOnlyTheRecipientsIdentity(t *testing.T) {
	want, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	recipient := want.Recipient().String()
	keysTxt := "# created: 2026-10-09T01:00:00Z\n# public key: " + recipient + "\n" + want.String() + "\n"
	const src, wrong = "op://Employee/lab.agekey/notesPlain", "op://Employee/other.agekey/notesPlain"
	tools := secrettest.New(map[string]string{src: keysTxt, wrong: "# public key: " + recipient + "\n" + other.String()})
	tools.Signed = true
	o := &secret.Ops{Run: tools.Run, Vault: shared, Session: true}
	dst, n, err := o.ImportAge(context.Background(), secret.Ref{Op: src}, recipient)
	if err != nil {
		t.Fatal(err)
	}
	item := secret.AgeItemRef(shared, recipient)
	if dst.Op != item || n != len(want.String()) || tools.Vault[item] != want.String() {
		t.Errorf("import = %s, %d bytes; the item holds %d bytes", dst, n, len(tools.Vault[item]))
	}
	if len(tools.Tokens) != 0 {
		t.Errorf("op got a service account token: %q", tools.Tokens)
	}
	// another key, even under a comment naming the recipient, is refused
	// before anything is written, and the refusal carries no value
	delete(tools.Vault, item)
	_, _, err = o.ImportAge(context.Background(), secret.Ref{Op: wrong}, recipient)
	if err == nil || !strings.Contains(err.Error(), recipient) || !strings.Contains(err.Error(), wrong) || strings.Contains(err.Error(), "AGE-SECRET-KEY") {
		t.Errorf("a wrong identity = %v", err)
	}
	if _, ok := tools.Vault[item]; ok {
		t.Error("a wrong identity was written")
	}
	for _, c := range tools.Calls {
		if strings.Contains(c, "AGE-SECRET-KEY") {
			t.Errorf("a command line carries an identity: %q", c)
		}
	}
	if _, _, err := o.ImportAge(context.Background(), secret.Ref{Op: src}, "age1nope"); err == nil {
		t.Error("an invalid recipient passes")
	}
}

// The item an import creates is op's Password template, which op's
// ItemValidator accepts: the built-in password field with its purpose, a
// built-in destination field (notesPlain) with its own purpose and type.
func TestImportCreatesAnItemOpsValidatorAccepts(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	recipient := id.Recipient().String()
	const src = "op://Employee/lab.agekey/notesPlain"
	tools := secrettest.New(map[string]string{src: id.String()})
	tools.Signed = true
	var created []map[string]any
	run := func(ctx context.Context, dir string, env []string, stdin io.Reader, name string, args ...string) ([]byte, error) {
		if name == "op" && len(args) > 1 && args[0] == "item" && args[1] == "create" {
			raw, err := io.ReadAll(stdin)
			if err != nil {
				return nil, err
			}
			var item map[string]any
			if err := json.Unmarshal(raw, &item); err != nil {
				t.Fatalf("item create got no JSON: %v", err)
			}
			created = append(created, item)
			stdin = bytes.NewReader(raw)
		}
		return tools.Run(ctx, dir, env, stdin, name, args...)
	}
	o := &secret.Ops{Run: run, Vault: shared, Session: true}
	if _, _, err := o.ImportAge(context.Background(), secret.Ref{Op: src}, recipient); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Import(context.Background(), secret.Ref{Op: src}, secret.Ref{Op: "op://Shared/lab notes/notesPlain"}); err != nil {
		t.Fatal(err)
	}
	if len(created) != 2 {
		t.Fatalf("items created: %d", len(created))
	}
	type want struct{ purpose, typ string }
	builtin := want{"PASSWORD", "CONCEALED"}
	for i, w := range []map[string]want{
		{"password": builtin},
		{"password": builtin, "notesPlain": {"NOTES", "STRING"}},
	} {
		item := created[i]
		if item["category"] != builtin.purpose {
			t.Errorf("item %d: category %v", i, item["category"])
		}
		fields, _ := item["fields"].([]any)
		got := map[string]want{}
		for _, f := range fields {
			m, _ := f.(map[string]any)
			p, _ := m["purpose"].(string)
			typ, _ := m["type"].(string)
			got[m["id"].(string)] = want{p, typ}
		}
		if !maps.Equal(got, w) {
			t.Errorf("item %d fields = %v, want %v", i, got, w)
		}
	}
	if created[0]["title"] != secret.AgeItemTitle(recipient) {
		t.Errorf("title = %v", created[0]["title"])
	}
	if tools.Vault[secret.AgeItemRef(shared, recipient)] != id.String() {
		t.Error("the recipient's item does not hold its identity")
	}
}

// An op refusal reaches the caller whole, past its first line, without the
// value op was given.
func TestStoreVaultRelaysOpsRefusalWhole(t *testing.T) {
	const src = "op://Employee/lab.key/password"
	tools := secrettest.New(map[string]string{src: password})
	tools.Signed = true
	const tail = "{1. a field the validator names at the very end of a long message}"
	run := func(ctx context.Context, dir string, env []string, stdin io.Reader, name string, args ...string) ([]byte, error) {
		if name == "op" && len(args) > 1 && args[0] == "item" && args[1] == "create" {
			return nil, errors.New("op: exit 1 ([ERROR] unable to process line 1: Validation: (validateVaultItem failed to Validate), " +
				"Couldn't validate the item: \"[ItemValidator] has found 1 errors, 0 warnings: Details: Errors: value " + password + " " + tail + "\")")
		}
		return tools.Run(ctx, dir, env, stdin, name, args...)
	}
	o := &secret.Ops{Run: run, Vault: shared, Session: true}
	_, err := o.Import(context.Background(), secret.Ref{Op: src}, secret.Ref{Op: "op://Shared/lab key/password"})
	if err == nil || !strings.Contains(err.Error(), tail) || !strings.HasPrefix(err.Error(), "op://Shared/lab key/password: op: exit 1") {
		t.Errorf("a refusal = %v", err)
	}
	if err != nil && strings.Contains(err.Error(), password) {
		t.Error("the refusal carries the value")
	}
}
