// Package findings places worker findings without creating comment trails.
package findings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/giantswarm/beekeeper/internal/github"
)

// Report names the owner and the current description, never an observation log.
// A reference to an existing issue is authoritative; a repository alone uses
// the title to identify the finding among all its open issues.
type Report struct {
	Repo   string
	Number int
	Title  string
	Body   string
}

// ErrUnplaced leaves ambiguous ownership to the guide, without a mutation.
var ErrUnplaced = errors.New("finding needs disambiguation")

const apiCommand = "api"

var header = regexp.MustCompile(`^(?:https://github\.com/)?([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)(?:/issues/([0-9]+))?:[ \t]+([^\n]+)\n+([\s\S]+)$`)

// Parse accepts an explicit owner, stable title and complete current body.
// Free prose and incidental references are left for the guide to disambiguate.
func Parse(text string) (Report, bool) {
	m := header.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return Report{}, false
	}
	r := Report{Repo: m[1], Title: strings.TrimSpace(m[3]), Body: strings.TrimSpace(m[4])}
	if m[2] != "" {
		n, err := strconv.Atoi(m[2])
		if err != nil || n <= 0 {
			return Report{}, false
		}
		r.Number = n
	}
	return r, r.Title != "" && r.Body != ""
}

type issue struct {
	Number int             `json:"number"`
	Title  string          `json:"title"`
	Body   string          `json:"body"`
	Pull   json.RawMessage `json:"pull_request"`
}

func normalized(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

// Place reads the entire open inventory before any write. Re-reading it on a
// retry recovers an issue created before an interrupted board intake. Nothing
// here posts comments. An ambiguous title or a closed explicit target is not a
// license to create another issue.
func Place(ctx context.Context, gh github.GH, r Report) (string, error) {
	ctx = github.ActingOn(ctx, r.Repo)
	raw, err := gh(ctx, apiCommand, "repos/"+r.Repo+"/issues?state=open&per_page=100", "--paginate", "--slurp")
	if err != nil {
		return "", err
	}
	var pages [][]issue
	if err := json.Unmarshal(raw, &pages); err != nil {
		return "", fmt.Errorf("read open findings: %w", err)
	}
	if len(pages) == 0 {
		return "", errors.New("open findings inventory contains no pages")
	}
	var matches []issue
	for _, page := range pages {
		if page == nil {
			return "", errors.New("open findings inventory contains an unread page")
		}
		for _, i := range page {
			if len(i.Pull) > 0 && string(i.Pull) != "null" {
				continue
			}
			if (r.Number > 0 && i.Number == r.Number) || (r.Number == 0 && normalized(i.Title) == normalized(r.Title)) {
				matches = append(matches, i)
			}
		}
	}
	if len(matches) > 1 || r.Number > 0 && len(matches) == 0 {
		return "", fmt.Errorf("%w: no unique open issue in %s", ErrUnplaced, r.Repo)
	}
	if len(matches) == 1 {
		i := matches[0]
		if strings.TrimSpace(i.Body) != strings.TrimSpace(r.Body) {
			// The reporter supplies the complete current description. Replace it in
			// place, preserving no timestamps, repetition counts or comment history.
			if _, err := gh(ctx, apiCommand, fmt.Sprintf("repos/%s/issues/%d", r.Repo, i.Number), "--method", "PATCH", "-f", "body="+r.Body); err != nil {
				return "", err
			}
		}
		return fmt.Sprintf("https://github.com/%s/issues/%d", r.Repo, i.Number), nil
	}
	raw, err = gh(ctx, apiCommand, "repos/"+r.Repo+"/issues", "--method", "POST", "-f", "title="+r.Title, "-f", "body="+r.Body)
	if err != nil {
		return "", err
	}
	var created issue
	if err := json.Unmarshal(raw, &created); err != nil {
		return "", err
	}
	if created.Number <= 0 {
		return "", fmt.Errorf("create finding returned no issue number")
	}
	return fmt.Sprintf("https://github.com/%s/issues/%d", r.Repo, created.Number), nil
}
