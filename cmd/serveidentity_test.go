package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/identity/identitytest"
)

const (
	identityConnector = "kubernetes"
	identityAudience  = "dex-k8s-authenticator"
)

// identityApp is serve's configuration with a Dex stand-in that exchanges
// any ServiceAccount token for an ID token of beekeeper's identity.
func identityApp(t *testing.T) *app {
	t.Helper()
	iss := identitytest.New(t)
	idToken := iss.Token(t, identityAudience, identitytest.Claims{
		Subject: "CiZzeXN0ZW0", Email: "beekeeper@gazelle.example", EmailVerified: identitytest.Verified(),
	})
	dex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.PostFormValue("connector_id") != identityConnector {
			http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{ //nolint:gosec // a stand-in token
			"access_token": idToken, "issued_token_type": "urn:ietf:params:oauth:token-type:id_token", "expires_in": 1800})
	}))
	t.Cleanup(dex.Close)
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("sa-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Serve.Issuer = dex.URL
	cfg.Serve.Identity = config.ServeIdentity{TokenFile: file, ConnectorID: identityConnector, ClientID: pluginName, Audience: identityAudience}
	return &app{cfg: cfg, out: &bytes.Buffer{}}
}

func runIdentity(a *app, args ...string) (string, error) {
	c := a.serveIdentityCmd()
	var out bytes.Buffer
	c.SetArgs(args)
	c.SetOut(&out)
	c.SetErr(&out)
	c.SilenceUsage, c.SilenceErrors = true, true
	err := c.ExecuteContext(context.Background())
	return out.String(), err
}

func TestServeIdentity(t *testing.T) {
	a := identityApp(t)
	out, err := runIdentity(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"identity: beekeeper@gazelle.example (subject CiZzeXN0ZW0)", "for dex-k8s-authenticator", "no groups", "expires "} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q lacks %q", out, want)
		}
	}
	if strings.Count(out, ".") > 10 {
		t.Errorf("output looks like it carries the token: %q", out)
	}
}

func TestServeIdentityRefusals(t *testing.T) {
	a := identityApp(t)
	if _, err := runIdentity(a, "--tool", "x_kubernetes_list"); err == nil || !strings.Contains(err.Error(), "serve.muster") {
		t.Fatalf("err = %v, want serve.muster unset", err)
	}
	a.cfg.Serve.Muster = "http://muster.invalid/mcp"
	if _, err := runIdentity(a, "--tool", "x", "--args", "[1]"); err == nil || !strings.Contains(err.Error(), "--args") {
		t.Fatalf("err = %v, want the --args refusal", err)
	}
	a.cfg.Serve.Identity.ConnectorID = "saml-staff"
	if _, err := runIdentity(a); err == nil || !strings.Contains(err.Error(), "400 Bad Request: invalid_request") {
		t.Fatalf("err = %v, want Dex's refusal", err)
	}
	a.cfg.Serve.Identity.ConnectorID = ""
	if _, err := runIdentity(a); err == nil || !strings.Contains(err.Error(), "serve.identity.connectorID") {
		t.Fatalf("err = %v, want the missing connector", err)
	}
}

func TestDescribeToken(t *testing.T) {
	for token, want := range map[string]string{
		"no-jwt":     "the token is no JWT",
		"a.!!!.c":    "do not decode",
		"a.bnVsbA.c": "identity: no email",
	} {
		if got := describeToken(token); !strings.Contains(got, want) {
			t.Errorf("describeToken(%q) = %q, want %q", token, got, want)
		}
	}
}
