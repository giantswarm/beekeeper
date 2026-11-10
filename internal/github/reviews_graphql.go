package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const reviewActorFields = `login type:__typename ... on User { id:databaseId } ... on Bot { id:databaseId }`
const reviewCommentFields = `user:author { ` + reviewActorFields + ` } created_at:createdAt html_url:url originalCommit { oid }`

// ViewerLogin reads the identity of this GH runner, without depending on the
// REST quota. Never cache across runners: they may act as different accounts.
func ViewerLogin(ctx context.Context, gh GH) (string, error) {
	var doc struct {
		Viewer struct{ Login string }
	}
	err := reviewQuery(ctx, gh, `query { viewer { login } }`, &doc)
	if err == nil && doc.Viewer.Login != "" {
		return doc.Viewer.Login, nil
	}
	if err == nil {
		err = errors.New("GraphQL viewer has no login")
	}
	raw, restErr := gh(ctx, "api", "user", "--jq", ".login")
	login := strings.TrimSpace(string(raw))
	if restErr == nil && login != "" {
		return login, nil
	}
	if restErr == nil {
		restErr = errors.New("REST user has no login")
	}
	return "", errors.Join(err, restErr)
}

// reviewQuery rejects partial GraphQL results: missing feedback cannot grant
// permission to merge. All calls retain the same repository and GH identity.
func reviewQuery(ctx context.Context, gh GH, query string, dst any) error {
	raw, err := gh(ctx, "api", "graphql", "-f", "query="+query)
	if err != nil {
		return err
	}
	var doc struct {
		Data   json.RawMessage
		Errors []struct{ Message string }
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	if len(doc.Errors) > 0 {
		return fmt.Errorf("GraphQL review read: %s", doc.Errors[0].Message)
	}
	if len(doc.Data) == 0 || string(doc.Data) == "null" {
		return errors.New("GraphQL review read has no data")
	}
	return json.Unmarshal(doc.Data, dst)
}

type reviewConnection[T any] struct {
	Nodes    []*T
	PageInfo *struct {
		HasNextPage bool
		EndCursor   string
	}
}

// reviewPages follows every connection, including comments on a single review.
// Null connections, null nodes and non-advancing cursors fail closed.
func reviewPages[T any](ctx context.Context, gh GH, id, kind, field, fields string) ([]T, error) {
	var all []T
	cursor := ""
	seen := map[string]bool{}
	for {
		after := ""
		if cursor != "" {
			after = ",after:" + quote(cursor)
		}
		q := fmt.Sprintf(`query { node(id:%s) { ... on %s { items:%s(first:100%s) { nodes { %s } pageInfo { hasNextPage endCursor } } } } }`, quote(id), kind, field, after, fields)
		var doc struct {
			Node *struct{ Items *reviewConnection[T] }
		}
		if err := reviewQuery(ctx, gh, q, &doc); err != nil {
			return nil, err
		}
		if doc.Node == nil || doc.Node.Items == nil || doc.Node.Items.PageInfo == nil || doc.Node.Items.Nodes == nil {
			return nil, fmt.Errorf("GraphQL %s.%s is incomplete", kind, field)
		}
		page := doc.Node.Items
		for _, node := range page.Nodes {
			if node == nil {
				return nil, fmt.Errorf("GraphQL %s.%s has a null node", kind, field)
			}
			all = append(all, *node)
		}
		if !page.PageInfo.HasNextPage {
			return all, nil
		}
		cursor = page.PageInfo.EndCursor
		if cursor == "" || seen[cursor] {
			return nil, fmt.Errorf("GraphQL %s.%s pagination did not advance", kind, field)
		}
		seen[cursor] = true
	}
}

type graphReview struct {
	ghReview
	ID       string
	Commit   struct{ OID string }
	Comments *struct{ TotalCount int }
}

type graphReviewComment struct {
	ghComment
	OriginalCommit *struct{ OID string }
}

// graphReviewFeedback reads the same facts as the REST guard: published
// reviews, all diff/conversation comments and the head commit's committer date.
func graphReviewFeedback(ctx context.Context, gh GH, repo string, n int) (feedback, ghUser, string, error) {
	var f feedback
	var doc struct {
		Viewer     struct{ Login string }
		Repository *struct {
			PullRequest *struct {
				ID         string
				HeadRefOID string
				Author     ghUser
				Commits    struct {
					Nodes []struct {
						Commit struct {
							OID           string
							CommittedDate time.Time
						}
					}
				}
			}
		}
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return f, ghUser{}, "", fmt.Errorf("invalid repository %q", repo)
	}
	q := fmt.Sprintf(`query { viewer { login } repository(owner:%s,name:%s) { pullRequest(number:%d) { id headRefOid author { %s } commits(last:1) { nodes { commit { oid committedDate } } } } } }`, quote(owner), quote(name), n, reviewActorFields)
	if err := reviewQuery(ctx, gh, q, &doc); err != nil {
		return f, ghUser{}, "", err
	}
	if doc.Repository == nil || doc.Repository.PullRequest == nil || doc.Viewer.Login == "" {
		return f, ghUser{}, "", errors.New("GraphQL review read has no pull request or viewer")
	}
	p := doc.Repository.PullRequest
	if p.ID == "" || p.HeadRefOID == "" || len(p.Commits.Nodes) != 1 || p.Commits.Nodes[0].Commit.OID != p.HeadRefOID || p.Commits.Nodes[0].Commit.CommittedDate.IsZero() {
		return f, ghUser{}, "", errors.New("GraphQL review read has no matching head commit")
	}
	f.headSHA, f.headAt = p.HeadRefOID, p.Commits.Nodes[0].Commit.CommittedDate
	reviews, err := reviewPages[graphReview](ctx, gh, p.ID, "PullRequest", "reviews", `id user:author { `+reviewActorFields+` } state body submitted_at:submittedAt commit { oid } html_url:url author_association:authorAssociation comments { totalCount }`)
	if err != nil {
		return f, ghUser{}, "", err
	}
	for _, r := range reviews {
		if r.State == "PENDING" {
			continue // unpublished drafts are not returned by the REST review lists
		}
		if r.State == "" || r.SubmittedAt.IsZero() || r.Comments == nil {
			return f, ghUser{}, "", errors.New("GraphQL review is incomplete")
		}
		r.CommitID = r.Commit.OID
		f.reviews = append(f.reviews, r.ghReview)
		if r.Comments.TotalCount == 0 {
			continue
		}
		if r.ID == "" {
			return f, ghUser{}, "", errors.New("GraphQL review has no id for its comments")
		}
		comments, err := reviewPages[graphReviewComment](ctx, gh, r.ID, "PullRequestReview", "comments", reviewCommentFields)
		if err != nil {
			return f, ghUser{}, "", err
		}
		for _, c := range comments {
			if c.CreatedAt.IsZero() {
				return f, ghUser{}, "", errors.New("GraphQL review comment has no date")
			}
			if c.OriginalCommit != nil {
				c.OriginalCommitID = c.OriginalCommit.OID
			}
			f.reviewComments = append(f.reviewComments, c.ghComment)
		}
	}
	f.comments, err = reviewPages[ghComment](ctx, gh, p.ID, "PullRequest", "comments", `user:author { `+reviewActorFields+` } created_at:createdAt html_url:url`)
	if err != nil {
		return f, ghUser{}, "", err
	}
	for _, c := range f.comments {
		if c.CreatedAt.IsZero() {
			return f, ghUser{}, "", errors.New("GraphQL conversation comment has no date")
		}
	}
	return f, p.Author, doc.Viewer.Login, nil
}
