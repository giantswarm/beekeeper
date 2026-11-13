//go:build scenario

package fakegithub

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a Go duration string in a scenario ("30s", "5m"), never
// negative.
type Duration struct{ time.Duration }

// UnmarshalYAML parses a duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	if v < 0 {
		return fmt.Errorf("line %d: duration %q is negative", n.Line, s)
	}
	d.Duration = v
	return nil
}

// MarshalYAML writes the duration string.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// Fault is one fault of the scenario's GitHub: the requests Match holds
// are answered as Respond says, Times times (0: every time) and, with
// For, only that long after the fault was armed.
type Fault struct {
	// Name is what a fault step clears it by.
	Name    string       `yaml:"name"`
	Match   FaultMatch   `yaml:"match"`
	Respond FaultRespond `yaml:"respond"`
	Times   int          `yaml:"times"`
	For     Duration     `yaml:"for"`

	match   compiledMatch
	used    int
	armedAt time.Time
}

// FaultMatch picks requests: every field set must hold. Method, Path,
// Operation and Caller are regular expressions that match whole. Path is
// the request's path without its leading slash and query
// ("repos/example/app/pulls", "graphql"); Operation a GraphQL operation's
// name or one of its top-level fields ("repositoryOwner"); Caller who
// sent it. Conditional picks the requests with (true) or without (false)
// If-None-Match; Via those through the gh CLI ("gh") or direct ("http").
// MinFields picks GraphQL requests with at least that many top-level
// fields, each alias counted: a batch too large for GitHub to answer in
// time.
type FaultMatch struct {
	Method      string `yaml:"method"`
	Path        string `yaml:"path"`
	Operation   string `yaml:"operation"`
	Caller      string `yaml:"caller"`
	Conditional *bool  `yaml:"conditional"`
	Via         string `yaml:"via"`
	MinFields   int    `yaml:"min_fields"`
}

// FaultRespond is a fault's answer. Status answers with that status
// (Body, of ContentType or JSON, or GitHub's usual body for it);
// SecondaryRateLimit with GitHub's 403 for a secondary rate limit; Outage
// drops the connection without an answer. RetryAfter adds the Retry-After
// header. Delay holds the answer that long in real time; a fault of Delay
// alone is a slow answer of the real one.
type FaultRespond struct {
	Status             int      `yaml:"status"`
	Body               string   `yaml:"body"`
	ContentType        string   `yaml:"content_type"`
	RetryAfter         Duration `yaml:"retry_after"`
	Delay              Duration `yaml:"delay"`
	SecondaryRateLimit bool     `yaml:"secondary_rate_limit"`
	Outage             bool     `yaml:"outage"`
}

type compiledMatch struct {
	method, path, operation, caller *regexp.Regexp
}

func whole(pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, nil
	}
	return regexp.Compile(`(?s)^(?:` + pattern + `)$`)
}

var faultNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// clearAll is the fault step's name for every fault.
const clearAll = "all"

// Validate checks a fault and compiles its patterns.
func (f *Fault) Validate() error {
	var errs []error
	if f.Name != "" && (!faultNameRE.MatchString(f.Name) || f.Name == clearAll) {
		errs = append(errs, fmt.Errorf("name %q: lower-case letters, digits and dashes, and not %q", f.Name, clearAll))
	}
	for field, pattern := range map[string]string{"method": f.Match.Method, "path": f.Match.Path, "operation": f.Match.Operation, "caller": f.Match.Caller} {
		re, err := whole(pattern)
		if err != nil {
			errs = append(errs, fmt.Errorf("match.%s %q: %w", field, pattern, err))
			continue
		}
		switch field {
		case "method":
			f.match.method = re
		case "path":
			f.match.path = re
		case "operation":
			f.match.operation = re
		case "caller":
			f.match.caller = re
		}
	}
	if v := f.Match.Via; v != "" && v != viaGH && v != viaHTTP {
		errs = append(errs, fmt.Errorf("match.via %q: %s or %s", v, viaGH, viaHTTP))
	}
	r := f.Respond
	kinds := 0
	for _, set := range []bool{r.Status != 0, r.SecondaryRateLimit, r.Outage} {
		if set {
			kinds++
		}
	}
	switch {
	case kinds > 1:
		errs = append(errs, errors.New("respond: at most one of status, secondary_rate_limit and outage"))
	case kinds == 0 && r.Delay.Duration == 0:
		errs = append(errs, errors.New("respond: one of status, secondary_rate_limit, outage or delay"))
	}
	if r.Status != 0 && (r.Status < 200 || r.Status > 599) {
		errs = append(errs, fmt.Errorf("respond.status %d is no HTTP status", r.Status))
	}
	if r.Body != "" && r.Status == 0 {
		errs = append(errs, errors.New("respond.body needs respond.status"))
	}
	if r.ContentType != "" && r.Body == "" {
		errs = append(errs, errors.New("respond.content_type needs respond.body"))
	}
	if f.Match.MinFields < 0 {
		errs = append(errs, fmt.Errorf("match.min_fields %d", f.Match.MinFields))
	}
	if f.Times < 0 {
		errs = append(errs, fmt.Errorf("times %d", f.Times))
	}
	return errors.Join(errs...)
}

// active says whether the fault still applies at now.
func (f *Fault) active(now time.Time) bool {
	if f.Times > 0 && f.used >= f.Times {
		return false
	}
	return f.For.Duration == 0 || now.Before(f.armedAt.Add(f.For.Duration))
}

// holds says whether the fault's match holds a request.
func (f *Fault) holds(req *Request, op *gqlOperation) bool {
	m := f.match
	if m.method != nil && !m.method.MatchString(req.Method) {
		return false
	}
	if m.path != nil && !m.path.MatchString(trimSlash(req.Path)) {
		return false
	}
	if m.caller != nil && !m.caller.MatchString(req.Caller) {
		return false
	}
	if v := f.Match.Via; v != "" && v != req.Via {
		return false
	}
	if c := f.Match.Conditional; c != nil && *c != (req.Header.Get("If-None-Match") != "") {
		return false
	}
	if n := f.Match.MinFields; n > 0 && (op == nil || op.Fields < n) {
		return false
	}
	if m.operation != nil {
		if op == nil {
			return false
		}
		matched := op.Op.Name != "" && m.operation.MatchString(op.Op.Name)
		for _, root := range op.Roots {
			matched = matched || m.operation.MatchString(root)
		}
		if !matched {
			return false
		}
	}
	return true
}

func trimSlash(p string) string {
	for len(p) > 0 && p[0] == '/' {
		p = p[1:]
	}
	return p
}

// secondaryMessage is GitHub's refusal under a secondary rate limit.
const secondaryMessage = "You have exceeded a secondary rate limit. Please wait a few minutes before you try again."

// secondaryResponse is GitHub's 403 for a secondary rate limit.
func secondaryResponse(retryAfter time.Duration) *Response {
	resp := jsonResponse(http.StatusForbidden, obj{
		"message":           secondaryMessage,
		"documentation_url": "https://docs.github.com/rest/overview/rate-limits-for-the-rest-api#about-secondary-rate-limits",
	})
	if retryAfter > 0 {
		resp.Header.Set("Retry-After", strconv.Itoa(int(retryAfter.Round(time.Second).Seconds())))
	}
	return resp
}

// response is the fault's answer; nil for a fault of delay alone.
func (f *Fault) response() *Response {
	r := f.Respond
	switch {
	case r.Outage:
		return &Response{Outage: true, Header: http.Header{}}
	case r.SecondaryRateLimit:
		return secondaryResponse(r.RetryAfter.Duration)
	case r.Status == 0:
		return nil
	}
	var resp *Response
	switch {
	case r.Body != "":
		resp = &Response{Status: r.Status, Header: http.Header{}, Body: []byte(r.Body)}
		resp.Header.Set("Content-Type", cmpOr(r.ContentType, "application/json; charset=utf-8"))
	case r.Status == http.StatusNotModified || r.Status == http.StatusNoContent:
		resp = &Response{Status: r.Status, Header: http.Header{}}
	case r.Status >= 400:
		resp = jsonResponse(r.Status, obj{"message": statusMessage(r.Status), "documentation_url": "https://docs.github.com/rest", "status": strconv.Itoa(r.Status)})
	default:
		resp = jsonResponse(r.Status, obj{})
	}
	if r.RetryAfter.Duration > 0 {
		resp.Header.Set("Retry-After", strconv.Itoa(int(r.RetryAfter.Round(time.Second).Seconds())))
	}
	return resp
}

// statusMessage is GitHub's message for a status.
func statusMessage(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "Bad credentials"
	case http.StatusNotFound:
		return "Not Found"
	case http.StatusInternalServerError:
		return "Server Error"
	}
	return http.StatusText(status)
}
