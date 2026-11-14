// Package github is the engine's GitHub client: a read-only forge.Reader over GitHub's REST and
// GraphQL APIs, authenticated as a GitHub App installation.
//
// It has no mutating method. The only POSTs it sends are GraphQL queries and the minting of
// installation tokens (contract §1.6), which ask for read permissions only (wire.ReadPermissions), so
// even a leaked token can't write. It never retries or sleeps: a failed read comes back as a
// forge.Error whose kind tells the engine when to try again on its own clock. Its budget follows
// GitHub's rate-limit headers, one bucket per API (rest, graphql and the App's own app bucket), so
// other users of the same App count too.
package github

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/golang-jwt/jwt/v5"

	"github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/forge"
	"github.com/giantswarm/beekeeper/internal/forge/github/wire"
	"github.com/giantswarm/beekeeper/internal/settle"
)

// Config configures a Client.
type Config struct {
	// BaseURL is the REST API's root (https://api.github.com); GraphQL is at BaseURL+"/graphql".
	BaseURL        string
	AppID          int64
	InstallationID int64
	// PrivateKeyPEM is the App's private key (PKCS#1 or PKCS#8), used to sign its JWT.
	PrivateKeyPEM []byte
	Clock         clock.Clock
	HTTPClient    *http.Client
	// UserAgent is "beekeeper/<version> (<replica>)".
	UserAgent string
	// Reserve is the share of each API's limit that observation leaves alone (0.2): an observation
	// read is deferred while the remaining budget is at or below Reserve × limit. 0 means none.
	Reserve float64
	// Observer is told about every request made (for the metrics). Optional.
	Observer Observer
	// Tracker makes in-flight requests visible to the harness. Optional.
	Tracker *settle.Tracker
	// Logger logs what GitHub answered beside the data, such as errors about parts of a GraphQL
	// answer. Optional (the zero Logger discards).
	Logger logr.Logger
}

// Request describes one request made to GitHub, for the metrics (engine_github_requests_total,
// engine_github_graphql_points_total, engine_github_rate_remaining).
type Request struct {
	API       string // rest, graphql, app
	Class     string // observation, metrics
	Operation string // REST operation id or GraphQL operation name
	Method    string
	Path      string // without the query
	// Code is the HTTP status; 0 when no answer came (timeout, connection error).
	Code        int
	Conditional bool
	NotModified bool
	// Points is a GraphQL request's cost.
	Points int
	// GraphQLErrors counts the errors a GraphQL answer carried beside its data (partial answers).
	GraphQLErrors int
	// Kind is the error kind when the request failed ("" when it succeeded).
	Kind forge.ErrorKind
	// Limit is the API's rate limit after the answer, when the answer carried rate-limit headers.
	Limit *forge.APILimit
}

// Observer receives every request the client makes.
type Observer func(Request)

const (
	tokenRefreshMargin = 5 * time.Minute // installation tokens live 1h; renew 5m before expiry
	jwtLifetime        = 9 * time.Minute // GitHub accepts at most 10m
	jwtBackdate        = 60 * time.Second
	maxPages           = 50
	defaultRetryAfter  = 60 * time.Second
)

// Client reads GitHub. It implements forge.Reader.
type Client struct {
	cfg Config
	// reserve is Config.Reserve, changeable at run time (SetReserve) as the Installation's budget changes.
	reserve atomic.Uint64
	base    string
	key     *rsa.PrivateKey
	http    *http.Client

	tokenMu  sync.Mutex // serialises minting
	authMu   sync.Mutex
	token    string
	tokenExp time.Time
	jwt      string
	jwtExp   time.Time

	mu             sync.Mutex
	buckets        map[string]*bucket
	secondaryUntil time.Time
	deferred       map[string]int
}

var _ forge.Reader = (*Client)(nil)

// New returns a client for the App installation.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("github: BaseURL is required")
	}
	if cfg.AppID == 0 || cfg.InstallationID == 0 {
		return nil, errors.New("github: AppID and InstallationID are required")
	}
	if cfg.Clock == nil {
		return nil, errors.New("github: Clock is required")
	}
	if cfg.Reserve < 0 || cfg.Reserve >= 1 {
		return nil, fmt.Errorf("github: Reserve %v is not in [0, 1)", cfg.Reserve)
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("github: App private key: %w", err)
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "beekeeper"
	}
	c := &Client{
		cfg:  cfg,
		base: strings.TrimRight(cfg.BaseURL, "/"),
		key:  key,
		http: hc,
		buckets: map[string]*bucket{
			forge.APIREST:    {},
			forge.APIGraphQL: {},
			forge.APIApp:     {},
		},
		deferred: map[string]int{},
	}
	c.reserve.Store(math.Float64bits(cfg.Reserve))
	return c, nil
}

type authKind int

const (
	authInstallation authKind = iota
	authJWT
)

// call is one HTTP request.
type call struct {
	api    string
	op     string
	method string
	path   string // with query
	body   []byte
	etag   string
	auth   authKind
	class  string
	// infra calls (token minting) are not observation reads: the reserve doesn't defer them.
	infra bool
}

// answer is a successful (2xx or 304) answer.
type answer struct {
	code   int
	header http.Header
	body   []byte
	// gql is the decoded GraphQL answer.
	gql wire.GraphQLResponse
}

// do makes one request: it checks the budget first (no request is made while an API is stopped or
// observation is deferred), authenticates, classifies the answer and reports it.
func (c *Client) do(ctx context.Context, cl call) (*answer, error) {
	if err := c.admit(cl); err != nil {
		return nil, err
	}
	var authz string
	switch cl.auth {
	case authJWT:
		s, err := c.appJWT()
		if err != nil {
			return nil, &forge.Error{Kind: forge.Refused, API: cl.api, Operation: cl.op, Err: err}
		}
		authz = "Bearer " + s
	default:
		tok, err := c.installationToken(ctx, cl.class)
		if err != nil {
			return nil, err
		}
		authz = "token " + tok
	}

	var body io.Reader
	if cl.body != nil {
		body = bytes.NewReader(cl.body)
	}
	req, err := http.NewRequestWithContext(ctx, cl.method, c.base+cl.path, body)
	if err != nil {
		return nil, &forge.Error{Kind: forge.Refused, API: cl.api, Operation: cl.op, Err: err}
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	req.Header.Set("Authorization", authz)
	if cl.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cl.etag != "" {
		req.Header.Set("If-None-Match", cl.etag)
	}

	done := c.cfg.Tracker.Begin("github request")
	resp, err := c.http.Do(req)
	var raw []byte
	if err == nil {
		raw, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	done()

	rq := Request{
		API: cl.api, Class: cl.class, Operation: cl.op, Method: cl.method, Path: pathOnly(cl.path),
		Conditional: cl.etag != "",
	}
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		fe := &forge.Error{Kind: forge.Transient, API: cl.api, Operation: cl.op, Code: code, Err: err}
		rq.Code, rq.Kind = code, fe.Kind
		c.observe(rq)
		return nil, fe
	}

	now := c.cfg.Clock.Now()
	rq.Code = resp.StatusCode
	rq.Limit = c.updateFromHeaders(cl.api, resp.Header, now)
	a := &answer{code: resp.StatusCode, header: resp.Header, body: raw}
	fe := c.classify(cl, a, now)
	if cl.api == forge.APIGraphQL && a.code == http.StatusOK {
		rq.Points = graphQLCost(raw)
		rq.GraphQLErrors = len(a.gql.Errors)
	}
	if a.code == http.StatusNotModified {
		rq.NotModified = true
	}
	if fe != nil {
		rq.Kind = fe.Kind
		if fe.Kind == forge.RateLimited {
			rq.Limit = c.limitSnapshot(cl.api, now)
		}
	}
	c.observe(rq)
	if fe != nil {
		return nil, fe
	}
	return a, nil
}

func (c *Client) observe(r Request) {
	if c.cfg.Observer != nil {
		c.cfg.Observer(r)
	}
}

// classify turns a non-success answer into a forge.Error and records rate limits.
func (c *Client) classify(cl call, a *answer, now time.Time) *forge.Error {
	mk := func(k forge.ErrorKind, err error) *forge.Error {
		return &forge.Error{Kind: k, API: cl.api, Operation: cl.op, Code: a.code, Err: err}
	}
	code := a.code
	switch {
	case code == http.StatusNotModified:
		return nil
	case code >= 200 && code < 300:
		if cl.api != forge.APIGraphQL {
			return nil
		}
		if err := json.Unmarshal(a.body, &a.gql); err != nil {
			return mk(forge.Transient, fmt.Errorf("decoding GraphQL answer: %w", err))
		}
		for _, e := range a.gql.Errors {
			if e.Type == wire.ErrorTypeRateLimited {
				return c.primaryLimited(cl, mk, a.header, now, e.Message)
			}
		}
		return nil
	case code == http.StatusForbidden || code == http.StatusTooManyRequests:
		msg := errorMessage(a.body)
		if a.header.Get(wire.HeaderRateRemaining) == "0" {
			return c.primaryLimited(cl, mk, a.header, now, msg)
		}
		ra, hasRA := retryAfter(a.header)
		if hasRA || strings.Contains(strings.ToLower(msg), "secondary rate limit") || code == http.StatusTooManyRequests {
			if !hasRA {
				ra = defaultRetryAfter
			}
			until := now.Add(ra)
			c.mu.Lock()
			if until.After(c.secondaryUntil) {
				c.secondaryUntil = until
			}
			c.mu.Unlock()
			fe := mk(forge.SecondaryRateLimited, errors.New(msg))
			fe.RetryAfter, fe.ResetAt = ra, until
			return fe
		}
		return mk(forge.Refused, errors.New(msg))
	case code == http.StatusUnauthorized:
		// Credentials went stale (an expired token): drop them, so the retry authenticates afresh.
		c.authMu.Lock()
		if cl.auth == authJWT {
			c.jwt = ""
		} else {
			c.token = ""
		}
		c.authMu.Unlock()
		return mk(forge.Transient, errors.New(errorMessage(a.body)))
	case code >= 500:
		return mk(forge.Transient, errors.New(errorMessage(a.body)))
	case code >= 400:
		// 404, 410, 422 and other client errors: refused until the inputs change.
		return mk(forge.Refused, errors.New(errorMessage(a.body)))
	default:
		return mk(forge.Transient, fmt.Errorf("unexpected status %d", code))
	}
}

// primaryLimited stops an API until its reset.
func (c *Client) primaryLimited(cl call, mk func(forge.ErrorKind, error) *forge.Error, h http.Header, now time.Time, msg string) *forge.Error {
	c.mu.Lock()
	b := c.buckets[cl.api]
	reset := b.resetAt
	if t, ok := headerTime(h, wire.HeaderRateReset); ok {
		reset = t
	}
	if !reset.After(now) {
		reset = now.Add(time.Hour)
	}
	b.known = true
	b.remaining = 0
	b.resetAt = reset
	b.stoppedUntil = reset
	c.mu.Unlock()
	fe := mk(forge.RateLimited, errors.New(msg))
	fe.ResetAt = reset
	return fe
}

func errorMessage(body []byte) string {
	var eb wire.ErrorBody
	if json.Unmarshal(body, &eb) == nil && eb.Message != "" {
		return eb.Message
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func retryAfter(h http.Header) (time.Duration, bool) {
	v := h.Get(wire.HeaderRetryAfter)
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0, false
	}
	return time.Duration(n) * time.Second, true
}

func headerTime(h http.Header, name string) (time.Time, bool) {
	v := h.Get(name)
	if v == "" {
		return time.Time{}, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(n, 0).UTC(), true
}

// graphQLCost reads data.rateLimit.cost.
func graphQLCost(body []byte) int {
	var r struct {
		Data struct {
			RateLimit *wire.RateLimit `json:"rateLimit"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &r) != nil || r.Data.RateLimit == nil {
		return 0
	}
	return r.Data.RateLimit.Cost
}

func pathOnly(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}

// appJWT returns the App's JWT, signed anew a minute before the previous one expires.
func (c *Client) appJWT() (string, error) {
	now := c.cfg.Clock.Now()
	c.authMu.Lock()
	defer c.authMu.Unlock()
	if c.jwt != "" && now.Before(c.jwtExp.Add(-time.Minute)) {
		return c.jwt, nil
	}
	exp := now.Add(jwtLifetime)
	claims := jwt.RegisteredClaims{
		Issuer:    strconv.FormatInt(c.cfg.AppID, 10),
		IssuedAt:  jwt.NewNumericDate(now.Add(-jwtBackdate)),
		ExpiresAt: jwt.NewNumericDate(exp),
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(c.key)
	if err != nil {
		return "", fmt.Errorf("signing the App JWT: %w", err)
	}
	c.jwt, c.jwtExp = s, exp
	return s, nil
}

// tokenRequest is the body of every mint: read permissions only.
var tokenRequest = func() []byte {
	b, err := json.Marshal(wire.InstallationTokenRequest{Permissions: wire.ReadPermissions()})
	if err != nil {
		panic(err)
	}
	return b
}()

// installationToken returns a cached installation token, minting one (POST
// /app/installations/{id}/access_tokens with the App's JWT) when none is valid for 5 more minutes.
// The token asks for read permissions only, whatever the App was granted.
func (c *Client) installationToken(ctx context.Context, class string) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	now := c.cfg.Clock.Now()
	c.authMu.Lock()
	tok, exp := c.token, c.tokenExp
	c.authMu.Unlock()
	if tok != "" && now.Before(exp.Add(-tokenRefreshMargin)) {
		return tok, nil
	}
	a, err := c.do(ctx, call{
		api: forge.APIApp, op: forge.OpCreateInstallToken, method: http.MethodPost,
		path: fmt.Sprintf("/app/installations/%d/access_tokens", c.cfg.InstallationID),
		body: tokenRequest, auth: authJWT, class: class, infra: true,
	})
	if err != nil {
		return "", err
	}
	var it wire.InstallationToken
	if err := json.Unmarshal(a.body, &it); err != nil || it.Token == "" {
		return "", &forge.Error{Kind: forge.Transient, API: forge.APIApp, Operation: forge.OpCreateInstallToken, Code: a.code,
			Err: fmt.Errorf("decoding the installation token: %v", err)}
	}
	c.authMu.Lock()
	c.token, c.tokenExp = it.Token, it.ExpiresAt
	c.authMu.Unlock()
	return it.Token, nil
}

// get reads a REST resource (with the installation's token) conditionally.
func (c *Client) get(ctx context.Context, api, op, path, etag string, out any) (forge.Meta, *answer, error) {
	auth := authInstallation
	if api == forge.APIApp {
		auth = authJWT
	}
	a, err := c.do(ctx, call{api: api, op: op, method: http.MethodGet, path: path, etag: etag, auth: auth, class: forge.ClassFrom(ctx)})
	if err != nil {
		return forge.Meta{}, nil, err
	}
	meta := forge.Meta{ETag: a.header.Get("ETag")}
	if a.code == http.StatusNotModified {
		meta.NotModified = true
		if meta.ETag == "" {
			meta.ETag = etag
		}
		return meta, a, nil
	}
	if out != nil {
		if err := json.Unmarshal(a.body, out); err != nil {
			return forge.Meta{}, nil, &forge.Error{Kind: forge.Transient, API: api, Operation: op, Code: a.code,
				Err: fmt.Errorf("decoding the answer: %w", err)}
		}
	}
	return meta, a, nil
}

// nextPage returns the path of a Link header's rel="next", relative to the base URL.
func (c *Client) nextPage(h http.Header) string {
	for _, part := range strings.Split(h.Get("Link"), ",") {
		part = strings.TrimSpace(part)
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		start, end := strings.IndexByte(part, '<'), strings.IndexByte(part, '>')
		if start < 0 || end <= start {
			return ""
		}
		u := part[start+1 : end]
		if !strings.HasPrefix(u, c.base) {
			return ""
		}
		return strings.TrimPrefix(u, c.base)
	}
	return ""
}

// SetReserve changes the observation reserve (the Installation's budget.observationReserve); values outside
// [0, 1) are ignored.
func (c *Client) SetReserve(reserve float64) {
	if reserve < 0 || reserve >= 1 {
		return
	}
	c.reserve.Store(math.Float64bits(reserve))
}

func (c *Client) currentReserve() float64 { return math.Float64frombits(c.reserve.Load()) }
