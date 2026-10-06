package secret_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

// The planted values: none may appear in an answer, an error or a file.
const (
	password = "planted-Pass-7c1d0e9b2a"
	token    = "planted-Token-55e3a1f0c8" //nolint:gosec // a planted test value
	vaultRef = "op://Shared/api/credential"
	pwPath   = "stringData.password"
	aFile    = "a.sops.yaml"
	shared   = "Shared"
	saToken  = "sa-token"
)

//nolint:gosec // a template of planted test values
const srcSecret = `apiVersion: v1
kind: Secret
metadata:
  name: app-credentials
  namespace: team-a
type: Opaque
stringData:
  password: %s
data:
  token: %s
`

// scratch is a repository with a .sops.yaml and the source Secret.
func scratch(t *testing.T) (dir, src string) {
	t.Helper()
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte("creation_rules: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src = filepath.Join(dir, "src.sops.yaml")
	plain := fmt.Sprintf(srcSecret, password, base64.StdEncoding.EncodeToString([]byte(token)))
	if err := os.WriteFile(src, secrettest.Encrypt(plain), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, src
}

func ops(tools *secrettest.Tools) *secret.Ops {
	return &secret.Ops{Run: tools.Run, Vault: shared, Token: saToken, Fingerprint: func(v string) string { return fmt.Sprintf("fp-%d", len(v)*7) }}
}

// noValue fails when any planted value is in what an operation answered.
func noValue(t *testing.T, what string, v any) {
	t.Helper()
	raw, _ := json.Marshal(v)
	s := fmt.Sprintf("%s %+v", raw, v)
	for _, p := range []string{password, token, base64.StdEncoding.EncodeToString([]byte(token)), base64.StdEncoding.EncodeToString([]byte(password))} {
		if strings.Contains(s, p) {
			t.Errorf("%s answers a value: %s", what, s)
		}
	}
}

// decrypted reads a file the fake encrypted.
func decrypted(t *testing.T, tools *secrettest.Tools, file string) string {
	t.Helper()
	out, err := tools.Run(context.Background(), "", nil, nil, "sops", "decrypt", "--output-type", "yaml", file)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestCopyFileRewritesTheMetadataAndAnswersKeysAndLengths(t *testing.T) {
	tools := secrettest.New(nil)
	dir, src := scratch(t)
	dst := filepath.Join(dir, "other", "dst.sops.yaml")
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	keys, err := ops(tools).CopyFile(context.Background(), secret.Ref{File: src}, dst, "app-copy", "team-b")
	if err != nil {
		t.Fatal(err)
	}
	want := []secret.Key{{Name: "data.token", Bytes: len(token)}, {Name: pwPath, Bytes: len(password)}}
	if !slices.Equal(keys, want) {
		t.Errorf("keys = %+v, want %+v", keys, want)
	}
	noValue(t, "copy", keys)
	raw, err := os.ReadFile(dst) //nolint:gosec // the test's own scratch file
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), password) {
		t.Fatal("the copy holds a value in plaintext")
	}
	plain := decrypted(t, tools, dst)
	for _, w := range []string{"name: app-copy", "namespace: team-b", "password: " + password, "kind: Secret"} {
		if !strings.Contains(plain, w) {
			t.Errorf("the copy lacks %q:\n%s", w, plain)
		}
	}
	// The encryption ran where the creation rules are, the path relative.
	if !slices.ContainsFunc(tools.Calls, func(c string) bool {
		return strings.Contains(c, "--filename-override "+filepath.Join("other", "dst.sops.yaml"))
	}) {
		t.Errorf("calls = %q", tools.Calls)
	}
	entries, _ := os.ReadDir(filepath.Dir(dst))
	if len(entries) != 1 {
		t.Errorf("the copy left more than its file: %v", entries)
	}
}

func TestCopyFileRefusesAnExistingDestinationAndAnObjectWithoutMetadata(t *testing.T) {
	tools := secrettest.New(nil)
	dir, src := scratch(t)
	if _, err := ops(tools).CopyFile(context.Background(), secret.Ref{File: src}, src, "x", ""); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Errorf("copy onto itself = %v", err)
	}
	plain := filepath.Join(dir, "plain.sops.yaml")
	if err := os.WriteFile(plain, secrettest.Encrypt("password: "+password+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ops(tools).CopyFile(context.Background(), secret.Ref{File: plain}, filepath.Join(dir, "n.sops.yaml"), "x", "")
	if err == nil || !strings.Contains(err.Error(), "Kubernetes object") {
		t.Errorf("rename of a plain file = %v", err)
	}
	noValue(t, "the refusal", err)
}

func TestCopyFileNeedsCreationRules(t *testing.T) {
	tools := secrettest.New(nil)
	_, src := scratch(t)
	_, err := ops(tools).CopyFile(context.Background(), secret.Ref{File: src}, filepath.Join(t.TempDir(), "x.sops.yaml"), "", "")
	if err == nil || !strings.Contains(err.Error(), ".sops.yaml") {
		t.Errorf("copy outside a .sops.yaml = %v", err)
	}
}

func TestCompareAnswersPerKey(t *testing.T) {
	tools := secrettest.New(map[string]string{vaultRef: password})
	dir, src := scratch(t)
	dst := filepath.Join(dir, "dst.sops.yaml")
	o := ops(tools)
	ctx := context.Background()
	if _, err := o.CopyFile(ctx, secret.Ref{File: src}, dst, "app-copy", ""); err != nil {
		t.Fatal(err)
	}
	vs, err := o.Compare(ctx, secret.Ref{File: src}, secret.Ref{File: dst})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, v := range vs {
		got[v.Key] = v.State
	}
	if got[pwPath] != secret.Equal || got["data.token"] != secret.Equal || got["metadata.name"] != secret.Different || got["metadata.namespace"] != secret.Equal {
		t.Errorf("compare = %+v", vs)
	}
	noValue(t, "compare", vs)
	one, err := o.Compare(ctx, secret.Ref{Op: vaultRef}, secret.Ref{File: src, Path: pwPath})
	if err != nil || len(one) != 1 || one[0].State != secret.Equal {
		t.Errorf("compare vault with SOPS path = %+v, %v", one, err)
	}
	if _, err := o.Compare(ctx, secret.Ref{Op: vaultRef}, secret.Ref{File: src}); err == nil {
		t.Error("compare of a value with a whole file passes")
	}
}

func TestFingerprintsAreKeyedAndNameNoValue(t *testing.T) {
	tools := secrettest.New(nil)
	_, src := scratch(t)
	ps, err := ops(tools).Fingerprints(context.Background(), secret.Ref{File: src, Path: pwPath})
	if err != nil || len(ps) != 1 || ps[0].Fingerprint != fmt.Sprintf("fp-%d", len(password)*7) {
		t.Fatalf("fingerprints = %+v, %v", ps, err)
	}
	noValue(t, "fingerprint", ps)
}

func TestOnlyTheSharedVault(t *testing.T) {
	tools := secrettest.New(map[string]string{otherVaultRef: password})
	o := ops(tools)
	if _, err := o.Fingerprints(context.Background(), secret.Ref{Op: otherVaultRef}); err == nil || !strings.Contains(err.Error(), "only the shared vault") {
		t.Errorf("another vault = %v", err)
	}
	o.Token = ""
	if _, err := o.Fingerprints(context.Background(), secret.Ref{Op: vaultRef}); err == nil || !strings.Contains(err.Error(), "secret.tokenFile") {
		t.Errorf("no service account = %v", err)
	}
	o.Vault = ""
	if _, err := o.Fingerprints(context.Background(), secret.Ref{Op: vaultRef}); err == nil || !strings.Contains(err.Error(), "secret.vault") {
		t.Errorf("no vault configured = %v", err)
	}
	if len(tools.Calls) != 0 {
		t.Errorf("op ran: %q", tools.Calls)
	}
}

func TestCopyValueIntoASOPSPathKeepsTheOthers(t *testing.T) {
	tools := secrettest.New(map[string]string{vaultRef: token})
	dir, src := scratch(t)
	o := ops(tools)
	ctx := context.Background()
	n, err := o.CopyValue(ctx, secret.Ref{Op: vaultRef}, secret.Ref{File: src, Path: "stringData.apiToken"})
	if err != nil || n != len(token) {
		t.Fatalf("copy = %d, %v", n, err)
	}
	plain := decrypted(t, tools, src)
	if !strings.Contains(plain, "apiToken: "+token) || !strings.Contains(plain, "password: "+password) {
		t.Errorf("the file after the copy:\n%s", plain)
	}
	fresh := filepath.Join(dir, "new.sops.yaml")
	if _, err := o.CopyValue(ctx, secret.Ref{File: src, Path: pwPath}, secret.Ref{File: fresh, Path: "a.b"}); err != nil {
		t.Fatal(err)
	}
	if plain := decrypted(t, tools, fresh); plain != "a:\n  b: "+password+"\n" {
		t.Errorf("the new file:\n%s", plain)
	}
}

func TestConsumerAllowList(t *testing.T) {
	for argv, ok := range map[string]bool{
		"gh secret set TOKEN --repo o/r":                   true,
		"docker login ghcr.example --password-stdin -u me": true,
		"tool build --secret id=-":                         true,
		"tool build --secret=id=-":                         true,
		catCmd:                                             false,
		"sh -c cat":                                        false,
		"tee /tmp/x":                                       false,
		"gh secret list":                                   false,
		"tool --secret id=file":                            false,
	} {
		if err := secret.Consumer(strings.Fields(argv)); (err == nil) != ok {
			t.Errorf("Consumer(%q) = %v, want allowed %v", argv, err, ok)
		}
	}
}

func TestCopyToConsumerRedactsItsOutput(t *testing.T) {
	tools := secrettest.New(map[string]string{vaultRef: token})
	dir := t.TempDir()
	// A consumer that echoes what it read: its output is redacted.
	gh := filepath.Join(dir, "gh")
	if err := os.WriteFile(gh, //nolint:gosec // an executable test consumer
		[]byte("#!/bin/sh\nread v\necho \"stored $v\"\nexit 3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	code, out, err := ops(tools).CopyToConsumer(context.Background(), secret.Ref{Op: vaultRef}, []string{gh, secretWord, setWord, "X"}, secret.Stdin{})
	if err != nil || code != 3 {
		t.Fatalf("consumer = %d, %q, %v", code, out, err)
	}
	if strings.Contains(out, token) || !strings.Contains(out, "[redacted: "+vaultRef+"]") {
		t.Errorf("output = %q", out)
	}
	if _, _, err := ops(tools).CopyToConsumer(context.Background(), secret.Ref{Op: vaultRef}, []string{catCmd}, secret.Stdin{}); err == nil {
		t.Error("cat took a value")
	}
}

func TestSetWritesTheVaultFirst(t *testing.T) {
	tools := secrettest.New(nil)
	dir, _ := scratch(t)
	dst := secret.Ref{File: filepath.Join(dir, "gen.sops.yaml"), Path: pwPath}
	res, err := ops(tools).Set(context.Background(), dst, secret.SetOptions{Vault: secret.Ref{Op: "op://Shared/app/password"}, Length: 24, Charset: alnumSet})
	if err != nil {
		t.Fatal(err)
	}
	v := tools.Vault["op://Shared/app/password"]
	if len(v) != 24 || res.Fingerprint != fmt.Sprintf("fp-%d", 24*7) {
		t.Fatalf("vault value of %d bytes, fingerprint %q", len(v), res.Fingerprint)
	}
	if slices.ContainsFunc(tools.Tokens, func(s string) bool { return s != "sa-token" }) {
		t.Errorf("op ran as %q, not the service account", tools.Tokens)
	}
	if !strings.Contains(decrypted(t, tools, dst.File), "password: "+v) {
		t.Error("the SOPS file does not hold the vault's value")
	}
	vaultCall := slices.IndexFunc(tools.Calls, func(c string) bool { return strings.HasPrefix(c, "op item create") })
	sopsCall := slices.IndexFunc(tools.Calls, func(c string) bool { return strings.HasPrefix(c, "sops --config") })
	if vaultCall < 0 || sopsCall < vaultCall {
		t.Errorf("calls = %q: the vault is written first", tools.Calls)
	}
	for _, c := range tools.Calls {
		if strings.Contains(c, v) {
			t.Errorf("a command line carries the value: %q", c)
		}
	}
	// A second set edits the item it made.
	if _, err := ops(tools).Set(context.Background(), dst, secret.SetOptions{Vault: secret.Ref{Op: "op://Shared/app/password"}, Length: 24, Charset: hexSet}); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(tools.Calls, func(c string) bool { return strings.HasPrefix(c, "op item edit id-app") }) {
		t.Errorf("calls = %q", tools.Calls)
	}
	if _, err := ops(tools).Set(context.Background(), dst, secret.SetOptions{Vault: secret.Ref{Op: "op://Other/app/password"}, Length: 24, Charset: hexSet}); err == nil {
		t.Error("set into another vault passes")
	}
}

func TestSetWithoutAVaultWritesTheSOPSPathAlone(t *testing.T) {
	tools := secrettest.New(nil)
	dir, _ := scratch(t)
	dst := secret.Ref{File: filepath.Join(dir, "gen.sops.yaml"), Path: pwPath}
	o := ops(tools)
	var applied string
	o.Apply = func(_ context.Context, _ []byte, _ secret.KubeTarget, v []byte) error {
		applied = string(v)
		return nil
	}
	tg := secret.KubeTarget{Context: labContext, Namespace: "garage", Name: "s3", Key: "secret"}
	res, err := o.Set(context.Background(), dst, secret.SetOptions{Length: 40, Charset: hexSet, Secret: &tg})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Vault) != 0 || slices.ContainsFunc(tools.Calls, func(c string) bool { return strings.HasPrefix(c, "op ") }) {
		t.Errorf("op ran without a vault: %q", tools.Calls)
	}
	if len(applied) != 40 || !strings.Contains(decrypted(t, tools, dst.File), "password: "+applied) {
		t.Errorf("the Secret and the SOPS path differ, or the value is not 40 characters")
	}
	if res.Key != dst.String() || res.Fingerprint != fmt.Sprintf("fp-%d", 40*7) {
		t.Errorf("result %+v", res)
	}
	for _, c := range tools.Calls {
		if strings.Contains(c, applied) {
			t.Errorf("a command line carries the value: %q", c)
		}
	}
}

func TestSetFeedsAConsumerAfterTheSOPSPath(t *testing.T) {
	tools := secrettest.New(nil)
	dir, _ := scratch(t)
	dst := secret.Ref{File: filepath.Join(dir, "gen.sops.yaml"), Path: pwPath}
	gh := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(gh, //nolint:gosec // an executable test consumer
		[]byte("#!/bin/sh\nread v\necho \"stored $v\"\nexit 2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	res, err := ops(tools).Set(context.Background(), dst, secret.SetOptions{Length: 24, Charset: alnumSet, Consumer: []string{gh, secretWord, setWord, "X"}})
	if err != nil || res.Code != 2 {
		t.Fatalf("set = %+v, %v", res, err)
	}
	if res.Output != "stored [redacted: "+dst.String()+"]\n" {
		t.Errorf("output = %q", res.Output)
	}
	// A refused consumer and a lab-less context draw no value at all.
	for _, opt := range []secret.SetOptions{
		{Length: 24, Charset: alnumSet, Consumer: []string{catCmd}},
		{Length: 24, Charset: alnumSet, Secret: &secret.KubeTarget{Context: gazelleContext, Namespace: "x", Name: "y", Key: "z"}},
	} {
		calls := len(tools.Calls)
		if _, err := ops(tools).Set(context.Background(), dst, opt); err == nil || len(tools.Calls) != calls {
			t.Errorf("set %+v = %v, ran %q", opt, err, tools.Calls[calls:])
		}
	}
}

func TestParseRef(t *testing.T) {
	for in, want := range map[string]secret.Ref{
		"a.sops.yaml":                {File: "a.sops.yaml"},
		"a.sops.yaml#data.x":         {File: aFile, Path: "data.x"},
		"sops://a.sops.yaml#data.x":  {File: aFile, Path: "data.x"},
		"op://Shared/item/field":     {Op: "op://Shared/item/field"},
		"op://Shared/item/sec/field": {Op: "op://Shared/item/sec/field"},
	} {
		if got, err := secret.ParseRef(in); err != nil || got != want {
			t.Errorf("ParseRef(%q) = %+v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"op://Shared/item", "#x", "op:///a/b"} {
		if _, err := secret.ParseRef(bad); err == nil {
			t.Errorf("ParseRef(%q) passes", bad)
		}
	}
}

const (
	s3Key = "stringData.secretKey"
	appNS = "app"
)

//nolint:gosec // a Secret skeleton, no value in it
const skeletonSecret = `apiVersion: v1
kind: Secret
metadata:
  name: app-s3
  namespace: app
type: Opaque
stringData:
`

func TestSetFillsAPlaintextSecretSkeleton(t *testing.T) {
	tools := secrettest.New(nil)
	dir, _ := scratch(t)
	file := filepath.Join(dir, "secret-s3.sops.yaml")
	if err := os.WriteFile(file, []byte(skeletonSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := secret.Ref{File: file, Path: s3Key}
	if _, err := ops(tools).Set(context.Background(), dst, secret.SetOptions{Length: 32, Charset: alnumSet}); err != nil {
		t.Fatal(err)
	}
	plain := decrypted(t, tools, file)
	for _, w := range []string{"kind: Secret", "name: app-s3", "namespace: app", "type: Opaque", "stringData:\n  secretKey: "} {
		if !strings.Contains(plain, w) {
			t.Errorf("the filled skeleton lacks %q:\n%s", w, plain)
		}
	}
	raw, err := os.ReadFile(file) //nolint:gosec // the test's scratch file
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "kind: Secret") {
		t.Error("the file stayed plaintext")
	}
	// A second set decrypts the SOPS file it became.
	if _, err := ops(tools).Set(context.Background(), secret.Ref{File: file, Path: "stringData.second"}, secret.SetOptions{Length: 16, Charset: hexSet}); err != nil {
		t.Fatal(err)
	}
	if plain := decrypted(t, tools, file); !strings.Contains(plain, "secretKey: ") || !strings.Contains(plain, "second: ") {
		t.Errorf("after a second set:\n%s", plain)
	}
}

func TestSetRefusesAPlaintextSecretHoldingAValue(t *testing.T) {
	tools := secrettest.New(nil)
	dir, _ := scratch(t)
	file := filepath.Join(dir, "plain.sops.yaml")
	body := skeletonSecret + "  password: " + password + "\n"
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ops(tools).Set(context.Background(), secret.Ref{File: file, Path: s3Key}, secret.SetOptions{Length: 32, Charset: alnumSet})
	if err == nil || !strings.Contains(err.Error(), "stringData.password") || strings.Contains(err.Error(), password) {
		t.Fatalf("set into a plaintext Secret with a value = %v", err)
	}
	if raw, _ := os.ReadFile(file); string(raw) != body { //nolint:gosec // the test's scratch file
		t.Error("the refused file changed")
	}
}

func TestSetStartsANewSecret(t *testing.T) {
	tools := secrettest.New(nil)
	dir, src := scratch(t)
	file := filepath.Join(dir, "new.sops.yaml")
	nw := &secret.NewSecret{Name: "app-s3", Namespace: appNS}
	if _, err := ops(tools).Set(context.Background(), secret.Ref{File: file, Path: s3Key}, secret.SetOptions{Length: 32, Charset: alnumSet, New: nw}); err != nil {
		t.Fatal(err)
	}
	plain := decrypted(t, tools, file)
	if !strings.HasPrefix(plain, "apiVersion: v1\nkind: Secret\nmetadata:\n  name: app-s3\n  namespace: app\ntype: Opaque\nstringData:\n  secretKey: ") {
		t.Errorf("the new Secret:\n%s", plain)
	}
	for _, bad := range []struct {
		file string
		nw   secret.NewSecret
	}{
		{src, *nw},
		{filepath.Join(dir, "x.sops.yaml"), secret.NewSecret{Name: "Bad_Name", Namespace: appNS}},
		{filepath.Join(dir, "x.sops.yaml"), secret.NewSecret{Name: "ok", Namespace: "no.dots"}},
	} {
		calls := len(tools.Calls)
		if _, err := ops(tools).Set(context.Background(), secret.Ref{File: bad.file, Path: "stringData.k"}, secret.SetOptions{Length: 32, Charset: alnumSet, New: &bad.nw}); err == nil || len(tools.Calls) != calls {
			t.Errorf("set %s as %+v = %v", bad.file, bad.nw, err)
		}
	}
}

func TestConsumerKubectlExec(t *testing.T) {
	const pod = "kubectl exec -i --context kind-lab -n garage garage-0 -- "
	for argv, ok := range map[string]bool{
		pod + "/garage json-api ImportKey -":                             true,
		pod + "garage json-api ImportKey -":                              true,
		"kubectl exec --stdin --context=kind-lab pod -- gh secret set X": true,
		pod + "tool --password-stdin":                                    true,
		"garage json-api ImportKey -":                                    true,
		pod + "/garage key import GK1 secret":                            false,
		pod + "garage json-api ImportKey {}":                             false,
		pod + "cat":                                                      false,
		pod:                                                              false,
		"kubectl exec --context kind-lab pod -- garage json-api ImportKey -":         false,
		"kubectl exec -i pod -- garage json-api ImportKey -":                         false,
		"kubectl exec -it --context kind-lab pod -- garage json-api ImportKey -":     false,
		"kubectl exec -i -t --context kind-lab pod -- garage json-api ImportKey -":   false,
		"kubectl exec -i -v=9 --context kind-lab pod -- garage json-api ImportKey -": false,
		"kubectl apply -i --context kind-lab -f - -- garage json-api ImportKey -":    false,
		"kubectl exec -i --context kind-lab pod garage json-api ImportKey -":         false,
	} {
		if err := secret.Consumer(strings.Fields(argv)); (err == nil) != ok {
			t.Errorf("Consumer(%q) = %v, want allowed %v", argv, err, ok)
		}
	}
	if c := secret.ConsumerContext(strings.Fields(pod + "x")); c != "kind-lab" {
		t.Errorf("ConsumerContext = %q", c)
	}
	if c := secret.ConsumerContext([]string{"gh", "secret", "set", "--context", "x"}); c != "" {
		t.Errorf("ConsumerContext of gh = %q", c)
	}
}

func TestSetHandsAPodAJSONRequestOnStdin(t *testing.T) {
	tools := secrettest.New(nil)
	dir, _ := scratch(t)
	file := filepath.Join(dir, "secret-s3.sops.yaml")
	if err := os.WriteFile(file, []byte(skeletonSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	// A fake kubectl that keeps its stdin and argv aside and echoes stdin:
	// the echo comes back redacted.
	bin := t.TempDir()
	kubectl := filepath.Join(bin, "kubectl")
	if err := os.WriteFile(kubectl, //nolint:gosec // an executable test consumer
		[]byte("#!/bin/sh\ncat > \""+bin+"/stdin\"\necho \"$@\" > \""+bin+"/argv\"\ncat \""+bin+"/stdin\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	argv := []string{kubectl, "exec", "-i", "--context", "kind-lab", "-n", "garage", "garage-0", "--", "/garage", "json-api", "ImportKey", "-"}
	in := secret.Stdin{Template: `{"accessKeyId":"GK0123","name":"app"}`, Field: "secretAccessKey"}
	res, err := ops(tools).Set(context.Background(), secret.Ref{File: file, Path: s3Key}, secret.SetOptions{Length: 32, Charset: alnumSet, Consumer: argv, Stdin: in})
	if err != nil || res.Code != 0 {
		t.Fatalf("set = %+v, %v", res, err)
	}
	raw, err := os.ReadFile(filepath.Join(bin, "stdin")) //nolint:gosec // the test's scratch file
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]string
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("stdin is no JSON object: %v", err)
	}
	v := req["secretAccessKey"]
	if req["accessKeyId"] != "GK0123" || req["name"] != "app" || len(v) != 32 {
		t.Fatalf("request keys %d, accessKeyId %q, value of %d bytes", len(req), req["accessKeyId"], len(v))
	}
	if !strings.Contains(decrypted(t, tools, file), "secretKey: "+v) {
		t.Error("the pod got another value than the SOPS file holds")
	}
	if args, _ := os.ReadFile(filepath.Join(bin, "argv")); strings.Contains(string(args), v) { //nolint:gosec // the test's scratch file
		t.Error("the value is on the consumer's argv")
	}
	if strings.Contains(res.Output, v) || !strings.Contains(res.Output, "[redacted: ") {
		t.Errorf("output not redacted (%d bytes)", len(res.Output))
	}
	// A bad template draws no value.
	for _, bad := range []secret.Stdin{{Template: "[]", Field: "k"}, {Template: `{"k":1}`, Field: "k"}, {Template: "{}"}} {
		calls := len(tools.Calls)
		if _, err := ops(tools).Set(context.Background(), secret.Ref{File: file, Path: "stringData.k"}, secret.SetOptions{Length: 32, Charset: alnumSet, Consumer: argv, Stdin: bad}); err == nil || len(tools.Calls) != calls {
			t.Errorf("stdin %+v = %v", bad, err)
		}
	}
}
