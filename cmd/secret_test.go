package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	secretValue = "planted-Secret-Value-9d41b7"
	dbRef       = "op://Shared/db/password"
	jsonFlag    = "--json"
	copyOp      = "copy"
)

// secretApp is an app over a scratch repository with one encrypted Secret
// and the fake sops and op.
func secretApp(t *testing.T) (*app, *secrettest.Tools, string) {
	t.Helper()
	tools := secrettest.New(map[string]string{dbRef: secretValue})
	prev := secretRun
	secretRun = tools.Run
	t.Cleanup(func() { secretRun = prev })
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".sops.yaml"), []byte("creation_rules: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plain := "apiVersion: v1\nkind: Secret\nmetadata:\n  name: db\n  namespace: team-a\ndata:\n  password: " + base64.StdEncoding.EncodeToString([]byte(secretValue)) + "\n"
	if err := os.WriteFile(filepath.Join(repo, "db.sops.yaml"), secrettest.Encrypt(plain), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	token := filepath.Join(dir, "sa-token")
	if err := os.WriteFile(token, []byte("sa-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{out: &bytes.Buffer{}, now: relayNow, as: "test: secret", store: store,
		cfg: &config.Config{StateDir: dir, Secret: config.Secret{Vault: "Shared", TokenFile: token}}}
	return a, tools, repo
}

func runSecret(a *app, args ...string) (string, error) {
	c := a.secretCmd()
	usageArgs(c)
	c.SetArgs(args)
	c.SetOut(a.out)
	c.SetErr(a.out)
	c.SilenceUsage, c.SilenceErrors = true, true
	err := c.ExecuteContext(context.Background())
	return a.out.(*bytes.Buffer).String(), err
}

// noSecret fails when the value, or its base64 form, is in s.
func noSecret(t *testing.T, what, s string) {
	t.Helper()
	if strings.Contains(s, secretValue) || strings.Contains(s, base64.StdEncoding.EncodeToString([]byte(secretValue))) {
		t.Errorf("%s carries the value:\n%s", what, s)
	}
}

func TestSecretOperationsReturnNoValueAndAreLogged(t *testing.T) {
	a, _, repo := secretApp(t)
	src := filepath.Join(repo, "db.sops.yaml")
	dst := filepath.Join(repo, "db-copy.sops.yaml")
	consumer := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(consumer, //nolint:gosec // an executable test consumer
		[]byte("#!/bin/sh\nread v\necho \"got $v\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{copyOp, src, dst, "--name", "db-copy", "--namespace", "team-b"},
		{"compare", src, dst},
		{"compare", src + "#data.password", dst + "#data.password"},
		{"fingerprint", src},
		{"fingerprint", dbRef},
		{copyOp, dbRef, dst + "#stringData.extra"},
		{copyOp, dbRef, "--", consumer, "secret", "set", "X"},
		{"set", dst, "stringData.generated", "--generate", "--vault", "op://Shared/gen/password"},
		{jsonFlag, copyOp, src, filepath.Join(repo, "json.sops.yaml")},
	} {
		a.out = &bytes.Buffer{}
		a.json = args[0] == jsonFlag
		if a.json {
			args = args[1:]
		}
		out, err := runSecret(a, args...)
		if err != nil && Code(err) != ExitError {
			t.Errorf("secret %s: %v", strings.Join(args, " "), err)
		}
		noSecret(t, "secret "+strings.Join(args, " "), fmt.Sprint(out, err))
	}
	a.out, a.json = &bytes.Buffer{}, false
	out, err := runSecret(a, copyOp, src, filepath.Join(repo, "x.sops.yaml"), "--name", "x")
	if err != nil || out != "wrote "+filepath.Join(repo, "x.sops.yaml")+": 1 keys\n  data.password"+strings.Repeat(" ", 45)+fmt.Sprintf(" %d bytes\n", len(secretValue)) {
		t.Errorf("copy answers %q, %v", out, err)
	}
	a.out = &bytes.Buffer{}
	if _, err := runSecret(a, "compare", src, dst); Code(err) != ExitError {
		t.Errorf("compare of files with different names = %v, want exit 1", err)
	}
	a.out = &bytes.Buffer{}
	if _, err := runSecret(a, copyOp, dbRef, "--", "cat"); Code(err) != ExitRefused {
		t.Errorf("copy to cat = %v, want refused", err)
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return strings.HasPrefix(e.Verb, "secret.") })
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 12 {
		t.Errorf("%d secret events, want 12", len(evs))
	}
	for _, e := range evs {
		noSecret(t, "the log", e.Detail)
		if e.By.Name != "test: secret" {
			t.Errorf("event by %+v", e.By)
		}
	}
}

func TestSecretRotateClosesTheRotationNotes(t *testing.T) {
	a, tools, repo := secretApp(t)
	src := filepath.Join(repo, "db.sops.yaml")
	a.cfg.Scan.SOPS = []string{filepath.Join(repo, "*.sops.yaml")}
	carrier := "Rotate sops://" + src + "#data.password: its value was in a Bash result."
	other := "Rotate op://Shared/other/password: its value was in a Bash result."
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Notes = []state.Note{{ID: 1, Text: "Rotate " + dbRef + ": its value was in a Bash result."}, {ID: 2, Text: carrier}, {ID: 3, Text: other}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	out, err := runSecret(a, "rotate", dbRef, "--generate")
	if err != nil {
		t.Fatal(err)
	}
	noSecret(t, "rotate", out)
	if !strings.HasPrefix(out, "rotated "+dbRef+": hmac:") || !strings.Contains(out, src+"#data.password (base64)") {
		t.Errorf("rotate answers %q", out)
	}
	if tools.Vault[dbRef] == secretValue {
		t.Error("the vault holds the old value")
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Notes) != 1 || st.Notes[0].Text != other {
		t.Errorf("open notes = %+v, want only #3", st.Notes)
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == "secret.rotate" || e.Verb == noteDone })
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 {
		t.Errorf("%d events, want the rotation and two notes done", len(evs))
	}
	for _, e := range evs {
		noSecret(t, "the log", e.Detail)
	}
	a.out = &bytes.Buffer{}
	if _, err := runSecret(a, "rotate", "platform://hazel/muster/x", "--generate"); Code(err) != ExitUsage {
		t.Errorf("--generate on a platform credential = %v, want usage", err)
	}
}
