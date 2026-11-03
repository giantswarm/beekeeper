package identity_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/identity"
)

const (
	connectorID = "kubernetes"
	clientID    = "beekeeper"
	idTokenType = "urn:ietf:params:oauth:token-type:id_token" //nolint:gosec // a token type's name
	plainScopes = "openid email profile groups"
	firstToken  = "dex-1"
	dexIssuer   = "https://dex"
)

// fakeDex answers Dex's token exchange: it checks the request and issues
// "dex-<n>" for the n-th exchange of the subject token it expects.
type fakeDex struct {
	t       *testing.T
	subject string
	scope   string
	status  int
	body    string
	n       atomic.Int32
}

func (d *fakeDex) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/token" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		d.t.Error(err)
	}
	want := map[string]string{ //nolint:gosec // the form's names, no credential
		"grant_type":           "urn:ietf:params:oauth:grant-type:token-exchange",
		"connector_id":         connectorID,
		"client_id":            clientID,
		"subject_token":        d.subject,
		"subject_token_type":   idTokenType,
		"requested_token_type": idTokenType,
		"scope":                d.scope,
	}
	for k, v := range want {
		if got := r.PostForm.Get(k); got != v {
			d.t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	n := d.n.Add(1)
	w.Header().Set("Content-Type", "application/json")
	if d.status != 0 {
		w.WriteHeader(d.status)
		_, _ = w.Write([]byte(d.body))
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{ //nolint:gosec // a stand-in token
		"access_token": "dex-" + string(rune('0'+n)), "issued_token_type": idTokenType,
		"token_type": "bearer", "expires_in": 1800,
	})
}

func newExchange(t *testing.T, d *fakeDex, audience string) (*identity.Exchanger, string) {
	t.Helper()
	srv := httptest.NewServer(d)
	t.Cleanup(srv.Close)
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(d.subject+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := identity.NewExchanger(srv.URL+"/", config.ServeIdentity{TokenFile: file, ConnectorID: connectorID, ClientID: clientID, Audience: audience})
	if err != nil {
		t.Fatal(err)
	}
	return e, file
}

func TestExchangerToken(t *testing.T) {
	d := &fakeDex{t: t, subject: "sa-1", scope: "openid email profile groups audience:server:client_id:dex-k8s-authenticator"}
	e, file := newExchange(t, d, "dex-k8s-authenticator")
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	e.SetNow(func() time.Time { return now })
	ctx := context.Background()

	for _, want := range []string{firstToken, firstToken} {
		got, err := e.Token(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("token %q, want %q", got, want)
		}
	}
	// Still two thirds of 30m: kept.
	now = now.Add(19 * time.Minute)
	if got, _ := e.Token(ctx); got != firstToken {
		t.Fatalf("at 19m: %q, want the kept dex-1", got)
	}
	// The kubelet rotated the projected token; a third left: exchanged anew.
	d.subject = "sa-2"
	if err := os.WriteFile(file, []byte("sa-2"), 0o600); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if got, err := e.Token(ctx); err != nil || got != "dex-2" {
		t.Fatalf("at 20m: %q, %v; want dex-2", got, err)
	}
}

func TestExchangerOwnAudience(t *testing.T) {
	d := &fakeDex{t: t, subject: "sa", scope: plainScopes}
	for _, aud := range []string{"", clientID} {
		e, _ := newExchange(t, d, aud)
		if _, err := e.Token(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExchangerRefused(t *testing.T) {
	d := &fakeDex{t: t, subject: "sa", scope: plainScopes, status: http.StatusUnauthorized, body: `{"error":"access_denied"}`}
	e, _ := newExchange(t, d, "")
	_, err := e.Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401 Unauthorized: access_denied") || !strings.Contains(err.Error(), "connector kubernetes") {
		t.Fatalf("err = %v, want the refusal with Dex's code and the connector", err)
	}
	if strings.Contains(err.Error(), "sa") && strings.Contains(err.Error(), "subject_token") {
		t.Fatalf("err names the subject token: %v", err)
	}
	// A refusal is not kept: the next call asks again.
	_, _ = e.Token(context.Background())
	if d.n.Load() != 2 {
		t.Fatalf("%d exchanges, want 2", d.n.Load())
	}
}

func TestNewExchangerRefusesIncompleteConfig(t *testing.T) {
	full := config.ServeIdentity{TokenFile: "/t", ConnectorID: connectorID, ClientID: clientID}
	for name, tc := range map[string]struct {
		issuer string
		cfg    func(c *config.ServeIdentity)
		want   string
	}{
		"issuer":    {"", func(*config.ServeIdentity) {}, "serve.issuer"},
		"connector": {dexIssuer, func(c *config.ServeIdentity) { c.ConnectorID = "" }, "serve.identity.connectorID"},
		"client":    {dexIssuer, func(c *config.ServeIdentity) { c.ClientID = "" }, "serve.identity.clientID"},
		"file":      {dexIssuer, func(c *config.ServeIdentity) { c.TokenFile = "" }, "serve.identity.tokenFile"},
	} {
		t.Run(name, func(t *testing.T) {
			c := full
			tc.cfg(&c)
			if _, err := identity.NewExchanger(tc.issuer, c); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %s", err, tc.want)
			}
		})
	}
}

func TestExchangerEmptyTokenFile(t *testing.T) {
	d := &fakeDex{t: t, subject: "", scope: plainScopes}
	e, _ := newExchange(t, d, "")
	if _, err := e.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("err = %v, want the empty token file", err)
	}
	if d.n.Load() != 0 {
		t.Fatal("an empty token file reached Dex")
	}
}
