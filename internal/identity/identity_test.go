package identity_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/identity"
	"github.com/giantswarm/beekeeper/internal/identity/identitytest"
)

const (
	org       = "giantswarm:giantswarm"
	bumblebee = "bumblebee"
	ana       = "ana@example.com"
	notValid  = "not valid"
	teamGroup = "giantswarm:team-bumblebee"
)

func serveConfig(issuer string) config.Serve {
	return config.Serve{
		Issuer: issuer, ClientIDs: []string{"muster", "exchanged"}, Organization: org,
		Teams:       map[string]string{teamGroup: bumblebee, "giantswarm:team-planeteers": "planeteers"},
		Supervisors: map[string]string{bumblebee: "giantswarm:bumblebee-supervisors"},
	}
}

func TestVerify(t *testing.T) {
	iss := identitytest.New(t)
	other := identitytest.New(t)
	ctx := context.Background()
	v, err := identity.New(ctx, serveConfig(iss.URL))
	if err != nil {
		t.Fatal(err)
	}
	member := []string{org, teamGroup}
	no := false

	c, err := v.Verify(ctx, iss.Token(t, "muster", identitytest.Claims{Email: "Ana@Example.com", EmailVerified: identitytest.Verified(), Groups: member}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Email != ana || c.Team != bumblebee {
		t.Errorf("caller %+v, want ana@example.com of bumblebee", c)
	}
	if v.Supervises(c, bumblebee) {
		t.Error("a member without the supervisors' group supervises")
	}
	sup, err := v.Verify(ctx, iss.Token(t, "exchanged", identitytest.Claims{Email: "sup@example.com", EmailVerified: identitytest.Verified(), Groups: append(member, "giantswarm:bumblebee-supervisors")}))
	if err != nil {
		t.Fatal(err)
	}
	if !v.Supervises(sup, bumblebee) || v.Supervises(sup, "planeteers") {
		t.Error("the supervisors' group supervises its team only")
	}

	for _, tc := range []struct {
		name, token, want string
	}{
		{"no token", "", "no bearer token"},
		{"garbage", "not.a.token", notValid},
		{"another issuer's key", other.Token(t, "muster", identitytest.Claims{Issuer: iss.URL, Email: ana, EmailVerified: identitytest.Verified(), Groups: member}), notValid},
		{"expired", iss.Token(t, "muster", identitytest.Claims{Email: ana, EmailVerified: identitytest.Verified(), Groups: member, Expiry: time.Now().Add(-time.Minute).Unix()}), notValid},
		{"another audience", iss.Token(t, "grafana", identitytest.Claims{Email: ana, EmailVerified: identitytest.Verified(), Groups: member}), "not for any of"},
		{"no email", iss.Token(t, "muster", identitytest.Claims{Groups: member}), "no email"},
		{"unverified email", iss.Token(t, "muster", identitytest.Claims{Email: ana, EmailVerified: &no, Groups: member}), "not verified"},
		{"no email_verified", iss.Token(t, "muster", identitytest.Claims{Email: ana, Groups: member}), "not verified"},
		{"not a member", iss.Token(t, "muster", identitytest.Claims{Email: "eve@example.com", EmailVerified: identitytest.Verified(), Groups: []string{teamGroup}}), "not a member"},
		{"no team", iss.Token(t, "muster", identitytest.Claims{Email: ana, EmailVerified: identitytest.Verified(), Groups: []string{org}}), "no team"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := v.Verify(ctx, tc.token)
			var r *identity.RefusedError
			if !errors.As(err, &r) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Verify = %v, want a refusal with %q", err, tc.want)
			}
		})
	}
}

func TestNewRefusesAnOpenConfiguration(t *testing.T) {
	iss := identitytest.New(t)
	for name, mod := range map[string]func(*config.Serve){
		"issuer":       func(c *config.Serve) { c.Issuer = "" },
		"clientIDs":    func(c *config.Serve) { c.ClientIDs = nil },
		"organization": func(c *config.Serve) { c.Organization = "" },
		"teams":        func(c *config.Serve) { c.Teams = nil },
	} {
		cfg := serveConfig(iss.URL)
		mod(&cfg)
		if _, err := identity.New(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "serve."+name) {
			t.Errorf("without %s: %v", name, err)
		}
	}
}
