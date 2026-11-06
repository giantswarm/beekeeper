// Package issuemonitor keeps issue metadata separate from agent work. It never
// requests issue bodies or comments. Human triage is required for outsiders.
package issuemonitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
)

type Class string

const (
	Employee    Class = "employee"
	External    Class = "external"
	Bot         Class = "bot"
	TriageLabel       = "beekeeper-triage"
)

// Classify deliberately has no cache. Even an unavailable API fails closed.
func Classify(ctx context.Context, gh github.GH, org, author string) Class {
	var membership struct {
		State string `json:"state"`
	}
	out, err := gh(ctx, "api", fmt.Sprintf("orgs/%s/teams/employees/memberships/%s", org, author))
	if strings.HasSuffix(strings.ToLower(author), "[bot]") || slices.Contains([]string{"renovate", "dependabot", "align-files"}, strings.ToLower(author)) {
		return Bot
	}
	if author != "" && err == nil && json.Unmarshal(out, &membership) == nil && membership.State == "active" {
		return Employee
	}
	return External
}

// Issue has only metadata. Title is used solely for delivery to human Slack.
type Issue struct {
	ReopenedAt time.Time `json:"-"`
	Number     int       `json:"number"`
	Title      string    `json:"title"`
	URL        string    `json:"url"`
	State      string    `json:"state"`
	Author     struct {
		Login string
	} `json:"author"`
	Assignees []struct {
		Login string
	} `json:"assignees"`
	Labels []struct {
		Name string
	} `json:"labels"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Pointer is the entire agent-facing queue entry: no title, body or comments.
type Pointer struct {
	Ref    string `json:"ref"`
	URL    string `json:"url"`
	Source string `json:"source"`
}
type Record struct {
	ReopenedAt time.Time `json:"reopenedAt,omitempty"`
	State      string    `json:"state"`
	Class      Class     `json:"class"`
	Author     string    `json:"author"`
	Notified   bool      `json:"notified,omitempty"`
	Cleanup    string    `json:"cleanup,omitempty"`
}
type State struct {
	Repositories map[string]string   `json:"repositories"`
	Issues       map[string]Record   `json:"issues"`
	Queue        []Pointer           `json:"queue"`
	Contributors map[string][]string `json:"contributors"`
	Backlog      map[string]bool     `json:"backlog"`
}

func (s *State) Init() {
	if s.Issues == nil {
		s.Issues = map[string]Record{}
	}
	if s.Contributors == nil {
		s.Contributors = map[string][]string{}
	}
	if s.Backlog == nil {
		s.Backlog = map[string]bool{}
	}
}

var ErrExternal = errors.New("issue author is not an active employee")

// Enqueue is a trust boundary, not a caller-supplied author assertion. It reads
// the current issue author itself and checks live membership before admission.
func (s *State) Enqueue(ctx context.Context, gh github.GH, repo string, n int) error {
	i, err := Read(ctx, gh, repo, n)
	if err != nil {
		return err
	}
	org, _, _ := strings.Cut(repo, "/")
	if Classify(ctx, gh, org, i.Author.Login) != Employee {
		return ErrExternal
	}
	if i.State != "OPEN" {
		return errors.New("issue is not open")
	}
	ref := fmt.Sprintf("%s#%d", repo, n)
	if !slices.ContainsFunc(s.Queue, func(p Pointer) bool { return p.Ref == ref }) {
		s.Queue = append(s.Queue, Pointer{ref, fmt.Sprintf("https://github.com/%s/issues/%d", repo, n), "issue"})
	}
	return nil
}

func Read(ctx context.Context, gh github.GH, repo string, n int) (Issue, error) {
	org, name, _ := strings.Cut(repo, "/")
	query := `query IssueMonitorIssue($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){issueOrPullRequest(number:$number){__typename ... on Issue{number state author{login}}}}}`
	out, err := gh(ctx, "api", "graphql", "-f", "query="+query, "-f", "owner="+org, "-f", "name="+name, "-F", fmt.Sprintf("number=%d", n))
	if err != nil {
		return Issue{}, errors.New("read issue metadata failed")
	}
	var response struct {
		Data struct {
			Repository struct {
				IssueOrPullRequest struct {
					Type   string `json:"__typename"`
					Number int
					State  string
					Author struct{ Login string }
				}
			}
		}
	}
	if err := json.Unmarshal(out, &response); err != nil {
		return Issue{}, errors.New("invalid issue metadata")
	}
	node := response.Data.Repository.IssueOrPullRequest
	if node.Type != "Issue" {
		return Issue{}, errors.New("reference is not an issue")
	}
	return Issue{Number: node.Number, State: node.State, Author: node.Author}, nil
}

// Notify must deliver to the owning team's human channel; it must not expose
// the message in logs, agent prompts or command output.
type Notify func(context.Context, string, string) error

type Monitor struct {
	GH     github.GH
	Notify Notify
	Save   func(*State) error
	Now    time.Time
}

// Poll reconciles every open issue in repositories from the ownership record.
// Each successful external notification is checkpointed before continuing.
func (m Monitor) Poll(ctx context.Context, s *State, ownership map[string]string) error {
	s.Init()
	s.Repositories = ownership
	s.Queue = slices.DeleteFunc(s.Queue, func(p Pointer) bool { repo, _, _ := strings.Cut(p.Ref, "#"); _, ok := ownership[repo]; return !ok })
	if err := m.Save(s); err != nil {
		return err
	}
	repos := make([]string, 0, len(ownership))
	for repo := range ownership {
		repos = append(repos, repo)
	}
	slices.Sort(repos)
	for _, repo := range repos {
		team := ownership[repo]
		// --limit is intentionally not used: GraphQL pagination reads the full set.
		org, name, _ := strings.Cut(repo, "/")
		cursor := ""
		for {
			query := `query($owner:String!,$name:String!,$cursor:String){repository(owner:$owner,name:$name){issues(first:100,after:$cursor,states:OPEN){nodes{number title url state author{login} assignees(first:1){nodes{login}} labels(first:100){nodes{name}} updatedAt timelineItems(last:1,itemTypes:[REOPENED_EVENT]){nodes{... on ReopenedEvent{createdAt}}}} pageInfo{hasNextPage endCursor}}}}`
			args := []string{"api", "graphql", "-f", "query=" + query, "-f", "owner=" + org, "-f", "name=" + name}
			if cursor != "" {
				args = append(args, "-f", "cursor="+cursor)
			}
			out, err := m.GH(ctx, args...)
			if err != nil {
				return fmt.Errorf("list issue metadata for %s failed", repo)
			}
			var page struct {
				Data struct {
					Repository *struct {
						Issues struct {
							Nodes []struct {
								Number            int
								Title, URL, State string
								Author            struct{ Login string }
								Assignees         struct{ Nodes []struct{ Login string } }
								Labels            struct{ Nodes []struct{ Name string } }
								UpdatedAt         time.Time
								TimelineItems     struct {
									Nodes []struct{ CreatedAt time.Time }
								}
							}
							PageInfo struct {
								HasNextPage bool
								EndCursor   string
							}
						}
					}
				}
			}
			if err := json.Unmarshal(out, &page); err != nil {
				return errors.New("invalid issue metadata response")
			}
			if page.Data.Repository == nil {
				return errors.New("missing repository metadata")
			}
			for _, node := range page.Data.Repository.Issues.Nodes {
				i := Issue{Number: node.Number, Title: node.Title, URL: node.URL, State: node.State, Author: node.Author, Assignees: node.Assignees.Nodes, Labels: node.Labels.Nodes, UpdatedAt: node.UpdatedAt}
				if len(node.TimelineItems.Nodes) > 0 {
					i.ReopenedAt = node.TimelineItems.Nodes[0].CreatedAt
				}
				if err := m.reconcile(ctx, s, repo, team, i, !s.Backlog[repo]); err != nil {
					return err
				}
			}
			info := page.Data.Repository.Issues.PageInfo
			if !info.HasNextPage {
				break
			}
			if info.EndCursor == "" || info.EndCursor == cursor {
				return errors.New("issue pagination did not advance")
			}
			cursor = info.EndCursor
		}
		s.Backlog[repo] = true
		if err := m.Save(s); err != nil {
			return err
		}
	}
	return nil
}

func (m Monitor) reconcile(ctx context.Context, s *State, repo, team string, i Issue, backlog bool) error {
	ref := fmt.Sprintf("%s#%d", repo, i.Number)
	org, _, _ := strings.Cut(repo, "/")
	class := Classify(ctx, m.GH, org, i.Author.Login)
	old := s.Issues[ref]
	r := Record{Class: class, Author: i.Author.Login, Notified: old.Notified, Cleanup: old.Cleanup, ReopenedAt: i.ReopenedAt}
	// Reclassification revokes any previously admitted work before side effects.
	s.Queue = slices.DeleteFunc(s.Queue, func(p Pointer) bool { return p.Ref == ref })
	if class == External {
		r.State = "triage-flagged"
		if !slices.Contains(s.Contributors[repo], i.Author.Login) {
			s.Contributors[repo] = append(s.Contributors[repo], i.Author.Login)
			slices.Sort(s.Contributors[repo])
		}
		if _, err := m.GH(ctx, "label", "create", TriageLabel, "--repo", repo, "--color", "D93F0B", "--description", "Human triage required", "--force"); err != nil {
			return errors.New("ensure triage label failed")
		}
		if _, err := m.GH(ctx, "issue", "edit", fmt.Sprint(i.Number), "--repo", repo, "--add-label", TriageLabel); err != nil {
			return errors.New("label external issue failed")
		}
		if !r.Notified {
			// Slack mrkdwn is disabled by the delivery adapter. No issue text is returned.
			message := fmt.Sprintf("%s\n%s\nAuthor: %s\nhttps://github.com/%s/issues/%d", repo, i.Title, i.Author.Login, repo, i.Number)
			if err := m.Notify(ctx, team, message); err != nil {
				return errors.New("notify issue triage failed")
			}
			r.Notified = true
		}
	} else if len(i.Assignees) > 0 || (old.State == "owned" && old.ReopenedAt.Equal(i.ReopenedAt)) {
		r.State = "owned"
	} else if class == Bot {
		r.State = "reported-for-cleanup"
	} else {
		if err := s.Enqueue(ctx, m.GH, repo, i.Number); err != nil {
			// Membership may have changed between the scan and admission. Retry the
			// whole classification next pass rather than persisting a false state.
			return err
		}
		r.State = "queued"
	}
	if (backlog || class == Bot) && r.Cleanup == "" {
		r.Cleanup = "still-valid (human confirmation required)"
		if m.Now.Sub(i.UpdatedAt) > 180*24*time.Hour {
			r.Cleanup = "stale"
		}
		for _, l := range i.Labels {
			if strings.EqualFold(l.Name, "duplicate") {
				r.Cleanup = "duplicate"
			}
			if strings.EqualFold(l.Name, "overtaken") {
				r.Cleanup = "overtaken (confirm merged PR)"
			}
		}
	}
	s.Issues[ref] = r
	return m.Save(s)
}
