// Package identity is who calls `beekeeper serve`: a person, from the Dex ID
// token muster forwards (or an installation exchanged), validated against
// the issuer's JWKS. A person is their verified email, never the token's
// subject, which Dex scopes to a connector and which differs between
// installations; their team comes from their groups.
package identity

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/giantswarm/beekeeper/internal/config"
)

// Caller is a verified person.
type Caller struct {
	// Email is the token's verified email: the person's key in every record.
	Email string
	// Team is the team of the first configured group the person is in.
	Team   string
	Groups []string
}

// In reports whether the caller is a member of group.
func (c Caller) In(group string) bool { return group != "" && slices.Contains(c.Groups, group) }

// RefusedError is a token or person the server does not accept.
type RefusedError struct{ Reason string }

func (e *RefusedError) Error() string { return e.Reason }

func refused(format string, a ...any) error {
	return &RefusedError{Reason: fmt.Sprintf(format, a...)}
}

// Verifier checks ID tokens against one issuer and the configured
// audiences, organization and teams.
type Verifier struct {
	v   *oidc.IDTokenVerifier
	cfg config.Serve
}

// New discovers the issuer's keys and returns its verifier. It refuses a
// configuration that would let anyone in.
func New(ctx context.Context, cfg config.Serve) (*Verifier, error) {
	switch {
	case cfg.Issuer == "":
		return nil, errors.New("serve.issuer is not set: the Dex issuer every token is checked against")
	case len(cfg.ClientIDs) == 0:
		return nil, errors.New("serve.clientIDs is not set: the audiences a token may carry")
	case cfg.Organization == "":
		return nil, errors.New("serve.organization is not set: the Dex group every member carries")
	case len(cfg.Teams) == 0:
		return nil, errors.New("serve.teams is not set: the Dex groups that name a caller's team")
	}
	p, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("the issuer %s: %w", cfg.Issuer, err)
	}
	// The audience is checked against the list below: go-oidc takes one.
	return &Verifier{v: p.Verifier(&oidc.Config{SkipClientIDCheck: true}), cfg: cfg}, nil
}

// Verify returns the person a bearer token names, or a RefusedError.
func (v *Verifier) Verify(ctx context.Context, raw string) (Caller, error) {
	if raw == "" {
		return Caller{}, refused("no bearer token: sign in through muster")
	}
	tok, err := v.v.Verify(ctx, raw)
	if err != nil {
		return Caller{}, refused("the token is not valid: %v", err)
	}
	if !slices.ContainsFunc(tok.Audience, func(a string) bool { return slices.Contains(v.cfg.ClientIDs, a) }) {
		return Caller{}, refused("the token is for %s, not for any of %s", strings.Join(tok.Audience, ", "), strings.Join(v.cfg.ClientIDs, ", "))
	}
	var claims struct {
		Email         string   `json:"email"`
		EmailVerified *bool    `json:"email_verified"`
		Groups        []string `json:"groups"`
	}
	if err := tok.Claims(&claims); err != nil {
		return Caller{}, refused("the token's claims: %v", err)
	}
	switch {
	case claims.Email == "":
		return Caller{}, refused("the token carries no email")
	case claims.EmailVerified == nil || !*claims.EmailVerified:
		return Caller{}, refused("the token's email %s is not verified", claims.Email)
	}
	c := Caller{Email: strings.ToLower(claims.Email), Groups: claims.Groups}
	if !c.In(v.cfg.Organization) {
		return Caller{}, refused("%s is not a member of %s", c.Email, v.cfg.Organization)
	}
	groups := make([]string, 0, len(v.cfg.Teams))
	for g := range v.cfg.Teams {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	for _, g := range groups {
		if c.In(g) {
			c.Team = v.cfg.Teams[g]
			return c, nil
		}
	}
	return Caller{}, refused("%s is in no team's group (serve.teams)", c.Email)
}

// Supervises reports whether c holds team's supervisor role.
func (v *Verifier) Supervises(c Caller, team string) bool {
	return c.In(v.cfg.Supervisors[team])
}
