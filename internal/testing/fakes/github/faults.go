package fakegithub

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/forge"
	"github.com/giantswarm/beekeeper/internal/forge/github/wire"
)

// Fault kinds of github_fault.
const (
	faultHTTP       = "http_error"
	faultSecondary  = "secondary_rate_limit"
	faultRateLimit  = "rate_limit"
	faultDeliveries = "deliveries"
	faultClear      = "clear"
)

// fault answers matching calls with an error instead of the real answer.
type fault struct {
	kind       string
	status     int
	match      string
	api        string
	times      int // 0: until clear
	retryAfter time.Duration
}

// pathMatches is github_calls' path rule: a prefix match by path segment, where * stands for one
// segment. The query is not part of the path.
func pathMatches(pattern, path string) bool {
	pattern = strings.Trim(pattern, "/")
	if pattern == "" {
		return true
	}
	ps := strings.Split(pattern, "/")
	xs := strings.Split(strings.Trim(path, "/"), "/")
	if len(ps) > len(xs) {
		return false
	}
	for i, p := range ps {
		if p != "*" && p != xs[i] {
			return false
		}
	}
	return true
}

// matches applies the fault's match: a path rule, or exactly a GraphQL operation name.
func (f *fault) matches(c *Call) bool {
	if f.api != "" && f.api != c.API {
		return false
	}
	if f.match == "" {
		return true
	}
	if c.API == forge.APIGraphQL && c.Operation == f.match {
		return true
	}
	return strings.HasPrefix(f.match, "/") && pathMatches(f.match, c.Path)
}

// matchFault returns the first fault that applies to c, spending one of its times.
func (s *Server) matchFault(c *Call) *fault {
	for i, f := range s.faults {
		if !f.matches(c) {
			continue
		}
		if f.times > 0 {
			f.times--
			if f.times == 0 {
				s.faults = append(s.faults[:i], s.faults[i+1:]...)
			}
		}
		return f
	}
	return nil
}

func (f *fault) reply() reply {
	if f.kind == faultSecondary {
		rp := errReply(http.StatusForbidden, wire.SecondaryRateLimitMessage+". Please wait a few minutes before you try again.")
		rp.header.Set(wire.HeaderRetryAfter, strconv.Itoa(int(f.retryAfter/time.Second)))
		return rp
	}
	msg := http.StatusText(f.status)
	if msg == "" {
		msg = "error"
	}
	return errReply(f.status, msg)
}
