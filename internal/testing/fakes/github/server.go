// Package fakegithub is the GitHub every scenario runs against (contract §3.1): an httptest server
// that speaks the subset of REST and GraphQL the engine's client uses, backed by a small state model
// (repos, org members and teams, issues with their board fields and links, PRs, check runs,
// statuses, reviews, the App installation, rate limits and the webhook delivery log). It answers
// GraphQL by operation name with GitHub's point costs, honours ETags, returns rate-limit headers,
// sends the webhook GitHub would send for every state change a step makes, records every call, and
// injects faults. It never needs real time: everything runs on the scenario's fake clock.
package fakegithub

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/forge"
	"github.com/giantswarm/beekeeper/internal/forge/github/wire"
	"github.com/giantswarm/beekeeper/internal/settle"
)

// Options configure the fake.
type Options struct {
	Clock   *clock.Fake
	Tracker *settle.Tracker
	// WebhookURL is where deliveries are POSTed (the engine's receiver); empty logs them as failed.
	WebhookURL string
	// WebhookSecret signs deliveries; the seed's app.webhookSecret overrides it.
	WebhookSecret string
}

// Call is one recorded request.
type Call struct {
	Index       int
	At          time.Time
	Replica     string // from the User-Agent's parentheses
	API         string // rest, graphql, app
	Method      string
	Path        string
	Operation   string // REST operation id or GraphQL operation name
	Code        int
	Conditional bool
	NotModified bool
	RateLimited bool // a primary or secondary rate limit answered it
	Mutating    bool // a mutating REST call (token minting excepted) or a GraphQL mutation
	Points      int
}

// Server is the fake GitHub.
type Server struct {
	opts   Options
	srv    *httptest.Server
	key    *rsa.PrivateKey
	keyPEM []byte
	hooks  *http.Client

	mu        sync.Mutex
	st        *state
	seeded    bool
	limits    map[string]*rateLimit
	calls     []Call
	marks     map[string]int
	faults    []*fault
	delivery  string // lost or never while a deliveries fault stands
	tokens    map[string]time.Time
	tokenSeq  int
	log       []*logEntry
	logSeq    int64
	guidSeq   int64
	reviewSeq int64
}

type rateLimit struct {
	limit     int
	remaining int
	resetAt   time.Time
}

var (
	appKeyOnce sync.Once
	appKey     *rsa.PrivateKey
	appKeyPEM  []byte
	appKeyErr  error
)

// sharedAppKey generates the App's key once per test binary: RSA key generation is slow, and the
// key needs no isolation between scenarios.
func sharedAppKey() (*rsa.PrivateKey, []byte, error) {
	appKeyOnce.Do(func() {
		appKey, appKeyErr = rsa.GenerateKey(rand.Reader, 2048)
		if appKeyErr == nil {
			appKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(appKey)})
		}
	})
	return appKey, appKeyPEM, appKeyErr
}

// New starts a fake GitHub.
func New(opts Options) (*Server, error) {
	if opts.Clock == nil {
		return nil, errors.New("fakegithub: Clock is required")
	}
	key, keyPEM, err := sharedAppKey()
	if err != nil {
		return nil, fmt.Errorf("fakegithub: generating the App key: %w", err)
	}
	st := newState()
	st.secret = opts.WebhookSecret
	s := &Server{
		opts:   opts,
		key:    key,
		keyPEM: keyPEM,
		hooks:  &http.Client{Timeout: 30 * time.Second},
		st:     st,
		limits: map[string]*rateLimit{
			forge.APIREST:    {limit: 5000, remaining: 5000},
			forge.APIGraphQL: {limit: 5000, remaining: 5000},
			forge.APIApp:     {limit: 5000, remaining: 5000},
		},
		marks:  map[string]int{},
		tokens: map[string]time.Time{},
	}
	s.srv = httptest.NewServer(s)
	return s, nil
}

// URL is the API's base URL (REST at URL, GraphQL at URL+"/graphql").
func (s *Server) URL() string { return s.srv.URL }

// AppKeyPEM is the App's private key, for the client's Config.
func (s *Server) AppKeyPEM() []byte { return append([]byte(nil), s.keyPEM...) }

// Close stops the server.
func (s *Server) Close() { s.srv.Close() }

// MarkStep records where a step starts in the call log, for github_calls' since_step.
func (s *Server) MarkStep(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marks[id] = len(s.calls)
}

// Calls returns the recorded calls.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

func (s *Server) now() time.Time { return s.opts.Clock.Now() }

// Seed loads the merged pre_configuration.github, decoded strictly.
func (s *Server) Seed(github map[string]any) error {
	var doc seedDoc
	if err := decodeStrict(github, &doc); err != nil {
		return fmt.Errorf("fakegithub: seed: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.seed(doc); err != nil {
		return fmt.Errorf("fakegithub: seed: %w", err)
	}
	s.seeded = true
	return nil
}

func (s *Server) seed(doc seedDoc) error {
	st, now := s.st, s.now()
	if doc.App != nil {
		st.appID, st.installationID = doc.App.ID, doc.App.Installation
		if doc.App.WebhookSecret != "" {
			st.secret = doc.App.WebhookSecret
		}
	}
	if doc.Org != nil {
		if doc.Org.Login != "" {
			st.org = doc.Org.Login
		}
		for _, m := range doc.Org.Members {
			st.members[m.Login] = append([]string(nil), m.Teams...)
		}
	}
	if doc.Board != nil {
		if doc.Board.Project != 0 {
			st.project = doc.Board.Project
		}
		if doc.Board.Teams != nil {
			st.teams = append([]string(nil), doc.Board.Teams...)
		}
		if doc.Board.Statuses != nil {
			st.statuses = append([]string(nil), doc.Board.Statuses...)
		}
		if doc.Board.Kinds != nil {
			st.kinds = append([]string(nil), doc.Board.Kinds...)
		}
	}
	for api, l := range doc.RateLimit {
		rl, ok := s.limits[api]
		if !ok {
			return fmt.Errorf("rateLimit: unknown API %q (want rest, graphql or app)", api)
		}
		if l.Limit > 0 {
			rl.limit, rl.remaining = l.Limit, l.Limit
		}
		if l.Remaining != nil {
			rl.remaining = *l.Remaining
		}
		if l.ResetIn != "" {
			d, err := time.ParseDuration(l.ResetIn)
			if err != nil {
				return fmt.Errorf("rateLimit.%s.resetIn: %w", api, err)
			}
			rl.resetAt = now.Add(d)
		}
	}
	for _, sr := range doc.Repos {
		r, err := st.ensureRepo(sr.Name)
		if err != nil {
			return err
		}
		switch sr.Visibility {
		case "", visibilityPublic:
			r.private, r.visibility = false, visibilityPublic
		case "private", "internal":
			r.private, r.visibility = true, sr.Visibility
		default:
			return fmt.Errorf("repo %s: invalid visibility %q", sr.Name, sr.Visibility)
		}
		if sr.Installed != nil {
			r.installed = *sr.Installed
		} else {
			r.installed = true
		}
		if sr.DefaultBranch != "" {
			r.defaultBranch = sr.DefaultBranch
		}
		r.ownerTeam = sr.Owner
		r.requiredChecks = append([]string(nil), sr.RequiredChecks...)
		r.requiredApprovals = sr.RequiredApprovals
		r.archived = sr.Archived
	}
	for _, si := range doc.Issues {
		if err := s.seedIssue(si, now); err != nil {
			return fmt.Errorf("issue %s: %w", si.Ref, err)
		}
	}
	// Links after every issue exists, so link order follows the seed.
	for _, si := range doc.Issues {
		is := st.issues[si.Ref]
		if si.Parent != "" {
			if err := s.link(is, si.Parent, now); err != nil {
				return fmt.Errorf("issue %s: %w", si.Ref, err)
			}
		}
		for _, b := range si.BlockedBy {
			if _, err := st.ensureIssue(b, now); err != nil {
				return fmt.Errorf("issue %s: %w", si.Ref, err)
			}
			if !contains(is.blockedBy, b) {
				is.blockedBy = append(is.blockedBy, b)
			}
		}
	}
	for _, sp := range doc.Pulls {
		if err := s.seedPull(sp, now); err != nil {
			return fmt.Errorf("pull %s: %w", sp.Ref, err)
		}
	}
	return nil
}

func (s *Server) link(child *issue, parentRef string, now time.Time) error {
	st := s.st
	parent, err := st.ensureIssue(parentRef, now)
	if err != nil {
		return err
	}
	if child.parent != "" && child.parent != parentRef {
		if old, ok := st.issues[child.parent]; ok {
			old.subIssues = remove(old.subIssues, child.ref)
		}
	}
	child.parent = parentRef
	if !contains(parent.subIssues, child.ref) {
		parent.subIssues = append(parent.subIssues, child.ref)
	}
	return nil
}

func (s *Server) seedIssue(si seedIssue, now time.Time) error {
	st := s.st
	is, ok := st.issues[si.Ref]
	if !ok {
		var err error
		if is, err = st.newIssue(si.Ref, now); err != nil {
			return err
		}
	}
	created, err := parseTime(si.CreatedAt, now)
	if err != nil {
		return err
	}
	state, reason, err := parseIssueState(si.State)
	if err != nil {
		return err
	}
	is.title, is.body, is.stub = si.Title, si.Body, false
	if si.Author != "" {
		is.author = si.Author
	}
	is.assignees = append([]string(nil), si.Assignees...)
	is.labels = append([]string(nil), si.Labels...)
	is.state, is.stateReason = state, reason
	is.createdAt, is.updatedAt = created, maxTime(created, now)
	if state == stateClosed {
		t := now
		is.closedAt = &t
	}
	if si.Board != nil {
		b := si.Board
		for _, f := range [][2]string{{fieldStatus, b.Status}, {fieldTeam, b.Team}, {fieldKind, b.Kind}} {
			if err := st.checkOption(f[0], f[1]); err != nil {
				return err
			}
		}
		it := is.item
		if it == nil {
			it = &boardItem{id: stableID("item", si.Ref), nodeID: st.itemNodeID(si.Ref), issue: si.Ref, createdAt: now}
			st.items = append(st.items, it)
			is.item = it
		}
		it.status, it.team, it.kind, it.priority = b.Status, b.Team, b.Kind, b.Priority
		it.statusAt, it.updatedAt = now, now
		it.archived = b.Archived
		if b.Archived {
			it.archAt = now
		}
	}
	return nil
}

func (s *Server) seedPull(sp seedPull, now time.Time) error {
	st := s.st
	repoName, n, err := splitRef(sp.Ref)
	if err != nil {
		return err
	}
	if _, ok := st.issues[sp.Ref]; ok {
		return fmt.Errorf("%s is an issue", sp.Ref)
	}
	r, err := st.ensureRepo(repoName)
	if err != nil {
		return err
	}
	created, err := parseTime(sp.CreatedAt, now)
	if err != nil {
		return err
	}
	p, ok := st.pulls[sp.Ref]
	if !ok {
		p = &pull{ref: sp.Ref, repo: repoName, number: n, id: stableID("pull", sp.Ref), nodeID: "PR_" + sp.Ref}
		st.pulls[sp.Ref] = p
	}
	if sp.Head == "" {
		return errors.New("head is required")
	}
	p.title = sp.Title
	if p.title == "" {
		p.title = "PR " + sp.Ref
	}
	p.author, p.head, p.draft = sp.Author, sp.Head, sp.Draft
	p.base = sp.Base
	if p.base == "" {
		p.base = r.defaultBranch
	}
	p.createdAt, p.updatedAt = created, maxTime(created, now)
	p.closes = append([]string(nil), sp.Closes...)
	for _, c := range p.closes {
		if _, err := st.ensureIssue(c, now); err != nil {
			return err
		}
	}
	switch sp.State {
	case "", stateOpen:
		p.state = stateOpen
	case stateClosed:
		p.state = stateClosed
		p.closedAt = &now
	case "merged":
		if sp.MergeSha == "" {
			return errors.New("a merged PR needs mergeSha")
		}
		p.state, p.merged, p.mergeSHA, p.mergedBy = stateClosed, true, sp.MergeSha, p.author
		p.mergedAt, p.closedAt = &now, &now
	default:
		return fmt.Errorf("invalid PR state %q (want open, closed or merged)", sp.State)
	}
	for _, c := range sp.Checks {
		status, conclusion, err := normCheckStatus(c.Status, c.Conclusion)
		if err != nil {
			return err
		}
		s.upsertCheck(repoName, p.head, c.Name, status, conclusion, now)
	}
	for _, cs := range sp.Statuses {
		state, err := normStatusState(cs.State)
		if err != nil {
			return err
		}
		key := repoName + "@" + p.head
		st.stats[key] = append(st.stats[key], &commitStatus{id: st.nextID(), context: cs.Context, state: state, createdAt: now})
	}
	for _, rv := range sp.Reviews {
		state, err := normReviewState(rv.State)
		if err != nil {
			return err
		}
		s.reviewSeq++
		p.reviews = append(p.reviews, &review{id: s.reviewSeq, by: rv.By, state: state, commit: p.head, submittedAt: now})
	}
	return nil
}

// upsertCheck creates or updates a check run by name on a commit; it reports whether it is new.
func (s *Server) upsertCheck(repoName, sha, name, status, conclusion string, now time.Time) (*checkRun, bool) {
	key := repoName + "@" + sha
	var cr *checkRun
	for _, x := range s.st.checks[key] {
		if x.name == name {
			cr = x
		}
	}
	isNew := cr == nil || cr.status == checkCompleted // a completed run that starts again is a re-run
	if isNew {
		cr = &checkRun{id: s.st.nextID(), name: name, sha: sha}
		s.st.checks[key] = append(s.st.checks[key], cr)
	}
	cr.status, cr.conclusion = status, conclusion
	if status != checkQueued && cr.startedAt == nil {
		t := now
		cr.startedAt = &t
	}
	if status == checkCompleted {
		t := now
		cr.completedAt = &t
	} else {
		cr.completedAt = nil
	}
	return cr, isNew
}

// ---- HTTP ----

type reply struct {
	code   int
	header http.Header
	body   []byte
}

func jsonReply(code int, v any) reply {
	b, err := json.Marshal(v)
	if err != nil {
		code, b = http.StatusInternalServerError, []byte(`{"message":"encoding failed"}`)
	}
	return reply{code: code, header: http.Header{}, body: b}
}

func errReply(code int, msg string) reply { return jsonReply(code, wire.ErrorBody{Message: msg}) }

// replicaOf reads the replica from a User-Agent "beekeeper/<version> (<replica>)".
func replicaOf(ua string) string {
	i, j := strings.LastIndexByte(ua, '('), strings.LastIndexByte(ua, ')')
	if i < 0 || j <= i {
		return ""
	}
	return strings.TrimSpace(ua[i+1 : j])
}

// ServeHTTP answers one request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body)) // for the handlers that read it (token minting)
	path := r.URL.Path
	c := Call{
		Replica:     replicaOf(r.Header.Get("User-Agent")),
		API:         wire.APIOf(path),
		Method:      r.Method,
		Path:        path,
		Conditional: r.Header.Get("If-None-Match") != "",
	}
	var gq *gqlRequest
	var route wire.Route
	var params map[string]string
	routed := false
	if c.API == forge.APIGraphQL && r.Method == http.MethodPost {
		gq = parseGQLRequest(body)
		c.Operation = gq.opName
		c.Mutating = gq.opType == "mutation"
	} else {
		route, params, routed = wire.MatchRoute(r.Method, path)
		if routed {
			c.Operation, c.API, c.Mutating = route.Operation, route.API, route.Mutating
		} else {
			c.Mutating = r.Method != http.MethodGet && r.Method != http.MethodHead
		}
	}

	s.mu.Lock()
	now := s.now()
	c.At = now
	rp := s.answer(&c, r, gq, route, params, routed, now)
	c.Code = rp.code
	c.Index = len(s.calls)
	s.calls = append(s.calls, c)
	s.mu.Unlock()

	for k, vs := range rp.header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if len(rp.body) > 0 {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	w.WriteHeader(rp.code)
	if r.Method != http.MethodHead {
		_, _ = w.Write(rp.body)
	}
}

// answer runs the request's pipeline: authentication, faults, the primary rate limit, the handler,
// conditional answers and the rate-limit headers. s.mu is held.
func (s *Server) answer(c *Call, r *http.Request, gq *gqlRequest, route wire.Route, params map[string]string, routed bool, now time.Time) reply {
	if msg, ok := s.authenticate(c, r, now); !ok {
		return errReply(http.StatusUnauthorized, msg)
	}
	rl := s.limit(c.API, now)
	if f := s.matchFault(c); f != nil {
		rp := f.reply()
		if f.kind == faultSecondary {
			c.RateLimited = true
		}
		s.rateHeaders(rp.header, c.API, rl)
		return rp
	}
	if rl.remaining <= 0 {
		c.RateLimited = true
		var rp reply
		msg := fmt.Sprintf("API rate limit exceeded for installation ID %d.", s.st.installationID)
		if c.API == forge.APIGraphQL {
			rp = jsonReply(http.StatusOK, map[string]any{
				answerData:   nil,
				answerErrors: []wire.GraphQLError{{Type: wire.ErrorTypeRateLimited, Message: msg}},
			})
		} else {
			rp = errReply(http.StatusForbidden, msg)
		}
		s.rateHeaders(rp.header, c.API, rl)
		return rp
	}

	var rp reply
	switch {
	case gq != nil:
		rp = s.graphql(c, gq, rl, now)
	case routed:
		rp = s.rest(c, route, params, r, now)
		if rp.code == http.StatusOK && r.Method == http.MethodGet {
			etag := etagOf(rp.body)
			rp.header.Set("ETag", etag)
			if etagMatches(r.Header.Get("If-None-Match"), etag) {
				c.NotModified = true
				rp = reply{code: http.StatusNotModified, header: rp.header}
			}
		}
		if rp.code != http.StatusNotModified && rl.remaining > 0 {
			rl.remaining--
		}
	default:
		rp = errReply(http.StatusNotFound, "Not Found")
		if rl.remaining > 0 {
			rl.remaining--
		}
	}
	s.rateHeaders(rp.header, c.API, rl)
	return rp
}

// authenticate checks the App's JWT on /app endpoints and an installation token elsewhere.
func (s *Server) authenticate(c *Call, r *http.Request, now time.Time) (string, bool) {
	authz := r.Header.Get("Authorization")
	scheme, cred, _ := strings.Cut(authz, " ")
	if c.API == forge.APIApp {
		if !strings.EqualFold(scheme, "bearer") || cred == "" {
			return "A JSON web token could not be decoded", false
		}
		if err := s.verifyJWT(cred, now); err != nil {
			return "'Expiration time' claim ('exp') or 'Issued at' claim ('iat') is invalid: " + err.Error(), false
		}
		return "", true
	}
	if !strings.EqualFold(scheme, "token") && !strings.EqualFold(scheme, "bearer") {
		return "Requires authentication", false
	}
	exp, ok := s.tokens[cred]
	if !ok || !now.Before(exp) {
		return "Bad credentials", false
	}
	return "", true
}

func (s *Server) verifyJWT(tok string, now time.Time) error {
	opts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
		jwt.WithTimeFunc(func() time.Time { return now }),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
	}
	if s.st.appID != 0 {
		opts = append(opts, jwt.WithIssuer(strconv.FormatInt(s.st.appID, 10)))
	}
	var claims jwt.RegisteredClaims
	if _, err := jwt.ParseWithClaims(tok, &claims, func(*jwt.Token) (any, error) { return &s.key.PublicKey, nil }, opts...); err != nil {
		return err
	}
	if claims.ExpiresAt.Sub(now) > 10*time.Minute+time.Second {
		return errors.New("expires more than 10 minutes ahead")
	}
	return nil
}

// limit returns an API's bucket, starting a new hour-long window once the reset has passed.
func (s *Server) limit(api string, now time.Time) *rateLimit {
	rl := s.limits[api]
	if rl.resetAt.IsZero() {
		rl.resetAt = now.Add(time.Hour)
	}
	if !now.Before(rl.resetAt) {
		rl.remaining = rl.limit
		rl.resetAt = now.Add(time.Hour)
	}
	return rl
}

func (s *Server) rateHeaders(h http.Header, api string, rl *rateLimit) {
	resource := "core"
	if api == forge.APIGraphQL {
		resource = "graphql"
	}
	h.Set(wire.HeaderRateLimit, strconv.Itoa(rl.limit))
	h.Set(wire.HeaderRateRemaining, strconv.Itoa(max(rl.remaining, 0)))
	h.Set(wire.HeaderRateUsed, strconv.Itoa(rl.limit-max(rl.remaining, 0)))
	h.Set(wire.HeaderRateReset, strconv.FormatInt(rl.resetAt.Unix(), 10))
	h.Set(wire.HeaderRateResource, resource)
}

func etagOf(body []byte) string {
	sum := sha256.Sum256(body)
	return `W/"` + hex.EncodeToString(sum[:16]) + `"`
}

func etagMatches(header, etag string) bool {
	if header == "" {
		return false
	}
	norm := func(s string) string { return strings.TrimPrefix(strings.TrimSpace(s), "W/") }
	for _, v := range strings.Split(header, ",") {
		if v = strings.TrimSpace(v); v == "*" || norm(v) == norm(etag) {
			return true
		}
	}
	return false
}

// ---- small helpers ----

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func remove(xs []string, x string) []string {
	out := xs[:0]
	for _, y := range xs {
		if y != x {
			out = append(out, y)
		}
	}
	return out
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func timePtr(t time.Time) *time.Time { return &t }
