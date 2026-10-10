//go:build unix

package secret_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

const (
	captureJQ = ".passwords[0].value"
	registry  = "registry.example.io"
	pullUser  = "pull-token"
)

// producer is a fake credential generator: a script that prints body on
// stdout and a line on stderr, then exits with code.
func producer(t *testing.T, body string, code int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "generate")
	script := fmt.Sprintf("#!/bin/sh\necho 'generating' >&2\ncat <<'EOF'\n%s\nEOF\nexit %d\n", body, code)
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil { //nolint:gosec // an executable the test runs
		t.Fatal(err)
	}
	return p
}

// tokenJSON is what a registry token's credential generation prints: the
// password once, beside the token's metadata.
var tokenJSON = fmt.Sprintf(`{"passwords":[{"name":"password1","value":%q},{"name":"password2","value":null}],"username":%q}`, password, pullUser)

func TestCaptureTakesTheWholeOutputOrOneFieldAndAnswersNoValue(t *testing.T) {
	for _, c := range []struct {
		name, body, jq string
	}{
		{"whole", password, ""},
		{"jq", tokenJSON, captureJQ},
	} {
		t.Run(c.name, func(t *testing.T) {
			tools := secrettest.New(nil)
			dir, _ := scratch(t)
			dst := secret.Ref{File: filepath.Join(dir, "token.sops.yaml"), Path: pwPath}
			var stderr bytes.Buffer
			res, err := ops(tools).Capture(context.Background(), dst, secret.CaptureOptions{
				Producer: []string{producer(t, c.body, 0)}, Stderr: &stderr, JQ: c.jq, Vault: secret.Ref{Op: appVault},
			})
			if err != nil {
				t.Fatal(err)
			}
			if res.Key != dst.String() || res.Bytes != len(password) || res.Fingerprint != fmt.Sprintf("fp-%d", len(password)*7) {
				t.Errorf("result %+v", res)
			}
			noValue(t, "capture", res)
			if stderr.String() != "generating\n" {
				t.Errorf("the producer's stderr = %q, want it passed through", stderr.String())
			}
			if tools.Vault[appVault] != password {
				t.Error("the vault does not hold the value")
			}
			if !strings.Contains(decrypted(t, tools, dst.File), "password: "+password+"\n") {
				t.Errorf("the SOPS path does not hold the value:\n%s", decrypted(t, tools, dst.File))
			}
			for _, call := range tools.Calls {
				if strings.Contains(call, password) {
					t.Errorf("a command line carries the value: %q", call)
				}
			}
		})
	}
}

func TestCaptureEncodes(t *testing.T) {
	tools := secrettest.New(nil)
	dir, _ := scratch(t)
	dst := secret.Ref{File: filepath.Join(dir, "token.sops.yaml"), Path: pwPath}
	o := ops(tools)
	o.Encode = secret.Encoding{Base64: true, User: pullUser}
	res, err := o.Capture(context.Background(), dst, secret.CaptureOptions{Producer: []string{producer(t, password, 0)}, Vault: secret.Ref{Op: appVault}})
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.StdEncoding.EncodeToString([]byte(pullUser + ":" + password))
	if res.Bytes != len(enc) || !strings.Contains(decrypted(t, tools, dst.File), "password: "+enc) || tools.Vault[appVault] != password {
		t.Errorf("result %+v: the SOPS path holds the encoded form, the vault the printed value", res)
	}
}

func TestCaptureWritesADockerConfigJSONSecret(t *testing.T) {
	tools := secrettest.New(nil)
	dir, _ := scratch(t)
	if err := os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte(secretRules), 0o600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "pull.sops.yaml")
	res, err := ops(tools).Capture(context.Background(), secret.Ref{File: file}, secret.CaptureOptions{
		Producer: []string{producer(t, tokenJSON, 0)}, JQ: captureJQ,
		Docker: &secret.DockerConfig{Registry: registry, Username: pullUser},
		New:    &secret.NewSecret{Name: "pull", Namespace: "flux-system"},
	})
	if err != nil {
		t.Fatal(err)
	}
	noValue(t, "capture", res)
	if res.Key != file+"#stringData..dockerconfigjson" {
		t.Errorf("key = %q", res.Key)
	}
	var s struct {
		Kind       string            `yaml:"kind"`
		Type       string            `yaml:"type"`
		Metadata   map[string]string `yaml:"metadata"`
		StringData map[string]string `yaml:"stringData"`
	}
	if err := yaml.Unmarshal([]byte(decrypted(t, tools, file)), &s); err != nil {
		t.Fatal(err)
	}
	if s.Kind != "Secret" || s.Type != "kubernetes.io/dockerconfigjson" || s.Metadata["name"] != "pull" || s.Metadata["namespace"] != "flux-system" {
		t.Errorf("the Secret = %+v", s)
	}
	var cfg struct {
		Auths map[string]struct{ Username, Password, Auth string } `json:"auths"`
	}
	raw := s.StringData[".dockerconfigjson"]
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("no dockerconfigjson: %v", err)
	}
	a := cfg.Auths[registry]
	if a.Username != pullUser || a.Password != password || a.Auth != base64.StdEncoding.EncodeToString([]byte(pullUser+":"+password)) {
		t.Errorf("the auths entry of %s is wrong", registry)
	}
	if res.Bytes != len(raw) {
		t.Errorf("bytes = %d, want %d", res.Bytes, len(raw))
	}
}

// A refusal comes before the producer runs: a once-printed credential is
// not spent on a call that cannot store it.
func TestCaptureRefusesBeforeTheProducerRuns(t *testing.T) {
	dir, _ := scratch(t)
	if err := os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte(secretRules), 0o600); err != nil {
		t.Fatal(err)
	}
	ran := filepath.Join(t.TempDir(), "ran")
	prod := filepath.Join(t.TempDir(), "generate")
	if err := os.WriteFile(prod, []byte("#!/bin/sh\ntouch "+ran+"\necho x\n"), 0o700); err != nil { //nolint:gosec // an executable the test runs
		t.Fatal(err)
	}
	file := filepath.Join(dir, "x.sops.yaml")
	docker := &secret.DockerConfig{Registry: registry, Username: pullUser}
	for _, c := range []struct {
		name string
		dst  secret.Ref
		opt  secret.CaptureOptions
		want string
	}{
		{"no path", secret.Ref{File: file}, secret.CaptureOptions{}, "file#path"},
		{"plaintext path", secret.Ref{File: file, Path: "token"}, secret.CaptureOptions{}, "would stay plaintext"},
		{"bad filter", secret.Ref{File: file, Path: pwPath}, secret.CaptureOptions{JQ: ".["}, "--jq"},
		{"other vault", secret.Ref{File: file, Path: pwPath}, secret.CaptureOptions{Vault: secret.Ref{Op: "op://Other/x/y"}}, "shared vault"},
		{"docker path", secret.Ref{File: file, Path: pwPath}, secret.CaptureOptions{Docker: docker}, "name the file alone"},
		{"docker no Secret", secret.Ref{File: file}, secret.CaptureOptions{Docker: docker}, "--name and --namespace"},
		{"docker user", secret.Ref{File: file}, secret.CaptureOptions{Docker: &secret.DockerConfig{Registry: registry, Username: "a:b"}}, "--username"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tools := secrettest.New(nil)
			c.opt.Producer = []string{prod}
			_, err := ops(tools).Capture(context.Background(), c.dst, c.opt)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("capture = %v, want a refusal naming %q", err, c.want)
			}
			if _, err := os.Stat(ran); err == nil {
				t.Fatal("the producer ran")
			}
			if len(tools.Calls) != 0 {
				t.Errorf("a refused capture ran %q", tools.Calls)
			}
		})
	}
}

func TestCaptureRefusesAFailingOrSilentProducerAndAnUnfitFilter(t *testing.T) {
	for _, c := range []struct {
		name, body, jq, want string
		code                 int
	}{
		{"exit", password, "", "exited 3: nothing stored", 3},
		{"empty", "", "", "printed nothing on stdout", 0},
		{"no JSON", password, captureJQ, "no JSON document", 0},
		{"nothing", tokenJSON, ".missing[]?", "picks nothing", 0},
		{"no string", tokenJSON, ".passwords", "a JSON array, not a string", 0},
		{"null", tokenJSON, ".passwords[1].value", "a JSON null", 0},
		{"several", tokenJSON, ".passwords[].name", "more than one value", 0},
		{"fails", tokenJSON, ".username | tonumber", "fails on the producer's output", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			tools := secrettest.New(nil)
			dir, _ := scratch(t)
			dst := secret.Ref{File: filepath.Join(dir, "token.sops.yaml"), Path: pwPath}
			_, err := ops(tools).Capture(context.Background(), dst, secret.CaptureOptions{Producer: []string{producer(t, c.body, c.code)}, JQ: c.jq, Vault: secret.Ref{Op: appVault}})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("capture = %v, want %q", err, c.want)
			}
			if strings.Contains(err.Error(), password) || strings.Contains(err.Error(), pullUser) {
				t.Errorf("the refusal carries the output: %v", err)
			}
			if len(tools.Vault) != 0 || slices.ContainsFunc(tools.Calls, func(c string) bool { return strings.HasPrefix(c, "sops --config") }) {
				t.Errorf("a refused capture stored: %q", tools.Calls)
			}
			if _, err := os.Stat(dst.File); err == nil {
				t.Error("a refused capture wrote the file")
			}
		})
	}
}

func TestProducerEnvDropsTheSecrets(t *testing.T) {
	env := secret.ProducerEnv([]string{"PATH=/bin", "OP_SESSION_abc=x", "OP_SERVICE_ACCOUNT_TOKEN=x", "SOPS_AGE_KEY=x", "GITHUB_TOKEN=x", "HOME=/h", "AZURE_CONFIG_DIR=/a"},
		[]string{"*_TOKEN"})
	if want := []string{"PATH=/bin", "HOME=/h", "AZURE_CONFIG_DIR=/a"}; !slices.Equal(env, want) {
		t.Errorf("env = %q, want %q", env, want)
	}
}
