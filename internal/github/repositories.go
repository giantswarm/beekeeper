package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Repository is the identity returned by a read-only repository probe.
type Repository struct {
	NameWithOwner string `json:"nameWithOwner"`
	Owner         struct {
		Type string `json:"__typename"`
	} `json:"owner"`
}

// Repositories resolves repositories using the same per-owner routing as
// Lookup and RefStates. Missing repositories and GraphQL errors fail the probe.
func Repositories(ctx context.Context, run GH, repos []string) ([]Repository, error) {
	for _, repo := range repos {
		parts := strings.Split(repo, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(repo, " \t\r\n#") {
			return nil, fmt.Errorf("%q is not owner/repo", repo)
		}
	}
	var resolved []Repository
	for _, group := range ByOwner(repos, func(repo string) string { return repo }) {
		var q strings.Builder
		q.WriteString("query{")
		for i, repo := range group {
			owner, name, _ := strings.Cut(repo, "/")
			fmt.Fprintf(&q, "r%d:repository(owner:%s,name:%s){nameWithOwner owner{__typename}}", i, quote(owner), quote(name))
		}
		q.WriteString("}")
		raw, err := run(ActingOn(ctx, group[0]), "api", "graphql", "-f", "query="+q.String())
		if err != nil {
			return nil, err
		}
		var doc struct {
			Data   map[string]*Repository `json:"data"`
			Errors []json.RawMessage      `json:"errors"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("repository probe: %w", err)
		}
		if len(doc.Errors) > 0 {
			return nil, fmt.Errorf("repository probe: GraphQL returned errors")
		}
		for i, repo := range group {
			r := doc.Data[fmt.Sprintf("r%d", i)]
			if r == nil || r.NameWithOwner == "" || r.Owner.Type == "" {
				return nil, fmt.Errorf("repository probe: could not resolve %s", repo)
			}
			resolved = append(resolved, *r)
		}
	}
	return resolved, nil
}
