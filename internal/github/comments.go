package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Comment is one comment on an issue's or a pull request's conversation.
type Comment struct {
	Login string    `json:"login"`
	At    time.Time `json:"at"`
	URL   string    `json:"url"`
	// Member says its author is on the repository's side: a member or an
	// owner of its organisation, or a collaborator, as a user-owned
	// repository's teammates are (GitHub's authorAssociation MEMBER, OWNER
	// or COLLABORATOR). Association is GitHub's word for it.
	Member      bool   `json:"member,omitempty"`
	Association string `json:"association,omitempty"`
	// Automation says its author is a bot, a GitHub App or one of the
	// pinned automation accounts.
	Automation bool `json:"automation,omitempty"`
	// Body is the comment's text.
	Body string `json:"body,omitempty"`
}

// latestCommentsQuery reads the newest comments of an issue or a pull
// request: GraphQL's last, the tail of the conversation, never a page of
// the oldest.
const latestCommentsQuery = `query($owner:String!,$name:String!,$number:Int!,$last:Int!){
  repository(owner:$owner,name:$name){
    issueOrPullRequest(number:$number){
      ... on Issue{comments(last:$last){nodes{...c}}}
      ... on PullRequest{comments(last:$last){nodes{...c}}}
    }
  }
}
fragment c on IssueComment{author{login __typename ... on User{databaseId}} authorAssociation createdAt url body}`

// LatestComments reads the last n comments of ref (owner/repo#n), oldest
// first.
func LatestComments(ctx context.Context, gh GH, ref string, n int) ([]Comment, error) {
	repo, num, ok := strings.Cut(ref, "#")
	owner, name, ok2 := strings.Cut(repo, "/")
	number, err := strconv.Atoi(num)
	if !ok || !ok2 || err != nil {
		return nil, fmt.Errorf("%q is no owner/repo#n", ref)
	}
	raw, err := gh(ActingOn(ctx, repo), "api", "graphql", "-f", "query="+latestCommentsQuery,
		"-F", "owner="+owner, "-F", "name="+name, "-F", "number="+strconv.Itoa(number), "-F", "last="+strconv.Itoa(n))
	if err != nil {
		return nil, err
	}
	var doc struct {
		Data struct {
			Repository struct {
				Item *struct {
					Comments struct {
						Nodes []struct {
							Author *struct {
								Login      string `json:"login"`
								Type       string `json:"__typename"`
								DatabaseID int64  `json:"databaseId"`
							} `json:"author"`
							Association string    `json:"authorAssociation"`
							CreatedAt   time.Time `json:"createdAt"`
							URL         string    `json:"url"`
							Body        string    `json:"body"`
						} `json:"nodes"`
					} `json:"comments"`
				} `json:"issueOrPullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("comments of %s: %w", ref, err)
	}
	if doc.Data.Repository.Item == nil {
		return nil, fmt.Errorf("comments of %s: GitHub answers no issue or pull request", ref)
	}
	var out []Comment
	for _, c := range doc.Data.Repository.Item.Comments.Nodes {
		cm := Comment{At: c.CreatedAt, URL: c.URL, Association: c.Association, Body: c.Body,
			Member: c.Association == "MEMBER" || c.Association == "OWNER" || c.Association == "COLLABORATOR"}
		if c.Author == nil {
			// A deleted account: GitHub's ghost, no person to wait on.
			cm.Login, cm.Automation = "ghost", true
		} else {
			u := ghUser{Login: c.Author.Login, ID: c.Author.DatabaseID, Type: c.Author.Type}
			cm.Login, cm.Automation = u.Login, u.automation()
		}
		out = append(out, cm)
	}
	return out, nil
}
