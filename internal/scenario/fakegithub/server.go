//go:build scenario

package fakegithub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// How a request reached the server.
const (
	viaGH   = "gh"
	viaHTTP = "http"
)

// APIEnv is the API base variable of code that calls GitHub over HTTP
// itself (internal/github.APIURL), and CallerEnv names the caller of a
// process to the fake gh.
const (
	APIEnv    = "BEEKEEPER_GITHUB_API"
	CallerEnv = "BEEKEEPER_SCENARIO_CALLER"
)

// callerPrefix starts the path that names a direct caller:
// <base>/caller/<name>/repos/… is <name> asking for /repos/….
const callerPrefix = "/caller/"

// publicAPI is the API's public address: what the fake gh's links name.
const publicAPI = "https://api.github.com"

// Request is one request to the scenario's GitHub.
type Request struct {
	Method string
	// Path is the API path ("/repos/o/r", "/graphql"), escaped as it was
	// sent.
	Path   string
	Query  url.Values
	Header http.Header
	Body   []byte
	// Caller is who sent it, Via how: the gh CLI or HTTP.
	Caller string
	Via    string
	// Base is the API's address as the caller reached it; an answer's
	// links start with it.
	Base string
}

// Response is the answer.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
	// Outage says there is no answer: the connection failed.
	Outage bool
}

func jsonResponse(status int, v any) *Response {
	body, _ := marshalNoEscape(v)
	h := http.Header{}
	h.Set("Content-Type", "application/json; charset=utf-8")
	return &Response{Status: status, Header: h, Body: body}
}

// Call is one request in the server's log.
type Call struct {
	Seq    int       `json:"seq"`
	At     time.Time `json:"at"`
	Caller string    `json:"caller"`
	Via    string    `json:"via"`
	Method string    `json:"method"`
	// Path is the path without its leading slash, Query its query.
	Path  string `json:"path"`
	Query string `json:"query,omitempty"`
	// Operation is a GraphQL request's operation name (or first field),
	// Fields its top-level fields.
	Operation string   `json:"operation,omitempty"`
	Fields    []string `json:"fields,omitempty"`
	Mutation  bool     `json:"mutation,omitempty"`
	Status    int      `json:"status"`
	// Bucket and Cost are what the request spent: nothing for a 304, a
	// fault's answer or a refusal.
	Bucket      string `json:"bucket,omitempty"`
	Cost        int    `json:"cost"`
	Conditional bool   `json:"conditional,omitempty"`
	// Fault names the fault that answered or delayed it.
	Fault  string `json:"fault,omitempty"`
	Outage bool   `json:"outage,omitempty"`
	// Unsupported is what the model lacked: such a call fails the
	// scenario.
	Unsupported string `json:"unsupported,omitempty"`
}

// Line is the call for a report.
func (c Call) Line() string {
	line := fmt.Sprintf("%s %s", c.Method, c.Path)
	if c.Operation != "" {
		line += " (" + c.Operation + ")"
	}
	return fmt.Sprintf("%s by %s via %s: %d", line, cmpOr(c.Caller, "nobody"), c.Via, c.Status)
}

// Cost declares what a GraphQL operation costs: Operation is a regular
// expression that matches the operation's name or one of its top-level
// fields whole.
type Cost struct {
	Operation string `yaml:"operation"`
	Cost      int    `yaml:"cost"`
	re        *regexp.Regexp
}

// Server is the scenario's GitHub: the model behind REST and GraphQL,
// with budgets, conditional reads, faults and a log of every call.
type Server struct {
	mu      sync.Mutex
	model   *Model
	clock   func() time.Time
	budgets *budgets
	faults  []*Fault
	costs   []Cost
	calls   []Call
	log     io.Writer
	// sleep waits a fault's delay.
	sleep func(context.Context, time.Duration)

	ln   net.Listener
	http *http.Server
	url  string
}

// NewServer serves model. Clock is the scenario's clock (the model's when
// nil) and log, when set, receives each call as a JSON line.
func NewServer(model *Model, log io.Writer) *Server {
	s := &Server{model: model, clock: model.Clock, log: log}
	s.budgets = newBudgets(s.clock())
	s.sleep = func(ctx context.Context, d time.Duration) {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
		case <-t.C:
		}
	}
	return s
}

// Model runs fn on the model, serialized with the requests.
func (s *Server) Model(fn func(m *Model) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(s.model)
}

// SetBudget sets a bucket.
func (s *Server) SetBudget(name string, spec BudgetSpec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.budgets.set(name, spec, s.clock())
}

// SetSecondary sets the secondary rate limit: more than max requests
// within per are refused with Retry-After.
func (s *Server) SetSecondary(maxRequests int, per, retryAfter time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.budgets.secondary = &secondaryLimit{Max: maxRequests, Per: per, RetryAfter: retryAfter}
}

// SetCosts declares what GraphQL operations cost.
func (s *Server) SetCosts(costs []Cost) error {
	for i := range costs {
		re, err := whole(costs[i].Operation)
		if err != nil || re == nil {
			return fmt.Errorf("costs[%d].operation %q: a regular expression", i, costs[i].Operation)
		}
		if costs[i].Cost < 0 {
			return fmt.Errorf("costs[%d].cost %d", i, costs[i].Cost)
		}
		costs[i].re = re
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.costs = costs
	return nil
}

// Arm adds a fault; one of the same name is replaced.
func (s *Server) Arm(f Fault) error {
	if err := f.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f.armedAt, f.used = s.clock(), 0
	if f.Name != "" {
		s.faults = slices.DeleteFunc(s.faults, func(o *Fault) bool { return o.Name == f.Name })
	}
	s.faults = append(s.faults, &f)
	return nil
}

// Clear removes the fault of that name, or every fault for "all".
func (s *Server) Clear(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == clearAll {
		s.faults = nil
		return nil
	}
	before := len(s.faults)
	s.faults = slices.DeleteFunc(s.faults, func(o *Fault) bool { return o.Name == name })
	if len(s.faults) == before {
		return fmt.Errorf("no fault %q is armed", name)
	}
	return nil
}

// Calls is the log so far.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// Unsupported are the calls that asked for what the model lacks.
func (s *Server) Unsupported() []Call {
	var out []Call
	for _, c := range s.Calls() {
		if c.Unsupported != "" {
			out = append(out, c)
		}
	}
	return out
}

// Spent is what each caller spent of each bucket.
func (s *Server) Spent() []Spend {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Spend
	for caller, by := range s.budgets.spent {
		for b, points := range by {
			out = append(out, Spend{Caller: caller, Bucket: b, Points: points})
		}
	}
	slices.SortFunc(out, func(a, b Spend) int {
		if c := strings.Compare(a.Caller, b.Caller); c != 0 {
			return c
		}
		return strings.Compare(a.Bucket, b.Bucket)
	})
	return out
}

// Bucket is a bucket's state now.
func (s *Server) Bucket(name string) (BucketState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.budgets.roll(s.clock())
	bk, ok := s.budgets.buckets[name]
	if !ok {
		return BucketState{}, false
	}
	return BucketState{Bucket: name, Limit: bk.Limit, Remaining: bk.remaining(), Used: bk.Used, Reset: bk.Reset}, true
}

// etagOf is a body's weak ETag.
func etagOf(body []byte) string {
	sum := sha256.Sum256(body)
	return `W/"` + hex.EncodeToString(sum[:16]) + `"`
}

func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		c := strings.TrimSpace(candidate)
		if c == "*" || strings.TrimPrefix(c, "W/") == strings.TrimPrefix(etag, "W/") {
			return true
		}
	}
	return false
}

// normalizePath maps a GitHub Enterprise path onto the API's own:
// /api/v3/… and /api/graphql.
func normalizePath(p string) string {
	if p == "/api/graphql" {
		return "/graphql"
	}
	if rest, ok := strings.CutPrefix(p, "/api/v3"); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
		p = cmpOr(rest, "/")
	}
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	return p
}

// graphqlRequest is a GraphQL request's body.
type graphqlRequest struct {
	Query         string         `json:"query"`
	OperationName string         `json:"operationName"`
	Variables     map[string]any `json:"variables"`
}

// Do answers one request.
func (s *Server) Do(ctx context.Context, req *Request) *Response {
	req.Path = normalizePath(req.Path)
	if req.Header == nil {
		req.Header = http.Header{}
	}
	if req.Query == nil {
		req.Query = url.Values{}
	}
	if req.Base == "" {
		req.Base = publicAPI
	}
	graphQL := req.Path == "/graphql"
	var op *gqlOperation
	var opErr error
	if graphQL && req.Method == http.MethodPost {
		var body graphqlRequest
		if err := json.Unmarshal(req.Body, &body); err != nil {
			opErr = &apiError{Status: http.StatusBadRequest, Message: "Problems parsing JSON"}
		} else {
			op, opErr = prepareGraphQL(body.Query, body.OperationName, body.Variables)
		}
	}

	s.mu.Lock()
	now := s.clock()
	s.budgets.roll(now)
	call := Call{At: now.UTC(), Caller: req.Caller, Via: req.Via, Method: req.Method, Path: trimSlash(req.Path), Query: req.Query.Encode(),
		Conditional: req.Header.Get("If-None-Match") != ""}
	if op != nil {
		call.Operation, call.Fields, call.Mutation = op.Name(), op.Roots, op.Op.Kind == "mutation"
	}
	var fault *Fault
	for _, f := range s.faults {
		if f.active(now) && f.holds(req, op) {
			fault = f
			f.used++
			break
		}
	}
	var resp *Response
	if fault != nil {
		call.Fault = cmpOr(fault.Name, "fault")
		resp = fault.response()
		if d := fault.Respond.Delay.Duration; d > 0 {
			// The answer is held without the lock: other callers go on.
			s.mu.Unlock()
			s.sleep(ctx, d)
			s.mu.Lock()
			now = s.clock()
			s.budgets.roll(now)
		}
	}
	if resp == nil {
		resp = s.answer(req, op, opErr, &call, now)
	}
	if resp.Header == nil {
		resp.Header = http.Header{}
	}
	if !resp.Outage {
		bucketName := cmpOr(call.Bucket, bucketCore)
		if graphQL {
			bucketName = bucketGraphQL
		}
		if resp.Header.Get("X-RateLimit-Limit") == "" {
			s.budgets.headers(resp.Header, bucketName)
		}
		resp.Header.Set("X-GitHub-Request-Id", fmt.Sprintf("FAKE:%06d", len(s.calls)+1))
		resp.Header.Set("X-GitHub-Media-Type", "github.v3; format=json")
	}
	call.Status, call.Outage = resp.Status, resp.Outage
	call.Seq = len(s.calls) + 1
	s.calls = append(s.calls, call)
	if s.log != nil {
		if b, err := json.Marshal(call); err == nil {
			_, _ = s.log.Write(append(b, '\n'))
		}
	}
	s.mu.Unlock()
	return resp
}

// answer is the model's answer to a request no fault answered. It holds
// the server's lock.
func (s *Server) answer(req *Request, op *gqlOperation, opErr error, call *Call, now time.Time) *Response {
	if req.Path == "/rate_limit" && (req.Method == http.MethodGet || req.Method == http.MethodHead) {
		// /rate_limit is exempt: it costs nothing of any bucket.
		return jsonResponse(http.StatusOK, s.budgets.rateLimitBody())
	}
	if wait, limited := s.budgets.tooFast(now); limited {
		return secondaryResponse(wait)
	}
	if req.Path == "/graphql" {
		return s.graphQL(req, op, opErr, call)
	}
	return s.rest(req, call)
}

// exhausted is GitHub's answer once a bucket holds no more points.
func (s *Server) exhausted(name string) *Response {
	resp := jsonResponse(http.StatusForbidden, obj{
		"message":           fmt.Sprintf("API rate limit exceeded for user ID %d.", s.model.Account(s.model.Viewer, "").ID),
		"documentation_url": "https://docs.github.com/rest/overview/rate-limits-for-the-rest-api",
		"status":            "403",
	})
	s.budgets.headers(resp.Header, name)
	return resp
}

func (s *Server) rest(req *Request, call *Call) *Response {
	rt, params, known := findRoute(req.Method, req.Path)
	if rt == nil {
		what := "the REST route " + req.Method + " " + req.Path
		if known {
			what = "the method " + req.Method + " of the REST route " + req.Path
		}
		call.Unsupported = what
		return jsonResponse(http.StatusNotFound, errNotFound().body())
	}
	call.Bucket = rt.bucket
	if s.budgets.buckets[rt.bucket].remaining() < 1 {
		return s.exhausted(rt.bucket)
	}
	c := &restCtx{s: s, m: s.model, req: &http.Request{Method: req.Method, URL: &url.URL{Path: req.Path}, Header: req.Header},
		params: params, query: req.Query, raw: req.Body, status: http.StatusOK, header: http.Header{}, base: req.Base}
	var resp *Response
	body, err := decodeBody(req.Body)
	if req.Method == http.MethodGet || req.Method == http.MethodHead || strings.HasSuffix(req.Path, "/assets") {
		body, err = obj{}, nil
	}
	var out any
	if err == nil {
		c.body = body
		out, err = rt.handle(c)
	}
	var api *apiError
	var un *unsupportedError
	switch {
	case errors.As(err, &un):
		call.Unsupported = un.What
		resp = jsonResponse(http.StatusNotFound, errNotFound().body())
	case errors.As(err, &api):
		resp = jsonResponse(api.Status, api.body())
	case err != nil:
		resp = jsonResponse(http.StatusInternalServerError, obj{"message": err.Error()})
	default:
		switch v := out.(type) {
		case rawBody:
			resp = &Response{Status: c.status, Header: http.Header{}, Body: v.Data}
			resp.Header.Set("Content-Type", v.ContentType)
		case nil:
			resp = &Response{Status: c.status, Header: http.Header{}}
		default:
			resp = jsonResponse(c.status, out)
		}
		for k, v := range c.header {
			resp.Header[k] = v
		}
	}
	if (req.Method == http.MethodGet || req.Method == http.MethodHead) && resp.Status == http.StatusOK {
		etag := etagOf(resp.Body)
		if match := req.Header.Get("If-None-Match"); match != "" && etagMatches(match, etag) {
			// A 304 costs nothing, as GitHub answers an authorized
			// conditional request.
			nm := &Response{Status: http.StatusNotModified, Header: http.Header{}}
			nm.Header.Set("ETag", etag)
			return nm
		}
		resp.Header.Set("ETag", etag)
	}
	if req.Method == http.MethodHead {
		resp.Body = nil
	}
	s.budgets.charge(rt.bucket, req.Caller, 1)
	call.Cost = 1
	return resp
}

func (s *Server) graphQL(req *Request, op *gqlOperation, opErr error, call *Call) *Response {
	call.Bucket = bucketGraphQL
	if req.Method != http.MethodPost {
		call.Unsupported = "the method " + req.Method + " of /graphql"
		return jsonResponse(http.StatusNotFound, errNotFound().body())
	}
	var api *apiError
	var syntax *gqlSyntaxError
	switch {
	case errors.As(opErr, &api):
		return jsonResponse(api.Status, api.body())
	case errors.As(opErr, &syntax):
		// GitHub answers a document it cannot parse with 200 and errors.
		return jsonResponse(http.StatusOK, obj{"errors": []any{obj{
			"message": syntax.Error(), "locations": []any{obj{"line": syntax.Line, "column": syntax.Col}},
		}}})
	case opErr != nil:
		return jsonResponse(http.StatusOK, obj{"errors": []any{obj{"message": opErr.Error()}}})
	}
	cost := queryCost(op)
	for _, declared := range s.costs {
		if declared.re.MatchString(op.Op.Name) || slices.ContainsFunc(op.Roots, declared.re.MatchString) {
			cost = declared.Cost
			break
		}
	}
	bk := s.budgets.buckets[bucketGraphQL]
	if bk.remaining() < cost || bk.remaining() == 0 {
		return jsonResponse(http.StatusOK, obj{"errors": []any{obj{
			"type": "RATE_LIMITED", "code": "graphql_rate_limit",
			"message": fmt.Sprintf("API rate limit already exceeded for user ID %d.", s.model.Account(s.model.Viewer, "").ID),
		}}})
	}
	x := &gqlExec{srv: s, m: s.model}
	x.rate = rateView{Limit: bk.Limit, Remaining: bk.remaining() - cost, Used: bk.Used + cost, Cost: cost, ResetAt: bk.Reset}
	res := x.execute(op)
	call.Unsupported = res.Unsupported
	s.budgets.charge(bucketGraphQL, req.Caller, cost)
	call.Cost = cost
	resp := &Response{Status: http.StatusOK, Header: http.Header{}, Body: res.body()}
	resp.Header.Set("Content-Type", "application/json; charset=utf-8")
	return resp
}

// uploadBase is where release assets are uploaded: the server itself.
func (s *Server) uploadBase(base string) string { return base }

// Listen serves the API on a loopback port; URL is its address.
func (s *Server) Listen() error {
	ln, err := listenLoopback()
	if err != nil {
		return err
	}
	s.ln = ln
	s.url = "http://" + ln.Addr().String()
	s.http = &http.Server{Handler: s, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = s.http.Serve(ln) }()
	return nil
}

// URL is the address Listen serves on.
func (s *Server) URL() string { return s.url }

// Close stops serving.
func (s *Server) Close() error {
	if s.http == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.http.Shutdown(ctx); err != nil {
		return s.http.Close()
	}
	return nil
}

// Env is the environment that points a process at the server and names
// it as caller: the API base variable for code that calls HTTP itself,
// and the caller for the fake gh.
func (s *Server) Env(caller string) map[string]string {
	env := map[string]string{CallerEnv: caller}
	if s.url != "" {
		env[APIEnv] = s.url + callerPrefix + url.PathEscape(caller)
	}
	return env
}

// ServeHTTP serves a request that arrived over HTTP. Its path may start
// with /caller/<name>, which names who sent it.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	// The path stays escaped: a route's segment may hold an escaped slash.
	req := &Request{Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.Query(), Header: r.Header, Body: body, Via: viaHTTP, Base: scheme + "://" + r.Host}
	if rest, ok := strings.CutPrefix(req.Path, callerPrefix); ok {
		name, path, _ := strings.Cut(rest, "/")
		req.Caller, req.Path = name, "/"+path
		req.Base += callerPrefix + url.PathEscape(name)
	}
	resp := s.Do(r.Context(), req)
	if resp.Outage {
		// net/http aborts the connection without an answer.
		panic(http.ErrAbortHandler)
	}
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	if resp.Status != http.StatusNotModified && resp.Status != http.StatusNoContent {
		w.Header().Set("Content-Length", strconv.Itoa(len(resp.Body)))
	}
	w.WriteHeader(resp.Status)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, bytes.NewReader(resp.Body))
	}
}
