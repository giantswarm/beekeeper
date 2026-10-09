package secret_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

// The shapes a Secret's key may hold in the tests.
const (
	dotenvDoc = "# the app's credentials\nexport CLIENT_ID=\"app-123\"\nCLIENT_SECRET='" + password + "' # the secret\n\nEMPTY=\nURL=https://idp.example.com/auth?scope=openid\nESCAPED=\"a \\\"quoted\\\" word\"\n"
	nestedDoc = "config.yaml: |\n  oidc:\n    clientID: app-123\n    clientSecret: " + password + "\n  issuer: https://idp.example.com\nvalues.env: |\n  TOKEN=" + token + "\n  MODE=dev\nname: app\n"
	jsonDoc   = `{"clientID": "app-123", "clientSecret": "` + password + `"}`
	listDoc   = "- " + password + "\n- " + token + "\n"
	textDoc   = "clientID: app-123\n  clientSecret: " + password + "\n"
	paddedDoc = "c2VjcmV0cGFzcw="
	tokenKey  = "Iv1.0123456789abcdefABCDEF0123456789"
)

// readingKube is an Ops whose Secret read answers raw.
func readingKube(raw []byte) *secret.Ops {
	o := ops(secrettest.New(nil))
	o.Read = func(context.Context, []byte, secret.KubeTarget) ([]byte, error) { return raw, nil }
	return o
}

var shapeTarget = secret.KubeTarget{Context: labContext, Namespace: kagentNS, Name: oauthName, Key: configKey}

// TestKubePathResolvesEveryShape: a path reaches a dotenv line's value
// (export and quotes stripped, comments ignored), a value of the YAML
// mapping or dotenv lines a block scalar holds one level down, a JSON
// mapping's value, and the block scalar itself.
func TestKubePathResolvesEveryShape(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, raw, path, want string
	}{
		{"dotenv quoted", dotenvDoc, "CLIENT_SECRET", password},
		{"dotenv exported", dotenvDoc, "CLIENT_ID", "app-123"},
		{"dotenv empty", dotenvDoc, "EMPTY", ""},
		{"dotenv url", dotenvDoc, "URL", "https://idp.example.com/auth?scope=openid"},
		{"dotenv escaped", dotenvDoc, "ESCAPED", `a "quoted" word`},
		{"block scalar mapping", nestedDoc, "config.yaml.oidc.clientSecret", password},
		{"block scalar dotenv", nestedDoc, "values.env.TOKEN", token},
		{"block scalar itself", nestedDoc, "values.env", "TOKEN=" + token + "\nMODE=dev\n"},
		{"json", jsonDoc, clientSecretPath, password},
		{"yaml", connectorDoc, clientSecretPath, password},
	} {
		o := readingKube([]byte(tc.raw))
		ps, err := o.Fingerprints(ctx, secret.Ref{Kube: shapeTarget, Path: tc.path})
		if err != nil || len(ps) != 1 || ps[0].Fingerprint != o.Fingerprint(tc.want) {
			t.Errorf("%s: fingerprint = %+v, %v, want the one of %d bytes", tc.name, ps, err, len(tc.want))
		}
		noValue(t, tc.name, ps)
	}
}

// TestKubePathCopiesADotenvValue copies one dotenv line's value into a
// SOPS file: the file holds the value, the answer its length.
func TestKubePathCopiesADotenvValue(t *testing.T) {
	_, dst := scratch(t)
	tools := secrettest.New(nil)
	o := ops(tools)
	o.Read = func(context.Context, []byte, secret.KubeTarget) ([]byte, error) { return []byte(dotenvDoc), nil }
	n, err := o.CopyValue(context.Background(), secret.Ref{Kube: shapeTarget, Path: "CLIENT_SECRET"}, secret.Ref{File: dst, Path: "stringData.clientSecret"})
	if err != nil || n != len(password) {
		t.Fatalf("copy = %d, %v", n, err)
	}
	if plain := decrypted(t, tools, dst); !strings.Contains(plain, "clientSecret: "+password) {
		t.Errorf("the file lacks the value:\n%s", plain)
	}
}

// TestKubePathRefusalNamesTheShape: a path that reaches nothing fails in
// one line with the shape of what the key, or the node on the way, holds:
// the format, the keys, the lines and the bytes, and never a value.
func TestKubePathRefusalNamesTheShape(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, raw, path string
		want           []string
	}{
		{"dotenv", dotenvDoc, clientSecretPath, []string{"no value at clientSecret: the key holds dotenv lines with the keys CLIENT_ID, CLIENT_SECRET, EMPTY, URL, ESCAPED (7 lines, " + bytesOf(dotenvDoc) + ")"}},
		{"yaml mapping", connectorDoc, "oidc.clientSecret", []string{"no value at oidc.clientSecret: the key holds a YAML mapping with the top-level keys clientID, clientSecret (2 lines, " + bytesOf(connectorDoc) + ")"}},
		{"json mapping", jsonDoc, "secret", []string{"the key holds a JSON mapping with the top-level keys clientID, clientSecret (1 line, "}},
		{"yaml list", listDoc, clientSecretPath, []string{"the key holds a YAML list of 2 items (2 lines, " + bytesOf(listDoc) + ")"}},
		{"scalar", password, clientSecretPath, []string{"the key holds a scalar (1 line, " + bytesOf(password) + ")"}},
		{"text", textDoc, clientSecretPath, []string{"the key holds text, no YAML document (2 lines, "}},
		{"padded encoding", paddedDoc, "c2VjcmV0cGFzcw", []string{"the key holds a scalar (1 line, 15 bytes)"}},
		{"binary", "\x00\x01\xff" + password, clientSecretPath, []string{"the key holds binary (" + bytesOf("\x00\x01\xff"+password) + ")"}},
		{"nothing", "", clientSecretPath, []string{"the key holds nothing (0 bytes)"}},
		{"stream", connectorDoc + "---\n" + connectorDoc, clientSecretPath, []string{"the key holds a YAML stream of several documents (5 lines, "}},
		{"mapping on the way", nestedDoc, "config.yaml.oidc.clientToken", []string{"no value at config.yaml.oidc.clientToken: config.yaml.oidc holds a mapping with the keys clientID, clientSecret"}},
		{"block scalar on the way", nestedDoc, "config.yaml.issuer.host", []string{"no value at config.yaml.issuer.host: config.yaml.issuer holds a scalar (1 line, 23 bytes)"}},
		{"nested dotenv", nestedDoc, "values.env.SECRET", []string{"no value at values.env.SECRET: values.env holds dotenv lines with the keys TOKEN, MODE (2 lines, "}},
		{"plain scalar on the way", nestedDoc, "name.first", []string{"no value at name.first: name holds a scalar (1 line, 3 bytes)"}},
		{"a mapping, not a value", nestedDoc, "config.yaml.oidc", []string{"no value at config.yaml.oidc: config.yaml.oidc holds a mapping with the keys clientID, clientSecret"}},
		{"a list, not a value", "peers:\n- a\n- b\n", "peers", []string{"no value at peers: peers holds a list of 2 items"}},
		{"keys like generated values are counted", `{"` + tokenKey + `": 1, "` + strings.Repeat("k", 65) + `": 2, "id": 3}`, "x", []string{"with the top-level keys <36 characters>, <65 characters>, id"}},
	} {
		_, err := readingKube([]byte(tc.raw)).Fingerprints(ctx, secret.Ref{Kube: shapeTarget, Path: tc.path})
		if err == nil {
			t.Errorf("%s: resolved", tc.name)
			continue
		}
		for _, w := range tc.want {
			if !strings.Contains(err.Error(), w) || !strings.HasPrefix(err.Error(), shapeTarget.String()+": ") {
				t.Errorf("%s = %v\nwant %q naming %s", tc.name, err, w, shapeTarget)
			}
		}
		if strings.Contains(err.Error(), tokenKey) {
			t.Errorf("%s names a key like a generated value: %v", tc.name, err)
		}
		noValue(t, tc.name, err.Error())
	}
}

// TestKubeShapeCountsManyKeys: a shape names at most twenty keys and
// counts the rest.
func TestKubeShapeCountsManyKeys(t *testing.T) {
	var b strings.Builder
	for i := range 25 {
		b.WriteString("K" + strings.Repeat("x", i) + "=v\n")
	}
	_, err := readingKube([]byte(b.String())).Fingerprints(context.Background(), secret.Ref{Kube: shapeTarget, Path: "absent"})
	if err == nil || !strings.Contains(err.Error(), ", "+"K"+strings.Repeat("x", 19)+" and 5 more (25 lines, ") {
		t.Errorf("25 keys = %v", err)
	}
}

// bytesOf is a value's byte length as a shape says it.
func bytesOf(s string) string {
	return strconv.Itoa(len(s)) + " bytes"
}
