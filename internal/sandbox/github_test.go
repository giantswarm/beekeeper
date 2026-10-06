package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
)

// testToken is a token of the App's length, never a real one.
const testToken = "ghu_" + "0123456789abcdefghijklmnopqrstuvwxyz"

func TestWriteEgress(t *testing.T) {
	roots := filepath.Join(t.TempDir(), "roots.pem")
	if err := os.WriteFile(roots, []byte("ROOTS\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	saved := systemRoots
	systemRoots = []string{filepath.Join(t.TempDir(), "missing"), roots}
	t.Cleanup(func() { systemRoots = saved })
	dir := EgressDir(t.TempDir())
	if err := WriteEgress(dir, []byte("CA\n"), "/opt/bee keeper"); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		EgressCA:                             "CA\n",
		EgressBundle:                         "ROOTS\nCA\n",
		filepath.Join(EgressGH, "hosts.yml"): "github.com:\n    oauth_token: " + GHLogin + "\n    git_protocol: https\n",
		EgressGPG:                            "#!/bin/sh\nexec '/opt/bee keeper' sandbox gpg \"$@\"\n",
	} {
		b, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // the test's own files
		if err != nil || string(b) != want {
			t.Errorf("%s = %q, %v; want %q", name, b, err, want)
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, EgressGPG)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("gpg.program: %v, %v; want an executable script", fi, err)
	}
	systemRoots = []string{filepath.Join(t.TempDir(), "missing")}
	if err := WriteEgress(dir, []byte("CA\n"), "/opt/bee keeper"); err == nil {
		t.Error("no system roots: written")
	}
}

func TestRemoveMaskedGitHub(t *testing.T) {
	run := t.TempDir()
	dir := filepath.Join(run, "beekeeper", "github")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"hosts.yml", "git-credential"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveMaskedGitHub(run); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s stays: %v", dir, err)
	}
	if err := RemoveMaskedGitHub(run); err != nil {
		t.Errorf("nothing to remove: %v", err)
	}
}

func TestKeepGitHub(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tokens := []string{testToken, testToken, ""}
	var said []string
	tok := &Token{}
	n := 0
	KeepGitHub(ctx, tok, time.Millisecond, func(context.Context) (string, error) {
		n++
		if n > len(tokens) {
			cancel()
			return testToken, nil
		}
		if tokens[n-1] == "" {
			return "", errors.New("no login")
		}
		return tokens[n-1], nil
	}, func(s string) { said = append(said, s) })
	renewed := 0
	for _, s := range said {
		if strings.Contains(s, testToken) {
			t.Fatalf("a line names the token: %q", s)
		}
		if strings.HasPrefix(s, "github token: renewed") {
			renewed++
		}
	}
	if renewed != 1 || tok.Get() != testToken || !strings.Contains(strings.Join(said, "\n"), "not renewed (no login)") {
		t.Errorf("lines %q: want one renewal and the failed one", said)
	}
}

func TestSettingsUseTheEgressProxy(t *testing.T) {
	p, home := policy(t)
	p.Egress = EgressDir("/run/user/1000")
	p.Vars = egressVars(p.Egress)
	b, err := p.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "credentials") || strings.Contains(string(b), "tlsTerminate") || strings.Contains(string(b), "git-credential") {
		t.Errorf("the policy masks or terminates: %s", b)
	}
	var s struct {
		Env     map[string]string
		Sandbox struct {
			Network struct{ HTTPProxyPort, SOCKSProxyPort int }
		}
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if s.Sandbox.Network.HTTPProxyPort != 3190 || s.Sandbox.Network.SOCKSProxyPort != 3190 {
		t.Errorf("network = %+v, want both proxies on the egress proxy's port", s.Sandbox.Network)
	}
	bundle := "/run/user/1000/beekeeper/egress/bundle.pem"
	for k, want := range map[string]string{
		"GH_CONFIG_DIR": "/run/user/1000/beekeeper/egress/gh", "SSL_CERT_FILE": bundle, "GIT_SSL_CAINFO": bundle,
		"CURL_CA_BUNDLE": bundle, "REQUESTS_CA_BUNDLE": bundle, "NODE_EXTRA_CA_CERTS": "/run/user/1000/beekeeper/egress/ca.pem",
		"GIT_CONFIG_COUNT": "4", "GIT_CONFIG_VALUE_0": "git@github.com:", "GIT_CONFIG_KEY_2": "credential.https://github.com.helper", "GIT_CONFIG_VALUE_2": "",
		"GIT_CONFIG_KEY_3": "gpg.program", "GIT_CONFIG_VALUE_3": "/run/user/1000/beekeeper/egress/gpg",
	} {
		if got, ok := s.Env[k]; !ok || got != want {
			t.Errorf("env %s = %q, want %q", k, got, want)
		}
	}
	if !p.Readable(filepath.Join(p.Egress, EgressCA), home) || p.Writable(filepath.Join(p.Egress, EgressCA), home) {
		t.Error("the file tools: want the CA readable and not writable")
	}
}

func TestNoRuntimeDirNoEgressVars(t *testing.T) {
	p, _ := policy(t)
	if p.Egress != "" || p.Vars["SSL_CERT_FILE"] != "" {
		t.Fatalf("Egress = %q, vars %v without a runtime directory", p.Egress, p.Vars)
	}
}

func TestRuntimeDirClosedButTheEgressDir(t *testing.T) {
	home, run := testHome(t), t.TempDir()
	p := New(config.Sandbox{ProxyPort: 3190}, Paths{Home: home, ConfigFile: filepath.Join(home, ".config/beekeeper/config.yaml"),
		StateDir: filepath.Join(home, ".local/state/beekeeper"), Exe: filepath.Join(home, ".go/bin/beekeeper"), RuntimeDir: run})
	fs := p.Settings()["sandbox"].(map[string]any)["filesystem"].(map[string]any)
	if deny := fs["denyRead"].([]string); !slices.Contains(deny, "/"+run) || !slices.Contains(deny, "~/") {
		t.Errorf("denyRead = %v, want the home and the runtime directory", deny)
	}
	if allow := fs["allowRead"].([]string); !slices.Contains(allow, "/"+EgressDir(run)) {
		t.Errorf("allowRead = %v, want the egress directory", allow)
	}
	for path, want := range map[string]bool{
		filepath.Join(run, "containers/auth.json"): false,
		filepath.Join(run, "beekeeper/vault.sock"): false,
		filepath.Join(EgressDir(run), EgressCA):    true,
	} {
		if got := p.Readable(path, home); got != want {
			t.Errorf("Readable(%s) = %v, want %v", path, got, want)
		}
	}
}
