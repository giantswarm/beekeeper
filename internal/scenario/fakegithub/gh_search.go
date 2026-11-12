//go:build scenario

package fakegithub

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// gh search prs, as the supervisor loop's sweep lists the desk's open pull
// requests: gh turns its flags into search qualifiers, asks REST's
// search/issues page by page and exports the asked fields.

// searchPRFields are the --json fields of gh search prs the translator
// serves; any other is an unanswered call.
var searchPRFields = []string{"body", "closedAt", "createdAt", "id", "isDraft", "isPullRequest", "number", "repository", "state", "title", "updatedAt", "url"}

// searchPRsQuery is the search gh writes for the flags of gh search prs:
// the keywords, then the qualifiers in the order of their names.
func searchPRsQuery(f flags) string {
	parts := slices.Clone(f.pos)
	add := func(key string, values ...string) {
		for _, v := range values {
			if strings.ContainsAny(v, " \t") {
				v = strconv.Quote(v)
			}
			parts = append(parts, key+":"+v)
		}
	}
	if v := f.str("author"); v != "" {
		add("author", v)
	}
	add("repo", f.all("repo")...)
	if v := f.str("state"); v != "" {
		add("state", v)
	}
	add("type", "pr")
	add("user", f.all("owner")...)
	return strings.Join(parts, " ")
}

func (g *ghRun) searchPRs(args []string) error {
	f, err := parseFlags("search prs", args, nil, append([]string{"author", "owner", "repo|R", "state", "limit|L"}, jsonFlags...))
	if err != nil {
		return err
	}
	if !f.has("json") {
		return unhandled("the fake gh translates gh search prs with --json only")
	}
	asked, err := g.fields(f, searchPRFields)
	if err != nil {
		return err
	}
	if state := f.str("state"); state != "" && state != "open" && state != "closed" {
		return fmt.Errorf("invalid argument %q for \"--state\" flag: valid values are {open|closed}", state)
	}
	limit := 30
	if f.has("limit") {
		if limit, err = strconv.Atoi(f.str("limit")); err != nil || limit < 1 || limit > 1000 {
			return fmt.Errorf("`--limit` must be between 1 and 1000")
		}
	}
	out := []any{}
	for page := 1; len(out) < limit; page++ {
		query := url.Values{}
		query.Set("page", strconv.Itoa(page))
		query.Set("per_page", strconv.Itoa(min(limit, 100)))
		query.Set("q", searchPRsQuery(f))
		var resp struct {
			Total int    `json:"total_count"`
			Items []node `json:"items"`
		}
		if err := g.rest(http.MethodGet, "/search/issues", query, nil, &resp); err != nil {
			return err
		}
		for _, item := range resp.Items {
			if len(out) < limit {
				out = append(out, exportSearchPR(item, asked))
			}
		}
		if len(resp.Items) == 0 || page*min(limit, 100) >= resp.Total {
			break
		}
	}
	return g.emit(f, out)
}

// exportSearchPR is a search result with exactly the asked fields, as gh
// exports it: the repository is named by the result's repository URL.
func exportSearchPR(item node, fields []string) node {
	pull := child(item, "pull_request")
	out := node{}
	for _, field := range fields {
		switch field {
		case "body", "id", "number", "title":
			out[field] = item[field]
		case "closedAt":
			out[field] = item["closed_at"]
		case "createdAt":
			out[field] = item["created_at"]
		case "updatedAt":
			out[field] = item["updated_at"]
		case "url":
			out[field] = item["html_url"]
		case "isDraft":
			out[field] = item["draft"] == true
		case "isPullRequest":
			out[field] = pull != nil
		case "state":
			state := text(item, "state")
			if pull != nil && pull["merged_at"] != nil {
				state = "merged"
			}
			out[field] = state
		case "repository":
			parts := strings.Split(text(item, "repository_url"), "/")
			name, full := "", ""
			if n := len(parts); n >= 2 {
				name, full = parts[n-1], parts[n-2]+"/"+parts[n-1]
			}
			out[field] = node{"name": name, "nameWithOwner": full}
		}
	}
	return out
}
