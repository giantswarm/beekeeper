package sandbox

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type policyTransport func(*http.Request) (*http.Response, error)

func (f policyTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEgressOrganizationWritePolicy(t *testing.T) {
	for _, tc := range []struct {
		name, host, method, path, body string
		allow                          bool
	}{
		{"foreign REST", hostAPI, "POST", "/repos/outside/repo/issues", `{}`, false},
		{"path traversal", hostAPI, "POST", "/repos/team/../outside/issues", `{}`, false},
		{"encoded separator", hostAPI, "POST", "/repos/team%2f..%2foutside/issues", `{}`, false},
		{"encoded traversal", hostAPI, "POST", "/repos/team/%2e%2e/outside/issues", `{}`, false},
		{"allowed REST", hostAPI, "POST", "/repos/TEAM/repo/issues", `{}`, true},
		{"foreign org", hostAPI, "PATCH", "/orgs/outside", `{}`, false},
		{"allowed org", hostAPI, "PATCH", "/orgs/team", `{}`, true},
		{"foreign push", hostGitHub, "POST", "/outside/repo.git/git-receive-pack", "pack", false},
		{"allowed push", hostGitHub, "POST", "/team/repo.git/git-receive-pack", "pack", true},
		{"foreign fetch", hostGitHub, "POST", "/outside/repo.git/git-upload-pack", "pack", true},
		{"foreign read", hostAPI, "GET", "/repos/outside/repo/issues", "", true},
		{"allowed upload", "uploads.github.com", "POST", "/repos/team/repo/releases/1/assets", "asset", true},
		{"foreign upload", "uploads.github.com", "POST", "/repos/outside/repo/releases/1/assets", "asset", false},
		{"unknown write", hostAPI, "POST", "/user/repos", `{}`, false},
		{"markdown", hostAPI, "POST", "/markdown", `{}`, true},
		{"GraphQL read", hostAPI, "POST", "/graphql", `{"query":"query { viewer { login } }"}`, true},
		{"GraphQL mutation", hostAPI, "POST", "/graphql", `{"query":"mutation { addComment(input: {subjectId: \"opaque\", body: \"x\"}) { clientMutationId } }"}`, false},
		{"GraphQL GET mutation", hostAPI, "GET", "/graphql?query=mutation%7Bwrite%7D", "", false},
		{"GraphQL malformed", hostAPI, "POST", "/graphql", `{`, false},
		{"GraphQL oversize", hostAPI, "POST", "/graphql", strings.Repeat("x", (1<<20)+1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			e := &Egress{AllowedOrgs: []string{"team"}, Token: func() string { return "broker-secret" }, Upstream: policyTransport(func(r *http.Request) (*http.Response, error) {
				called = true
				if r.Header.Get("Authorization") != Authorization(tc.host, "broker-secret") {
					t.Error("broker token missing")
				}
				var b []byte
				if r.Body != nil {
					b, _ = io.ReadAll(r.Body)
				}
				if string(b) != tc.body {
					t.Error("request body changed")
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
			})}
			r := httptest.NewRequest(tc.method, "https://"+tc.host+tc.path, strings.NewReader(tc.body))
			r = r.WithContext(context.WithValue(r.Context(), targetKey{}, tc.host))
			w := httptest.NewRecorder()
			e.injector().ServeHTTP(w, r)
			if called != tc.allow {
				t.Fatalf("forwarded = %t; response %d: %s", called, w.Code, w.Body.String())
			}
			if !tc.allow && (w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "github.allowedOrgs (team)")) {
				t.Fatalf("refusal = %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestGraphQLReadClassification(t *testing.T) {
	for _, tc := range []struct {
		query string
		read  bool
	}{
		{`{ viewer { login } }`, true},
		{"# mutation ignored\nquery { viewer { login } }", true},
		{`query { repository(name: "mutation", owner: "team") { id } }`, true},
		{`query { repository(name: """mutation""", owner: "team") { id } }`, true},
		{`query Read { viewer { login } } mutation Write { write }`, false},
		{`# query only`, false},
		{`mutation { write }`, false},
		{`query { field(arg: "unterminated) }`, false},
	} {
		if got := graphqlRead(tc.query); got != tc.read {
			t.Errorf("%q: read = %t", tc.query, got)
		}
	}
}
