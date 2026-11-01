// Package claimtest is a fake of the GitHub issue comments the claims live
// in, for the tests of the claim package and its callers.
package claimtest

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// comments is the comments segment of a path.
const comments = "comments"

// GitHub keeps the comments of issues in memory and answers the gh api
// calls claim.Client makes: a listing with an ETag (an If-None-Match that
// still matches is gh's "HTTP 304"), a post and an edit (an unknown comment
// is gh's "HTTP 404").
type GitHub struct {
	mu       sync.Mutex
	next     int64
	comments map[string][]Comment // by owner/repo#n
	// Calls counts the calls by kind: list, notModified, post, edit.
	Calls map[string]int
}

// Comment is one comment of an issue.
type Comment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
}

// New is a GitHub without comments.
func New() *GitHub {
	return &GitHub{next: 1000, comments: map[string][]Comment{}, Calls: map[string]int{}}
}

// Add puts a comment on issue (owner/repo#n) and returns its id.
func (g *GitHub) Add(issue, body string) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.next++
	g.comments[issue] = append(g.comments[issue], Comment{ID: g.next, Body: body})
	return g.next
}

// Comments are issue's comments, oldest first.
func (g *GitHub) Comments(issue string) []Comment {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Comment(nil), g.comments[issue]...)
}

// Delete removes comment id wherever it is.
func (g *GitHub) Delete(id int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, cs := range g.comments {
		for i, c := range cs {
			if c.ID == id {
				g.comments[k] = append(cs[:i:i], cs[i+1:]...)
			}
		}
	}
}

// GH is the fake gh CLI.
func (g *GitHub) GH(_ context.Context, args ...string) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(args) < 2 || args[0] != "api" {
		return nil, fmt.Errorf("gh %v: not a call claims make", args)
	}
	var method, path, body, etag string
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "-X":
			i++
			method = args[i]
		case "-f":
			i++
			body = strings.TrimPrefix(args[i], "body=")
		case "-H":
			i++
			etag = strings.TrimPrefix(args[i], "If-None-Match: ")
		case "-i":
		default:
			path = args[i]
		}
	}
	path, _, _ = strings.Cut(path, "?")
	parts := strings.Split(path, "/") // repos o r issues n comments | repos o r issues comments id
	if len(parts) != 6 || parts[0] != "repos" || parts[3] != "issues" {
		return nil, fmt.Errorf("gh api %s: not a call claims make", path)
	}
	repo := parts[1] + "/" + parts[2]
	switch {
	case method == "" && parts[5] == comments:
		issue := repo + "#" + parts[4]
		raw, _ := json.Marshal(append([]Comment{}, g.comments[issue]...))
		tag := fmt.Sprintf(`W/"%x"`, sha256.Sum256(raw))
		if etag == tag {
			g.Calls["notModified"]++
			return []byte("HTTP/2.0 304 Not Modified\r\n\r\n"), fmt.Errorf("gh api %s: exit status 1: gh: HTTP 304", path)
		}
		g.Calls["list"]++
		return []byte("HTTP/2.0 200 OK\r\nEtag: " + tag + "\r\n\r\n" + string(raw)), nil
	case method == "POST" && parts[5] == comments:
		g.Calls["post"]++
		g.next++
		issue := repo + "#" + parts[4]
		g.comments[issue] = append(g.comments[issue], Comment{ID: g.next, Body: body})
		return []byte(`{"id":` + strconv.FormatInt(g.next, 10) + `}`), nil
	case method == "PATCH" && parts[4] == comments:
		g.Calls["edit"]++
		id, _ := strconv.ParseInt(parts[5], 10, 64)
		for k, cs := range g.comments {
			if !strings.HasPrefix(k, repo+"#") {
				continue
			}
			for i := range cs {
				if cs[i].ID == id {
					cs[i].Body = body
					return []byte(`{"id":` + parts[5] + `}`), nil
				}
			}
		}
		return nil, fmt.Errorf("gh api %s: exit status 1: gh: Not Found (HTTP 404)", path)
	}
	return nil, fmt.Errorf("gh api %s %s: not a call claims make", method, path)
}
