package secret_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

// dex-operator's provider credentials: a block scalar holding a YAML
// document that mixes public configuration with the client's secret.
const (
	dexClientSecret = "planted-Dex-Secret-7c1e40a9" //nolint:gosec // a planted test value
	dexCredentials  = `client-id: dex-public-client
client-secret: ` + dexClientSecret + `
# the GitHub organisation
org: giantswarm
teams: giantswarm-admins,employees # who logs in
team-name-field: slug
`
	dexOperator = `oidc:
  giantswarm:
    providers:
      - name: giantswarm-github
        credentials: |
` + "          client-id: dex-public-client\n          client-secret: " + dexClientSecret + `
          # the GitHub organisation
          org: giantswarm
          teams: giantswarm-admins,employees # who logs in
          team-name-field: slug
      - name: other
        credentials: one opaque line
`
	credentials = "oidc.giantswarm.providers.0.credentials" //nolint:gosec // a key path, no value
	teamsPath   = credentials + ".teams"
	newTeams    = "giantswarm-admins,employees,team-bumblebee"
)

// dexOperatorScratch is a repository holding the encrypted dex-operator
// values patch.
func dexOperatorScratch(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte("creation_rules: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	patch := filepath.Join(dir, "secret-values.yaml.patch")
	if err := os.WriteFile(patch, secrettest.Encrypt(dexOperator), 0o600); err != nil {
		t.Fatal(err)
	}
	return patch
}

// credentialsOf is the decrypted credentials leaf of the patch.
func credentialsOf(t *testing.T, tools *secrettest.Tools, patch string) string {
	t.Helper()
	plain, err := tools.Run(context.Background(), "", nil, nil, "sops", "decrypt", "--output-type", "yaml", patch)
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		OIDC struct {
			Giantswarm struct {
				Providers []struct {
					Credentials string `yaml:"credentials"`
				} `yaml:"providers"`
			} `yaml:"giantswarm"`
		} `yaml:"oidc"`
	}
	if err := yaml.Unmarshal(plain, &v); err != nil {
		t.Fatal(err)
	}
	return v.OIDC.Giantswarm.Providers[0].Credentials
}

// noDexOperatorSecret fails when the client's secret is in what an
// operation answered.
func noDexOperatorSecret(t *testing.T, what string, v any) {
	t.Helper()
	raw, _ := json.Marshal(v)
	if s := fmt.Sprintf("%s %+v", raw, v); strings.Contains(s, dexClientSecret) {
		t.Errorf("%s answers the client secret: %s", what, s)
	}
}

func TestSetOneKeyInsideALeafDocument(t *testing.T) {
	for _, tc := range []struct {
		name, path, value string
		want              string
	}{
		{"an existing key", teamsPath, newTeams,
			strings.Replace(dexCredentials, "teams: giantswarm-admins,employees # who logs in", "teams: "+newTeams+" # who logs in", 1)},
		{"an absent key", credentials + ".loadAllGroups", "true",
			dexCredentials + `loadAllGroups: "true"` + "\n"},
		{"a value YAML would read otherwise", credentials + ".org", "yes: no",
			strings.Replace(dexCredentials, "org: giantswarm", `org: 'yes: no'`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools := secrettest.New(nil)
			patch := dexOperatorScratch(t)
			res, err := ops(tools).Set(context.Background(), secret.Ref{File: patch, Path: tc.path}, secret.SetOptions{Literal: tc.value})
			if err != nil {
				t.Fatal(err)
			}
			noDexOperatorSecret(t, "set", res)
			if res.Key != patch+"#"+tc.path || res.Length != len(tc.value) {
				t.Errorf("set answers %+v", res)
			}
			if got := credentialsOf(t, tools, patch); got != tc.want {
				t.Errorf("the credentials hold\n%s\nwant\n%s", got, tc.want)
			}
			fs, err := ops(tools).Reveal(context.Background(), patch, []string{tc.path})
			if err != nil || len(fs) != 1 || fs[0] != (secret.Field{Path: tc.path, Value: tc.value}) {
				t.Errorf("reveal %s = %+v, %v", tc.path, fs, err)
			}
		})
	}
}

func TestSetInsideALeafRefusesWhatIsNoMappingDocument(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"oidc.giantswarm.providers.1.credentials.teams", "credentials holds a scalar (1 line, 15 bytes)"},
		{credentials + ".client-id.deeper", "a top-level key"},
		{"oidc.giantswarm.providers.0.name.teams", "holds a scalar"},
	} {
		tools := secrettest.New(nil)
		patch := dexOperatorScratch(t)
		before, _ := os.ReadFile(patch) //nolint:gosec // the test's scratch file
		_, err := ops(tools).Set(context.Background(), secret.Ref{File: patch, Path: tc.path}, secret.SetOptions{Literal: "x,y"})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("set %s = %v, want a refusal naming %q", tc.path, err, tc.want)
		}
		noDexOperatorSecret(t, "set "+tc.path, err)
		if err != nil && strings.Contains(err.Error(), "opaque") {
			t.Errorf("set %s names the leaf's value: %v", tc.path, err)
		}
		if after, _ := os.ReadFile(patch); string(after) != string(before) { //nolint:gosec // the test's scratch file
			t.Errorf("the refused set %s changed the file", tc.path)
		}
	}
	tools := secrettest.New(nil)
	patch := dexOperatorScratch(t)
	if _, err := ops(tools).Set(context.Background(), secret.Ref{File: patch, Path: teamsPath}, secret.SetOptions{Literal: "a\nb"}); err == nil || !strings.Contains(err.Error(), "one line") {
		t.Errorf("a value of two lines: %v", err)
	}
}

func TestRevealAndCompareInsideALeafDocument(t *testing.T) {
	tools := secrettest.New(nil)
	patch := dexOperatorScratch(t)
	fs, err := ops(tools).Reveal(context.Background(), patch, []string{teamsPath, credentials + ".client-id"})
	want := []secret.Field{{Path: teamsPath, Value: "giantswarm-admins,employees"}, {Path: credentials + ".client-id", Value: "dex-public-client"}}
	if err != nil || len(fs) != 2 || fs[0] != want[0] || fs[1] != want[1] {
		t.Errorf("reveal = %+v, %v", fs, err)
	}
	for _, p := range []string{credentials + ".client-secret", credentials} {
		fs, err := ops(tools).Reveal(context.Background(), patch, []string{p})
		if !errors.Is(err, secret.ErrSecretLike) || fs != nil {
			t.Errorf("reveal %s = %+v, %v; want refused", p, fs, err)
		}
		noDexOperatorSecret(t, "reveal "+p, err)
	}
	if _, err := ops(tools).Reveal(context.Background(), patch, []string{credentials + ".absent"}); err == nil || !strings.Contains(err.Error(), "no value at "+credentials+".absent") {
		t.Errorf("an absent key: %v", err)
	}
	ps, err := ops(tools).Fingerprints(context.Background(), secret.Ref{File: patch, Path: credentials + ".client-secret"})
	if err != nil || len(ps) != 1 {
		t.Fatalf("fingerprint of the secret inside the leaf = %+v, %v", ps, err)
	}
	noDexOperatorSecret(t, "fingerprint", ps)
	other := filepath.Join(filepath.Dir(patch), "teams.sops.yaml")
	if err := os.WriteFile(other, secrettest.Encrypt("teams: "+newTeams+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ops(tools).Set(context.Background(), secret.Ref{File: patch, Path: teamsPath}, secret.SetOptions{Literal: newTeams}); err != nil {
		t.Fatal(err)
	}
	vs, err := ops(tools).Compare(context.Background(), secret.Ref{File: patch, Path: teamsPath}, secret.Ref{File: other, Path: "teams"})
	if err != nil || len(vs) != 1 || vs[0].State != secret.Equal {
		t.Errorf("compare = %+v, %v", vs, err)
	}
}
