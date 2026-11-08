package sandbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"unicode"
)

const (
	upstreamAPIHost = "api.github.com"
	upstreamGitHost = "github.com"
)

// githubWrite checks the actual request before any broker token is attached.
// Unknown write targets fail closed; GraphQL mutations identify their targets
// by opaque node IDs, so clients must use the owner-scoped REST endpoints.
func (e *Egress) githubWrite(r *http.Request, host string) error {
	if len(e.AllowedOrgs) == 0 && len(e.Visibility.PublicOnlyOwners) == 0 {
		return nil
	}
	deny := func() error {
		return fmt.Errorf("write refused: target must belong to github.allowedOrgs (%s); use an owner-scoped REST endpoint for mutations", strings.Join(e.AllowedOrgs, ", "))
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if host == upstreamAPIHost && len(parts) == 1 && parts[0] == "graphql" {
		// Check even GET: a query string must not smuggle a mutation.
		query := r.URL.Query().Get("query")
		if r.Method == http.MethodPost {
			const bound = 1 << 20
			body, err := io.ReadAll(io.LimitReader(r.Body, bound+1))
			if err != nil || len(body) > bound {
				return deny()
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			var payload struct {
				Query string `json:"query"`
			}
			if json.Unmarshal(body, &payload) != nil {
				return deny()
			}
			query = payload.Query
		} else if r.Method != http.MethodGet {
			return deny()
		}
		if !graphqlRead(query) {
			return deny()
		}
		return nil
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return nil
	}
	// Never authorize one spelling and forward a path the upstream can
	// normalize into another owner (including encoded path separators).
	escaped := strings.ToLower(r.URL.EscapedPath())
	if strings.ContainsAny(r.URL.Path, `\`) || strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%5c") {
		return deny()
	}
	for _, part := range parts {
		if part == "." || part == ".." || part == "" {
			return deny()
		}
	}
	owner, repo := "", ""
	switch host {
	case upstreamAPIHost, "uploads.github.com":
		if len(parts) >= 2 && (parts[0] == "repos" || parts[0] == "orgs") {
			owner = parts[1]
			if parts[0] == "repos" && len(parts) >= 3 {
				repo = parts[2]
			}
		}
		// Rendering Markdown has no persistent side effects.
		if r.Method == http.MethodPost && (r.URL.Path == "/markdown" || r.URL.Path == "/markdown/raw") {
			return nil
		}
	case upstreamGitHost:
		// Git's smart HTTP protocol uses POST for both fetch and push.
		if len(parts) == 3 && parts[2] == "git-upload-pack" {
			return nil
		}
		if len(parts) == 3 && parts[2] == "git-receive-pack" {
			owner = parts[0]
			repo = strings.TrimSuffix(parts[1], ".git")
		}
	}
	if owner != "" && (len(e.AllowedOrgs) == 0 || slices.ContainsFunc(e.AllowedOrgs, func(o string) bool { return strings.EqualFold(owner, o) })) {
		return e.Visibility.Check(owner + "/" + repo)
	}
	return deny()
}

// graphqlRead accepts read operations, ignoring quoted strings and comments.
// A document containing any mutation is refused even if operationName selects
// a different operation. Block strings are handled before ordinary strings.
func graphqlRead(query string) bool {
	first := ""
	for i := 0; i < len(query); {
		c := query[i]
		if unicode.IsSpace(rune(c)) || c == ',' {
			i++
			continue
		}
		if c == '#' {
			for i < len(query) && query[i] != '\n' {
				i++
			}
			continue
		}
		if c == '"' {
			if strings.HasPrefix(query[i:], `"""`) {
				i += 3
				closed := false
				for i < len(query) {
					if strings.HasPrefix(query[i:], `\"""`) {
						i += 4
						continue
					}
					if strings.HasPrefix(query[i:], `"""`) {
						i += 3
						closed = true
						break
					}
					i++
				}
				if !closed {
					return false
				}
			} else {
				i++
				closed := false
				for i < len(query) {
					if query[i] == '\\' {
						i += 2
						continue
					}
					if query[i] == '"' {
						i++
						closed = true
						break
					}
					i++
				}
				if !closed {
					return false
				}
			}
			continue
		}
		if c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' {
			start := i
			for i < len(query) && (query[i] == '_' || query[i] >= 'A' && query[i] <= 'Z' || query[i] >= 'a' && query[i] <= 'z' || query[i] >= '0' && query[i] <= '9') {
				i++
			}
			word := query[start:i]
			if first == "" {
				first = word
			}
			if word == "mutation" || word == "subscription" {
				return false
			}
			continue
		}
		if first == "" {
			first = string(c)
		}
		i++
	}
	return first == "query" || first == "{" || first == "fragment"
}
