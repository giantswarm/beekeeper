package claim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/giantswarm/beekeeper/internal/github"
)

// ErrGone is the answer to an edit of a comment GitHub no longer has.
var ErrGone = errors.New("the claim's comment is gone")

// Client reads and writes the marker comments of issues through GitHub's
// REST API, acting on each issue's repository (github.ActingOn). A read
// sends the ETag of the last answer for the issue (Cache, a JSON file;
// none when empty): an unchanged thread costs no rate limit.
type Client struct {
	GH    github.GH
	Cache string
}

// cached is one issue's last answer: its ETag and its marker comments.
type cached struct {
	ETag     string    `json:"etag"`
	Comments []Comment `json:"comments"`
}

// cacheMu keeps one process's writes of a cache file apart; a write
// replaces the file whole, so another process reads the old or the new one.
var cacheMu sync.Mutex

// ref splits owner/repo#n.
func ref(s string) (repo string, n int, err error) {
	repo, num, ok := strings.Cut(s, "#")
	if ok {
		n, err = strconv.Atoi(num)
	}
	if !ok || err != nil || n <= 0 || strings.Count(repo, "/") != 1 {
		return "", 0, fmt.Errorf("%q is not an issue: owner/repo#n", s)
	}
	return repo, n, nil
}

// notModified is gh api's error for GitHub's answer to an ETag that still
// matches.
var notModified = regexp.MustCompile(`\bHTTP 304\b`)

// nextPage finds a Link header's next page.
var nextPage = regexp.MustCompile(`<[^>]*[?&]page=(\d+)[^>]*>;\s*rel="next"`)

// Read is the marker comments on issue ref (owner/repo#n), oldest first.
func (c *Client) Read(ctx context.Context, issue string) ([]Comment, error) {
	repo, n, err := ref(issue)
	if err != nil {
		return nil, err
	}
	ctx = github.ActingOn(ctx, repo)
	key := strings.ToLower(issue)
	last, hasLast := c.load()[key]
	var all []Comment
	for page := 1; page > 0; {
		args := []string{"api", "-i", fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100&page=%d", repo, n, page)}
		if page == 1 && hasLast && last.ETag != "" {
			args = append(args, "-H", "If-None-Match: "+last.ETag)
		}
		out, err := c.GH(ctx, args...)
		if err != nil {
			if page == 1 && hasLast && notModified.MatchString(err.Error()) {
				return last.Comments, nil
			}
			return nil, fmt.Errorf("reading the comments of %s: %w", issue, err)
		}
		head, body := splitHTTP(out)
		var raw []struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, fmt.Errorf("reading the comments of %s: %w", issue, err)
		}
		for _, r := range raw {
			if m, ok := Parse(r.Body); ok {
				all = append(all, Comment{ID: r.ID, Marker: m})
			}
		}
		if page == 1 {
			last = cached{ETag: header(head, "Etag")}
		}
		page = 0
		if m := nextPage.FindStringSubmatch(header(head, "Link")); m != nil {
			page, _ = strconv.Atoi(m[1])
			last.ETag = "" // a thread of several pages is read whole every time
		}
	}
	last.Comments = all
	c.store(key, last)
	return all, nil
}

// Post adds m as a new comment on issue and returns its id.
func (c *Client) Post(ctx context.Context, issue string, m Marker) (int64, error) {
	repo, n, err := ref(issue)
	if err != nil {
		return 0, err
	}
	out, err := c.GH(github.ActingOn(ctx, repo), "api", "-X", "POST", fmt.Sprintf("repos/%s/issues/%d/comments", repo, n), "-f", "body="+m.Body())
	if err != nil {
		return 0, fmt.Errorf("claiming %s: %w", issue, err)
	}
	var r struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(out, &r); err != nil || r.ID == 0 {
		return 0, fmt.Errorf("claiming %s: no comment id in GitHub's answer", issue)
	}
	return r.ID, nil
}

// Edit replaces comment id on issue with m; ErrGone when GitHub has no
// such comment.
func (c *Client) Edit(ctx context.Context, issue string, id int64, m Marker) error {
	repo, _, err := ref(issue)
	if err != nil {
		return err
	}
	_, err = c.GH(github.ActingOn(ctx, repo), "api", "-X", "PATCH", fmt.Sprintf("repos/%s/issues/comments/%d", repo, id), "-f", "body="+m.Body())
	switch {
	case err != nil && strings.Contains(err.Error(), "HTTP 404"):
		return fmt.Errorf("%s, comment %d: %w", issue, id, ErrGone)
	case err != nil:
		return fmt.Errorf("editing the claim on %s: %w", issue, err)
	}
	return nil
}

// Delete removes comment id on issue; ErrGone when GitHub has no such
// comment.
func (c *Client) Delete(ctx context.Context, issue string, id int64) error {
	repo, _, err := ref(issue)
	if err != nil {
		return err
	}
	_, err = c.GH(github.ActingOn(ctx, repo), "api", "-X", "DELETE", fmt.Sprintf("repos/%s/issues/comments/%d", repo, id))
	switch {
	case err != nil && strings.Contains(err.Error(), "HTTP 404"):
		return fmt.Errorf("%s, comment %d: %w", issue, id, ErrGone)
	case err != nil:
		return fmt.Errorf("deleting a released claim on %s: %w", issue, err)
	}
	return nil
}

// Pull says whether issue is a pull request, which carries no claim.
func (c *Client) Pull(ctx context.Context, issue string) (bool, error) {
	repo, n, err := ref(issue)
	if err != nil {
		return false, err
	}
	out, err := c.GH(github.ActingOn(ctx, repo), "api", fmt.Sprintf("repos/%s/issues/%d", repo, n))
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", issue, err)
	}
	var r struct {
		PullRequest json.RawMessage `json:"pull_request"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return false, fmt.Errorf("reading %s: %w", issue, err)
	}
	return len(r.PullRequest) > 0 && string(r.PullRequest) != "null", nil
}

// splitHTTP splits gh api --include's output into its head and body.
func splitHTTP(out []byte) (head string, body []byte) {
	for _, sep := range []string{"\r\n\r\n", "\n\n"} {
		if i := bytes.Index(out, []byte(sep)); i >= 0 {
			return string(out[:i]), out[i+len(sep):]
		}
	}
	return "", out
}

// header is the value of header name in head, "" when it has none.
func header(head, name string) string {
	for l := range strings.SplitSeq(head, "\n") {
		k, v, ok := strings.Cut(strings.TrimRight(l, "\r"), ":")
		if ok && strings.EqualFold(strings.TrimSpace(k), name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func (c *Client) load() map[string]cached {
	out := map[string]cached{}
	if c.Cache == "" {
		return out
	}
	if raw, err := os.ReadFile(c.Cache); err == nil {
		_ = json.Unmarshal(raw, &out) // a broken cache is an empty one
	}
	return out
}

func (c *Client) store(key string, v cached) {
	if c.Cache == "" {
		return
	}
	cacheMu.Lock()
	defer cacheMu.Unlock()
	all := c.load()
	all[key] = v
	raw, err := json.Marshal(all)
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.Cache), ".claims-*")
	if err != nil {
		return
	}
	if _, err := tmp.Write(raw); err != nil || tmp.Close() != nil {
		_ = os.Remove(tmp.Name())
		return
	}
	if os.Rename(tmp.Name(), c.Cache) != nil {
		_ = os.Remove(tmp.Name())
	}
}
