package wire

import (
	"net/http"
	"strings"

	"github.com/giantswarm/beekeeper/internal/forge"
)

// REST operation ids that only the fake GitHub needs (the client never calls them).
const (
	OpCombinedStatus   = "repos/get-combined-status-for-ref"
	OpRedeliverWebhook = "apps/redeliver-webhook-delivery"
)

// Route is a REST route with its operation id from GitHub's OpenAPI description.
type Route struct {
	Method string
	// Pattern has {name} segments; a final {name...} takes the rest of the path.
	Pattern   string
	Operation string
	API       string
	// Mutating routes change state on GitHub. Token minting is not one: it changes nothing.
	Mutating bool
}

// Routes are the REST routes of the subset of GitHub's API the engine reads, plus the one App
// endpoint that would write (redelivery), so that a call to it is recognised and labelled.
var Routes = []Route{
	{Method: http.MethodGet, Pattern: "/repos/{owner}/{repo}", Operation: forge.OpReposGet, API: forge.APIREST},
	{Method: http.MethodGet, Pattern: "/repos/{owner}/{repo}/issues/{number}", Operation: forge.OpIssuesGet, API: forge.APIREST},
	{Method: http.MethodGet, Pattern: "/repos/{owner}/{repo}/pulls/{number}", Operation: forge.OpPullsGet, API: forge.APIREST},
	{Method: http.MethodGet, Pattern: "/repos/{owner}/{repo}/pulls/{number}/reviews", Operation: forge.OpPullsListReviews, API: forge.APIREST},
	{Method: http.MethodGet, Pattern: "/repos/{owner}/{repo}/commits/{ref}/check-runs", Operation: forge.OpChecksListForRef, API: forge.APIREST},
	{Method: http.MethodGet, Pattern: "/repos/{owner}/{repo}/commits/{ref}/statuses", Operation: forge.OpStatusesListForRef, API: forge.APIREST},
	{Method: http.MethodGet, Pattern: "/repos/{owner}/{repo}/commits/{ref}/status", Operation: OpCombinedStatus, API: forge.APIREST},
	{Method: http.MethodGet, Pattern: "/repos/{owner}/{repo}/rules/branches/{branch...}", Operation: forge.OpReposGetBranchRules, API: forge.APIREST},
	{Method: http.MethodGet, Pattern: "/installation/repositories", Operation: forge.OpListInstallationRepos, API: forge.APIREST},
	{Method: http.MethodGet, Pattern: "/app/hook/deliveries", Operation: forge.OpListWebhookDeliveries, API: forge.APIApp},
	{Method: http.MethodGet, Pattern: "/app/hook/deliveries/{id}", Operation: forge.OpGetWebhookDelivery, API: forge.APIApp},
	{Method: http.MethodPost, Pattern: "/app/hook/deliveries/{id}/attempts", Operation: OpRedeliverWebhook, API: forge.APIApp, Mutating: true},
	{Method: http.MethodPost, Pattern: "/app/installations/{id}/access_tokens", Operation: forge.OpCreateInstallToken, API: forge.APIApp},
}

// GraphQLPath is where GraphQL is served, relative to the API's base URL.
const GraphQLPath = "/graphql"

// MatchRoute finds the route of a request and the values of its {name} segments.
func MatchRoute(method, path string) (Route, map[string]string, bool) {
	for _, r := range Routes {
		if r.Method != method {
			continue
		}
		if params, ok := matchPattern(r.Pattern, path); ok {
			return r, params, true
		}
	}
	return Route{}, nil, false
}

func matchPattern(pattern, path string) (map[string]string, bool) {
	ps := strings.Split(strings.Trim(pattern, "/"), "/")
	xs := strings.Split(strings.Trim(path, "/"), "/")
	params := map[string]string{}
	for i, p := range ps {
		if strings.HasPrefix(p, "{") && strings.HasSuffix(p, "...}") {
			if i >= len(xs) {
				return nil, false
			}
			params[strings.TrimSuffix(strings.TrimPrefix(p, "{"), "...}")] = strings.Join(xs[i:], "/")
			return params, true
		}
		if i >= len(xs) {
			return nil, false
		}
		if strings.HasPrefix(p, "{") && strings.HasSuffix(p, "}") {
			if xs[i] == "" {
				return nil, false
			}
			params[strings.Trim(p, "{}")] = xs[i]
			continue
		}
		if p != xs[i] {
			return nil, false
		}
	}
	if len(xs) != len(ps) {
		return nil, false
	}
	return params, true
}

// APIOf names the API (and so the rate-limit bucket) a path belongs to: GraphQL, the App's own
// endpoints (authenticated with the App's JWT), or REST with the installation's token.
func APIOf(path string) string {
	switch {
	case path == GraphQLPath:
		return forge.APIGraphQL
	case path == "/app" || strings.HasPrefix(path, "/app/"):
		return forge.APIApp
	default:
		return forge.APIREST
	}
}
