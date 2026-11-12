//go:build scenario

package fakegithub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// obj is a JSON object of a REST answer.
type obj = map[string]any

// apiError is a REST refusal: its status and GitHub's message.
type apiError struct {
	Status  int
	Message string
	// Errors are a validation's messages.
	Errors []string
	cause  error
}

func (e *apiError) Error() string {
	msg := e.Message
	if len(e.Errors) > 0 {
		msg += ": " + strings.Join(e.Errors, ", ")
	}
	if e.cause != nil {
		msg += " (" + e.cause.Error() + ")"
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, msg)
}

func (e *apiError) Unwrap() error { return e.cause }

// body is the error as GitHub writes it.
func (e *apiError) body() obj {
	out := obj{"message": e.Message, "documentation_url": "https://docs.github.com/rest", "status": strconv.Itoa(e.Status)}
	if len(e.Errors) > 0 {
		errs := make([]any, 0, len(e.Errors))
		for _, m := range e.Errors {
			errs = append(errs, obj{"resource": "Resource", "code": "custom", "message": m})
		}
		out["errors"] = errs
	}
	return out
}

func errNotFound() *apiError { return &apiError{Status: http.StatusNotFound, Message: "Not Found"} }

func errInvalid(format string, args ...any) *apiError {
	return &apiError{Status: http.StatusUnprocessableEntity, Message: "Validation Failed", Errors: []string{fmt.Sprintf(format, args...)}}
}

// rawBody is an answer that is no JSON: a file's content, a diff, a log.
type rawBody struct {
	ContentType string
	Data        []byte
}

// restCtx is one REST request as a route's handler reads it.
type restCtx struct {
	s      *Server
	m      *Model
	req    *http.Request
	params map[string]string
	query  url.Values
	body   obj
	raw    []byte
	// status is the answer's status, 200 unless the handler sets it.
	status int
	header http.Header
	// base is what the answer's links start with: the API as the caller
	// reached it.
	base string
}

func (c *restCtx) param(name string) string { return c.params[name] }

func (c *restCtx) num(name string) int {
	n, _ := strconv.Atoi(c.params[name])
	return n
}

// str is a body field's string.
func (c *restCtx) str(name string) string {
	switch v := c.body[name].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

func (c *restCtx) has(name string) bool {
	v, ok := c.body[name]
	return ok && v != nil
}

func (c *restCtx) flag(name string) bool {
	switch v := c.body[name].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	}
	return false
}

// strs is a body field's list of strings; a string is a list of one.
func (c *restCtx) strs(name string) []string {
	switch v := c.body[name].(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			switch s := e.(type) {
			case string:
				out = append(out, s)
			case map[string]any:
				if n, ok := s["name"].(string); ok {
					out = append(out, n)
				}
			}
		}
		return out
	}
	return nil
}

// accepts says whether the request's Accept names a media type.
func (c *restCtx) accepts(media string) bool {
	return strings.Contains(strings.ToLower(c.req.Header.Get("Accept")), media)
}

func (c *restCtx) repo() (*Repo, error) {
	r, ok := c.m.Repo(c.param("owner") + "/" + c.param("repo"))
	if !ok {
		return nil, errNotFound()
	}
	return r, nil
}

func (c *restCtx) issue() (*Repo, *Issue, error) {
	r, err := c.repo()
	if err != nil {
		return nil, nil, err
	}
	i, ok := r.Issue(c.num("number"))
	if !ok {
		return r, nil, errNotFound()
	}
	return r, i, nil
}

func (c *restCtx) pull() (*Repo, *Pull, error) {
	r, err := c.repo()
	if err != nil {
		return nil, nil, err
	}
	p, ok := r.Pull(c.num("number"))
	if !ok {
		return r, nil, errNotFound()
	}
	return r, p, nil
}

// subject is the issue or pull request of the path's number: REST serves
// a pull request's comments and labels as its issue's.
func (c *restCtx) subject() (*Repo, *Issue, *Pull, error) {
	r, err := c.repo()
	if err != nil {
		return nil, nil, nil, err
	}
	if i, ok := r.Issue(c.num("number")); ok {
		return r, i, nil, nil
	}
	if p, ok := r.Pull(c.num("number")); ok {
		return r, nil, p, nil
	}
	return r, nil, nil, errNotFound()
}

// commit resolves the path's ref to a commit of the repository.
func (c *restCtx) commit(name string) (*Repo, string, error) {
	r, err := c.repo()
	if err != nil {
		return nil, "", err
	}
	sha, ok := c.m.Commit(r, c.param(name))
	if !ok {
		return r, "", &apiError{Status: http.StatusUnprocessableEntity, Message: "No commit found for SHA: " + c.param(name)}
	}
	return r, sha, nil
}

// perPage is the request's page size (30, at most 100) and page.
func (c *restCtx) perPage() (size, page int) {
	size, page = 30, 1
	if n, err := strconv.Atoi(c.query.Get("per_page")); err == nil && n > 0 {
		size = min(n, 100)
	}
	if n, err := strconv.Atoi(c.query.Get("page")); err == nil && n > 0 {
		page = n
	}
	return size, page
}

// page cuts a list to the request's page and links the others.
func page[T any](c *restCtx, items []T) []T {
	size, n := c.perPage()
	last := max(1, (len(items)+size-1)/size)
	link := func(p int, rel string) string {
		q := url.Values{}
		for k, v := range c.query {
			q[k] = v
		}
		q.Set("page", strconv.Itoa(p))
		q.Set("per_page", strconv.Itoa(size))
		return fmt.Sprintf(`<%s%s?%s>; rel="%s"`, c.base, c.req.URL.Path, q.Encode(), rel)
	}
	var links []string
	if n > 1 {
		links = append(links, link(n-1, "prev"))
	}
	if n < last {
		links = append(links, link(n+1, "next"), link(last, "last"))
	}
	if n > 1 {
		links = append(links, link(1, "first"))
	}
	if len(links) > 0 {
		c.header.Set("Link", strings.Join(links, ", "))
	}
	from := min((n-1)*size, len(items))
	return items[from:min(from+size, len(items))]
}

// list is a page of items as JSON values.
func list[T any](c *restCtx, items []T, shape func(T) obj) []any {
	out := []any{}
	for _, it := range page(c, items) {
		out = append(out, shape(it))
	}
	return out
}

// route is one REST route: a method, a path pattern and its handler. A
// pattern's {name} is one segment, {name...} one or more.
type route struct {
	method  string
	pattern string
	segs    []string
	bucket  string
	handle  func(c *restCtx) (any, error)
}

func (rt *route) match(segs []string) (map[string]string, bool) {
	params := map[string]string{}
	i := 0
	for at, pat := range rt.segs {
		switch {
		case strings.HasSuffix(pat, "...}"):
			rest := len(rt.segs) - at - 1
			take := len(segs) - i - rest
			if take < 1 {
				return nil, false
			}
			params[pat[1:len(pat)-4]] = strings.Join(segs[i:i+take], "/")
			i += take
		case i >= len(segs):
			return nil, false
		case strings.HasPrefix(pat, "{"):
			params[pat[1:len(pat)-1]] = segs[i]
			i++
		case pat != segs[i]:
			return nil, false
		default:
			i++
		}
	}
	return params, i == len(segs)
}

var routes []*route

func handle(method, pattern string, fn func(c *restCtx) (any, error)) *route {
	rt := &route{method: method, pattern: pattern, segs: strings.Split(strings.Trim(pattern, "/"), "/"), bucket: bucketCore, handle: fn}
	routes = append(routes, rt)
	return rt
}

// findRoute is the route of a request, and whether its path is one the
// model serves with another method.
func findRoute(method, path string) (*route, map[string]string, bool) {
	raw := strings.Split(strings.Trim(path, "/"), "/")
	segs := make([]string, 0, len(raw))
	for _, s := range raw {
		if u, err := url.PathUnescape(s); err == nil {
			s = u
		}
		segs = append(segs, s)
	}
	known := false
	for _, rt := range routes {
		params, ok := rt.match(segs)
		if !ok {
			continue
		}
		known = true
		if rt.method == method || rt.method == http.MethodGet && method == http.MethodHead {
			return rt, params, true
		}
	}
	return nil, nil, known
}

// decodeBody reads a request's JSON object; an empty body is an empty
// object.
func decodeBody(raw []byte) (obj, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return obj{}, nil
	}
	var out obj
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &apiError{Status: http.StatusBadRequest, Message: "Problems parsing JSON"}
	}
	return out, nil
}
