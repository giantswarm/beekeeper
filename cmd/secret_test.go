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
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/sandbox"
	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	secretValue   = "planted-Secret-Value-9d41b7"
	dbRef         = "op://Shared/db/password"
	jsonFlag      = "--json"
	copyOp        = "copy"
	compareOp     = "compare"
	fingerprintOp = "fingerprint"
	sopsA         = "a.sops.yaml"
	sopsB         = "b.sops.yaml"
	generateFlag  = "--generate"
	ghSecret      = "secret"
	setOp         = "set"
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
		{compareOp, src, dst},
		{compareOp, src + "#data.password", dst + "#data.password"},
		{fingerprintOp, src},
		{fingerprintOp, dbRef},
		{copyOp, dbRef, dst + "#stringData.extra"},
		{copyOp, dbRef, "--", consumer, ghSecret, setOp, "X"},
		{setOp, dst, "stringData.generated", generateFlag, "--vault", "op://Shared/gen/password"},
		{setOp, dst, "stringData.local", generateFlag},
		{setOp, dst, "stringData.fed", generateFlag, "--", consumer, ghSecret, setOp, "X"},
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
	if _, err := runSecret(a, compareOp, src, dst); Code(err) != ExitError {
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
	if len(evs) != 14 {
		t.Errorf("%d secret events, want 14", len(evs))
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
	out, err := runSecret(a, "rotate", dbRef, generateFlag)
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
	if _, err := runSecret(a, "rotate", "platform://hazel/muster/x", generateFlag); Code(err) != ExitUsage {
		t.Errorf("--generate on a platform credential = %v, want usage", err)
	}
}

func TestSecretCopyToSecretOnlyIntoAHeldLab(t *testing.T) {
	a, _, repo := secretApp(t)
	a.cfg.LeaseDir = t.TempDir()
	a.cfg.Resources = []string{labOne, labTwo}
	a.cfg.Labs = map[string]string{labOne: labCluster, labTwo: labTwo}
	var applied []string
	prev := secretApply
	secretApply = func(_ context.Context, _ []byte, tg secret.KubeTarget, v []byte) error {
		applied = append(applied, fmt.Sprintf("%s %d", tg, len(v)))
		return nil
	}
	t.Cleanup(func() { secretApply = prev })
	if _, err := lease.Dir(a.cfg.LeaseDir).Claim(labOne, lease.Holder{Env: labOne, Name: a.as}); err != nil {
		t.Fatal(err)
	}
	if _, err := lease.Dir(a.cfg.LeaseDir).Claim(labTwo, lease.Holder{Env: labTwo, Name: "another session"}); err != nil {
		t.Fatal(err)
	}
	const key = "kagent/kagent-anthropic/ANTHROPIC_API_KEY"
	out, err := runSecret(a, copyOp, dbRef, "--to-secret", "kind-agentlab/"+key)
	if err != nil || out != fmt.Sprintf("wrote kind-agentlab/%s: %d bytes\n", key, len(secretValue)) {
		t.Errorf("copy into the held lab answers %q, %v", out, err)
	}
	for _, ctx := range []string{"kind-agentlab-2", "kind-aps-282", "teleport.giantswarm.io-gazelle"} {
		a.out = &bytes.Buffer{}
		out, err := runSecret(a, copyOp, dbRef, "--to-secret", ctx+"/"+key)
		if Code(err) != ExitRefused {
			t.Errorf("copy into %s = %q, %v, want refused", ctx, out, err)
		}
		noSecret(t, "the refusal", fmt.Sprint(out, err))
	}
	gen := filepath.Join(repo, "gen.sops.yaml")
	a.out = &bytes.Buffer{}
	if out, err := runSecret(a, setOp, gen, "data.key", generateFlag, "--length", "20", "--to-secret", "kind-agentlab-2/"+key); Code(err) != ExitRefused {
		t.Errorf("set into a lab held by another = %q, %v, want refused", out, err)
	}
	if _, err := os.Stat(gen); err == nil {
		t.Error("a refused set wrote the SOPS file")
	}
	a.out = &bytes.Buffer{}
	out, err = runSecret(a, setOp, gen, "data.key", generateFlag, "--length", "20", "--to-secret", "kind-agentlab/"+key)
	if err != nil || !strings.HasPrefix(out, "wrote "+gen+"#data.key and kind-agentlab/"+key+": 20 characters, hmac:") {
		t.Errorf("set into the held lab answers %q, %v", out, err)
	}
	if len(applied) != 2 || applied[0] != fmt.Sprintf("kind-agentlab/%s %d", key, len(secretValue)) || applied[1] != "kind-agentlab/"+key+" 20" {
		t.Errorf("applied %q", applied)
	}
	a.out = &bytes.Buffer{}
	if _, err := runSecret(a, copyOp, "op://Shared/absent/field", "--to-secret", "kind-agentlab/"+key); Code(err) != ExitVault {
		t.Errorf("a value the vault cannot give = %v (exit %d), want exit %d", err, Code(err), ExitVault)
	}
	a.cfg.Secret.TokenFile = ""
	a.out = &bytes.Buffer{}
	if _, err := runSecret(a, fingerprintOp, dbRef); Code(err) != ExitVault {
		t.Errorf("no vault token = %v (exit %d), want exit %d", err, Code(err), ExitVault)
	}
}

func TestBrokeredSecretHoldsItsFilesToTheSandbox(t *testing.T) {
	a, _, repo := secretApp(t)
	// a home outside the temporary directory, which the sandbox opens, and
	// the requester's working directory in the repository
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".sops.yaml"), []byte("creation_rules: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", filepath.Join(base, "tmp"))
	t.Chdir(repo)
	t.Setenv(sandbox.Env, "1")
	t.Setenv(sandbox.Brokered, "1")
	closed := filepath.Join(home, "credentials.sops.yaml")
	if err := os.WriteFile(closed, secrettest.Encrypt("token: "+secretValue+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(repo, "db.sops.yaml")
	for _, args := range [][]string{
		{fingerprintOp, closed},
		{fingerprintOp, filepath.Join(a.cfg.StateDir, "scan", "planted.sops.yaml")},
		{compareOp, src, closed},
		{copyOp, closed + "#token", filepath.Join(repo, "x.sops.yaml") + "#token"},
		{copyOp, src, filepath.Join(home, "out.sops.yaml")},
		{copyOp, src + "#data.password", filepath.Join(home, "out.sops.yaml") + "#p"},
	} {
		a.out = &bytes.Buffer{}
		out, err := runSecret(a, args...)
		if Code(err) != ExitRefused || !strings.Contains(err.Error(), "agent sandbox does not let") {
			t.Errorf("%q: exit %d, %v", args, Code(err), err)
		}
		noSecret(t, "a refused call", out)
	}
	a.out = &bytes.Buffer{}
	if _, err := runSecret(a, copyOp, src, filepath.Join(repo, "copy.sops.yaml")); err != nil {
		t.Errorf("a copy inside the sandbox's lists: %v", err)
	}
}

func TestSecretSetNewSecretAndPodConsumers(t *testing.T) {
	a, tools, repo := secretApp(t)
	a.cfg.Kube.Production = "gazelle"
	file := filepath.Join(repo, "s3.sops.yaml")
	if _, err := runSecret(a, setOp, file, "stringData.secretKey", generateFlag, "--name", "app-s3", "--namespace", "app"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(file) //nolint:gosec // the test's scratch file
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), "FAKESOPS") || len(tools.Calls) == 0 {
		t.Fatalf("set with --name wrote no SOPS file")
	}
	pod := []string{"kubectl", "exec", "-i", "--context", "teleport.example.io-gazelle", "garage-0", "--", "/garage", "json-api", "ImportKey", "-"}
	for _, c := range []struct {
		args []string
		code int
	}{
		{append([]string{setOp, file, "stringData.x", generateFlag, "--"}, pod...), ExitRefused},
		{append([]string{copyOp, file + "#stringData.secretKey", "--"}, pod...), ExitRefused},
		{[]string{setOp, file, "stringData.x", generateFlag, "--stdin-json", "{}", "--stdin-field", "k"}, ExitUsage},
		{[]string{setOp, filepath.Join(repo, "y.sops.yaml"), "stringData.x", generateFlag, "--name", "y"}, ExitUsage},
	} {
		a.out = &bytes.Buffer{}
		calls := len(tools.Calls)
		if _, err := runSecret(a, c.args...); Code(err) != c.code || len(tools.Calls) != calls {
			t.Errorf("secret %s = %v, want exit %d and no tool run", strings.Join(c.args, " "), err, c.code)
		}
	}
}
