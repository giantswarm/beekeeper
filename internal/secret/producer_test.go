//go:build unix

package secret_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

// fluxPull is the pull Secret an absent file starts as.
var fluxPull = &secret.NewSecret{Name: pullUser, Namespace: "flux-giantswarm"}

const azGenerate = "cmd://az acr token credential generate --registry gsociprivate --name " + pullUser + " --password1 -o json#passwords.0.value"

// fakeAz puts an az where producers resolve that prints the token's JSON on stdout, the
// password on stderr too (as --debug would), records that it ran, and
// exits with code.
func fakeAz(t *testing.T, code int) (ran string) {
	t.Helper()
	dir := t.TempDir()
	ran = filepath.Join(dir, "ran")
	script := "#!/bin/sh\ntouch " + ran + "\necho 'response: " + password + "' >&2\ncat <<'EOF'\n" + tokenJSON + "\nEOF\nexit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "az"), []byte(script), 0o700); err != nil { //nolint:gosec // an executable the test runs
		t.Fatal(err)
	}
	t.Cleanup(secret.UseProducersIn(dir))
	return ran
}

func producing(tools *secrettest.Tools) *secret.Ops {
	o := ops(tools)
	o.Produce = true
	return o
}

func TestCmdRefRunsOnlyAnAllowedProducer(t *testing.T) {
	r, err := secret.ParseRef(azGenerate)
	if err != nil {
		t.Fatal(err)
	}
	if r.String() != azGenerate || !r.IsSource() || !r.Single() {
		t.Errorf("ref = %+v (%s)", r, r)
	}
	if _, err := secret.ParseRef("cmd://openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256"); err != nil {
		t.Errorf("openssl genpkey: %v", err)
	}
	for _, s := range []string{
		"cmd://sh -c 'cat /etc/passwd'",
		"cmd://az account get-access-token",
		"cmd://az acr token credential",
		"cmd:///usr/bin/az acr token credential generate --name x",
		"cmd://",
		"cmd://az acr token credential generate --name x --debug",
		"cmd://az acr token credential generate --name x --verbose=true",
		"cmd://openssl genpkey -algorithm ed25519 -out key.pem",
		"cmd://openssl req -new",
	} {
		if _, err := secret.ParseRef(s); err == nil {
			t.Errorf("%s: allowed", s)
		}
	}
}

func TestCopyDockerTakesAProducersPasswordAndAnswersNoValue(t *testing.T) {
	fakeAz(t, 0)
	tools := secrettest.New(nil)
	dir, _ := scratch(t)
	if err := os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte(secretRules), 0o600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "pull-secret.enc.yaml")
	src, err := secret.ParseRef(azGenerate)
	if err != nil {
		t.Fatal(err)
	}
	n, err := producing(tools).CopyDocker(context.Background(), src, file, secret.DockerConfig{Registry: registry, Username: pullUser},
		fluxPull)
	if err != nil {
		t.Fatal(err)
	}
	noValue(t, "copy --dockerconfigjson", n)
	var s struct {
		Type       string            `yaml:"type"`
		Metadata   map[string]string `yaml:"metadata"`
		StringData map[string]string `yaml:"stringData"`
	}
	if err := yaml.Unmarshal([]byte(decrypted(t, tools, file)), &s); err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Auths map[string]struct{ Username, Password string } `json:"auths"`
	}
	raw := s.StringData[".dockerconfigjson"]
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	if s.Type != "kubernetes.io/dockerconfigjson" || s.Metadata["namespace"] != fluxPull.Namespace || cfg.Auths[registry].Password != password || cfg.Auths[registry].Username != pullUser {
		t.Errorf("the Secret is wrong: type %q, metadata %v", s.Type, s.Metadata)
	}
	if n != len(raw) {
		t.Errorf("bytes = %d, want %d", n, len(raw))
	}
	// a path of a SOPS file takes the value itself
	v := secret.Ref{File: filepath.Join(dir, "token.sops.yaml"), Path: "stringData.password"}
	if _, err := producing(tools).CopyValue(context.Background(), src, v); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(decrypted(t, tools, v.File), password) {
		t.Error("copy to a path wrote no password")
	}
}

// A refusal comes before the producer runs, and only copy runs one.
func TestCopyFromAProducerRefusesBeforeItRuns(t *testing.T) {
	ran := fakeAz(t, 0)
	dir, _ := scratch(t)
	if err := os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte(secretRules), 0o600); err != nil {
		t.Fatal(err)
	}
	src, _ := secret.ParseRef(azGenerate)
	d := secret.DockerConfig{Registry: registry, Username: pullUser}
	file := filepath.Join(dir, "x.sops.yaml")
	tools := secrettest.New(nil)
	for name, call := range map[string]func() error{
		"no Secret": func() error {
			_, err := producing(tools).CopyDocker(context.Background(), src, file, d, nil)
			return err
		},
		"bad user": func() error {
			_, err := producing(tools).CopyDocker(context.Background(), src, file, secret.DockerConfig{Registry: registry, Username: "a:b"}, nil)
			return err
		},
		"not copy": func() error {
			_, err := ops(tools).CopyValue(context.Background(), src, secret.Ref{File: file, Path: pwPath})
			return err
		},
		"plain rule": func() error {
			_, err := producing(tools).CopyValue(context.Background(), src, secret.Ref{File: file, Path: "metadata.x"})
			return err
		},
	} {
		err := call()
		if err == nil {
			t.Errorf("%s: no refusal", name)
		} else if strings.Contains(err.Error(), password) {
			t.Errorf("%s: the refusal carries the value", name)
		}
		if _, err := os.Stat(ran); err == nil {
			t.Fatalf("%s: the producer ran", name)
		}
	}
}

// A failing producer names its exit code; its output reaches no answer.
func TestAFailingProducerStoresNothing(t *testing.T) {
	fakeAz(t, 3)
	tools := secrettest.New(nil)
	dir, _ := scratch(t)
	if err := os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte(secretRules), 0o600); err != nil {
		t.Fatal(err)
	}
	src, _ := secret.ParseRef(strings.TrimSuffix(azGenerate, "#passwords.0.value") + "#passwords.1.value")
	file := filepath.Join(dir, "x.sops.yaml")
	_, err := producing(tools).CopyDocker(context.Background(), src, file, secret.DockerConfig{Registry: registry, Username: pullUser},
		fluxPull)
	if err == nil || !strings.Contains(err.Error(), "exited 3") {
		t.Errorf("err = %v", err)
	}
	if err != nil && strings.Contains(err.Error(), password) {
		t.Error("the refusal carries the producer's stderr")
	}
	if _, statErr := os.Stat(file); statErr == nil {
		t.Error("a file was written")
	}
	fakeAz(t, 0)
	if _, err := producing(tools).CopyDocker(context.Background(), src, file, secret.DockerConfig{Registry: registry, Username: pullUser},
		fluxPull); err == nil {
		t.Error("an empty password (null) was stored")
	}
}
