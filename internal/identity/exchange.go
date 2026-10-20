package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
)

// The RFC 8693 names Dex's token exchange speaks.
const (
	grantTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange" //nolint:gosec // a grant type's name
	tokenTypeID        = "urn:ietf:params:oauth:token-type:id_token"       //nolint:gosec // a token type's name
)

// Exchanger is serve's own identity toward muster: its projected
// ServiceAccount token exchanged at Dex for a short-lived Dex ID token,
// kept until a third of its lifetime is left.
type Exchanger struct {
	cfg      config.ServeIdentity
	tokenURL string
	http     *http.Client
	now      func() time.Time

	mu      sync.Mutex
	token   string
	renewAt time.Time
}

// NewExchanger returns serve's identity at issuer's Dex. It refuses a
// configuration without the connector or the client.
func NewExchanger(issuer string, cfg config.ServeIdentity) (*Exchanger, error) {
	switch {
	case issuer == "":
		return nil, errors.New("serve.issuer is not set: the Dex serve's identity is exchanged at")
	case cfg.ConnectorID == "":
		return nil, errors.New("serve.identity.connectorID is not set: the Dex connector that trusts the ServiceAccount issuer")
	case cfg.ClientID == "":
		return nil, errors.New("serve.identity.clientID is not set: the public Dex client the exchange is made as")
	case cfg.TokenFile == "":
		return nil, errors.New("serve.identity.tokenFile is not set: the projected ServiceAccount token")
	}
	return &Exchanger{
		cfg:      cfg,
		tokenURL: strings.TrimSuffix(issuer, "/") + "/token",
		http:     &http.Client{Timeout: 30 * time.Second},
		now:      time.Now,
	}, nil
}

// Token returns a Dex ID token of serve's identity, exchanging the
// ServiceAccount token anew once a third of the last one's lifetime is left.
func (e *Exchanger) Token(ctx context.Context) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.token != "" && e.now().Before(e.renewAt) {
		return e.token, nil
	}
	token, lifetime, err := e.exchange(ctx)
	if err != nil {
		return "", err
	}
	e.token, e.renewAt = token, e.now().Add(lifetime*2/3)
	return token, nil
}

// exchange trades the ServiceAccount token for a Dex ID token and returns
// it with its lifetime.
func (e *Exchanger) exchange(ctx context.Context) (string, time.Duration, error) {
	raw, err := os.ReadFile(e.cfg.TokenFile)
	if err != nil {
		return "", 0, fmt.Errorf("serve.identity.tokenFile: %w", err)
	}
	subject := strings.TrimSpace(string(raw))
	if subject == "" {
		return "", 0, fmt.Errorf("serve.identity.tokenFile %s is empty", e.cfg.TokenFile)
	}
	scopes := []string{"openid", "email", "profile", "groups"}
	if e.cfg.Audience != "" && e.cfg.Audience != e.cfg.ClientID {
		scopes = append(scopes, "audience:server:client_id:"+e.cfg.Audience)
	}
	form := url.Values{
		"grant_type":           {grantTokenExchange},
		"connector_id":         {e.cfg.ConnectorID},
		"client_id":            {e.cfg.ClientID},
		"scope":                {strings.Join(scopes, " ")},
		"subject_token":        {subject},
		"subject_token_type":   {tokenTypeID},
		"requested_token_type": {tokenTypeID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := e.http.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("dex token exchange: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", 0, fmt.Errorf("dex token exchange: %w", err)
	}
	var out struct {
		AccessToken     string `json:"access_token"`
		IssuedTokenType string `json:"issued_token_type"`
		ExpiresIn       int    `json:"expires_in"`
		Error           string `json:"error"`
		Description     string `json:"error_description"`
	}
	// An error page that is no JSON leaves out empty: the status says it.
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("dex token exchange at %s (connector %s, client %s): %s", e.tokenURL, e.cfg.ConnectorID, e.cfg.ClientID, dexError(resp.Status, out.Error, out.Description))
	}
	switch {
	case out.AccessToken == "":
		return "", 0, fmt.Errorf("dex token exchange at %s: the answer carries no token", e.tokenURL)
	case out.IssuedTokenType != "" && out.IssuedTokenType != tokenTypeID:
		return "", 0, fmt.Errorf("dex token exchange at %s: issued %s, not an ID token", e.tokenURL, out.IssuedTokenType)
	case out.ExpiresIn <= 0:
		return "", 0, fmt.Errorf("dex token exchange at %s: the answer carries no lifetime (expires_in)", e.tokenURL)
	}
	return out.AccessToken, time.Duration(out.ExpiresIn) * time.Second, nil
}

// dexError words a refused exchange: the status, Dex's error code and its
// description where it gave them.
func dexError(status, code, desc string) string {
	parts := []string{status}
	if code != "" {
		parts = append(parts, code)
	}
	if desc != "" {
		parts = append(parts, desc)
	}
	return strings.Join(parts, ": ")
}
