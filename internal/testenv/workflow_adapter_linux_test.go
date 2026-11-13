//go:build linux

//nolint:goconst // Literal CLI and API fixtures keep the external protocol visible.
package testenv_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

type apiState struct {
	Status, Base, Head, MergeCommit, Unknown string
	Parked, Merged                           bool
	Sessions                                 []session
	Reviews                                  []map[string]any
	Calls                                    [][]string
}
type session struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
}

func loadAPI() (apiState, error) {
	var a apiState
	b, e := os.ReadFile("/work/api.json") //nolint:gosec // Test-owned scratch files or this test executable.
	if e == nil {
		e = json.Unmarshal(b, &a)
	}
	return a, e
}
func saveAPI(a apiState) error {
	b, e := json.Marshal(a)
	if e != nil {
		return e
	}
	if e = os.WriteFile("/work/api.next", b, 0600); e != nil { //nolint:gosec // Test-owned scratch files or this test executable.
		return e
	}
	return os.Rename("/work/api.next", "/work/api.json")
}
func locked(f func(*apiState) error) error {
	fd, e := os.OpenFile("/work/api.lock", os.O_CREATE|os.O_RDWR, 0600) //nolint:gosec // Test-owned scratch files or this test executable.
	if e != nil {
		return e
	}
	defer func() { _ = fd.Close() }()
	if e = syscall.Flock(int(fd.Fd()), syscall.LOCK_EX); e != nil {
		return e
	}
	defer func() { _ = syscall.Flock(int(fd.Fd()), syscall.LOCK_UN) }()
	a, e := loadAPI()
	if e != nil {
		return e
	}
	err := f(&a)
	if e = saveAPI(a); e != nil {
		return e
	}
	return err
}
func emit(v any) error { return json.NewEncoder(os.Stdout).Encode(v) }
func claudeFixture(args []string) error {
	if slices.Equal(args, []string{"plugin", "list", "--json"}) {
		return emit([]any{})
	}
	if slices.Equal(args, []string{"-p", "/usage", "--output-format", "json"}) {
		return emit(map[string]any{"result": "Current session: 0% used · resets " + time.Now().UTC().Add(time.Hour).Format("Jan 2, 3:04pm") + " (UTC)\nCurrent week (all models): 0% used · resets " + time.Now().UTC().Add(24*time.Hour).Format("Jan 2, 3:04pm") + " (UTC)"})
	}
	if slices.Equal(args, []string{"agents", "--json"}) || slices.Equal(args, []string{"agents", "--json", "--all"}) {
		return locked(func(a *apiState) error { return emit(a.Sessions) })
	}
	if len(args) == 2 && args[0] == "stop" && (strings.HasPrefix("aaaaaaaa-1111-4111-8111-111111111111", args[1]) || strings.HasPrefix("bbbbbbbb-2222-4222-8222-222222222222", args[1])) {
		return nil
	}
	if len(args) > 0 && args[0] == "--bg" && slices.Contains(args, "-n") {
		name := args[slices.Index(args, "-n")+1]
		id := "aaaaaaaa-1111-4111-8111-111111111111"
		if strings.HasPrefix(name, "Review ") {
			id = "bbbbbbbb-2222-4222-8222-222222222222"
		}
		if err := locked(func(a *apiState) error {
			a.Sessions = append(a.Sessions, session{id, id, name, "background"})
			return nil
		}); err != nil {
			return err
		}
		log, e := os.OpenFile("/work/turn-"+id+".log", os.O_CREATE|os.O_WRONLY, 0600) //nolint:gosec // Test-owned scratch files or this test executable.
		if e != nil {
			return e
		}
		defer func() { _ = log.Close() }()
		c := exec.Command("/work/bin/fixture", "--turn", name, id) //nolint:gosec // Fixed fixture executable/arguments or the candidate in this run's private workspace.
		c.Stdout = log
		c.Stderr = log
		if e = c.Start(); e != nil {
			return e
		}
		fmt.Println("backgrounded · " + id[:8] + " · " + name)
		return nil
	}
	return locked(func(a *apiState) error { a.Unknown = fmt.Sprint("claude ", args); return fmt.Errorf("%s", a.Unknown) })
}
func turn(name, id string) error {
	for key, value := range map[string]string{"CLAUDE_CODE_SESSION_ID": id, "CLAUDE_CODE_SESSION_NAME": name} {
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	if err := until(func() (bool, error) {
		s, e := readState()
		if e != nil {
			return false, nil
		}
		return slices.ContainsFunc(s.Agents, func(a state.Agent) bool { return a.Name == name && a.Session == id }), nil
	}); err != nil {
		return err
	}
	if strings.HasPrefix(name, "Review ") {
		a, e := loadAPI()
		if e != nil {
			return e
		}
		// Publish an obsolete approval first. The driver must prove it does not
		// release the current head before this reviewer publishes its verdict.
		if _, e = command("gh", "api", "repos/o/r/pulls/7/reviews", "-f", "event=COMMENT", "-f", "body=Review: approved\nHead reviewed: `"+a.Base+"`", "-f", "commit_id="+a.Base); e != nil {
			return e
		}
		if e = until(func() (bool, error) { _, e := os.Stat("/work/allow-review"); return e == nil, nil }); e != nil {
			return e
		}
		// Use GitHub's review endpoint, including the exact reviewed commit, just
		// like a production reviewer. The candidate later parses this response.
		body := "Review: approved\nHead reviewed: `" + a.Head + "`"
		if _, e = command("gh", "api", "repos/o/r/pulls/7/reviews", "-f", "event=COMMENT", "-f", "body="+body, "-f", "commit_id="+a.Head); e != nil {
			return e
		}
		_, e = bk("agents", "idle", "--done", "--problem", "none", "--report", body+"\nhttps://github.com/o/r/pull/7#pullrequestreview-2")
		if e != nil {
			return e
		}
		return write("/work/reviewer-done", "done")
	}
	for _, args := range [][]string{{"checkout", "-b", "work"}} {
		if _, e := git(args...); e != nil {
			return e
		}
	}
	if e := write("change.txt", "deterministic worker\n"); e != nil {
		return e
	}
	for _, args := range [][]string{{"add", "change.txt"}, {"commit", "-m", "worker change"}, {"push", "origin", "work"}} {
		if _, e := git(args...); e != nil {
			return e
		}
	}
	if _, e := command("gh", "pr", "create", "--repo", "o/r", "--base", "main", "--head", "work", "--title", "Fixture change", "--body", "Closes #10"); e != nil {
		return e
	}

	out, e := bk("agents", "park", "--on", "o/r#7", "desk review")
	fmt.Println(out)
	if e != nil {
		return e
	}
	return locked(func(a *apiState) error { a.Parked = true; return nil })
}

func issue(a *apiState) map[string]any {
	now := time.Now().UTC().Format(time.RFC3339)
	return map[string]any{"id": "ISSUE", "number": 10, "url": "https://github.com/o/r/issues/10", "title": "Fixture task", "state": "OPEN", "createdAt": now, "updatedAt": now, "repository": map[string]any{"nameWithOwner": "o/r"}, "labels": map[string]any{"nodes": []any{}}, "assignees": map[string]any{"nodes": []any{}}, "subIssuesSummary": map[string]any{"total": 0, "completed": 0}, "blockedBy": map[string]any{"totalCount": 0, "nodes": []any{}}}
}
func item(a *apiState) map[string]any {
	return map[string]any{"id": "ITEM", "isArchived": false, "status": map[string]any{"name": a.Status, "updatedAt": time.Now().UTC().Format(time.RFC3339)}, "project": map[string]any{"id": "PROJECT", "number": 1, "owner": map[string]any{"login": "o"}}, "content": issue(a)}
}
func pull(a *apiState) map[string]any {
	s := "OPEN"
	if a.Merged {
		s = "MERGED"
	}
	reviews := []any{}
	for _, r := range a.Reviews {
		reviews = append(reviews, map[string]any{"id": r["node_id"], "url": r["html_url"], "author": r["user"], "authorAssociation": "MEMBER", "body": r["body"], "state": r["state"], "submittedAt": r["submitted_at"], "commit": map[string]any{"oid": r["commit_id"]}})
	}
	return map[string]any{"state": s, "url": "https://github.com/o/r/pull/7", "headRefOid": a.Head, "baseRefName": "main", "headRefName": "work", "statusCheckRollup": []any{map[string]any{"__typename": "CheckRun", "name": "fixture", "status": "COMPLETED", "conclusion": "SUCCESS"}}, "reviews": reviews, "author": map[string]any{"login": "desk"}, "title": "Fixture change", "files": []any{map[string]any{"path": "change.txt"}}, "changedFiles": 1, "mergeCommit": map[string]any{"oid": a.MergeCommit}, "isDraft": false, "mergeable": "MERGEABLE", "mergeStateStatus": "CLEAN", "closingIssuesReferences": []any{map[string]any{"number": 10, "url": "https://github.com/o/r/issues/10", "repository": map[string]any{"nameWithOwner": "o/r"}}}, "updatedAt": time.Now().UTC().Format(time.RFC3339)}
}
func adapter(args []string) error {
	return locked(func(a *apiState) error {
		a.Calls = append(a.Calls, args)
		joined := strings.Join(args, " ")
		fields := map[string]string{}
		for _, s := range args {
			k, v, ok := strings.Cut(s, "=")
			if ok {
				fields[k] = v
			}
		}
		data := func(v any) error { return emit(map[string]any{"data": v}) }
		switch {
		case joined == "api -i repos/o/r/issues/10/comments?per_page=100&page=1" || joined == "api -i repos/o/r/issues/7/comments?per_page=100&page=1":
			fmt.Print("HTTP/2.0 200 OK\r\nContent-Type: application/json\r\n\r\n[]")
			return nil
		case joined == "api --paginate repos/o/r/pulls/7/reviews":
			return emit(a.Reviews)
		case joined == "api --paginate repos/o/r/pulls/7/comments" || joined == "api --paginate repos/o/r/issues/7/comments":
			return emit([]any{})
		case len(args) == 4 && args[0] == "api" && args[1] == "repos/o/r/commits/"+a.Head && args[2] == "--jq" && args[3] == ".commit.committer.date":
			date, e := git("show", "-s", "--format=%cI", a.Head)
			if e != nil {
				return e
			}
			fmt.Println(date)
			return nil
		case joined == "api repos/o/r/pulls/7":
			return emit(map[string]any{"user": map[string]any{"login": "desk"}, "base": map[string]any{"ref": "main"}, "head": map[string]any{"ref": "work", "sha": a.Head, "repo": map[string]any{"full_name": "o/r"}}})
		case joined == "pr create --repo o/r --base main --head work --title Fixture change --body Closes #10":
			head, e := git("--git-dir=/work/origin.git", "rev-parse", "refs/heads/work")
			if e != nil {
				return e
			}
			a.Head = head
			fmt.Println("https://github.com/o/r/pull/7")
			return nil
		case strings.HasPrefix(joined, "search prs "):
			return emit([]any{})
		case strings.HasPrefix(joined, "pr list "):
			return emit([]any{})
		case len(args) > 2 && args[0] == "pr" && args[1] == "view" && args[2] == "7":
			return emit(pull(a))
		case len(args) > 2 && args[0] == "pr" && args[1] == "merge" && args[2] == "7":
			idx := slices.Index(args, "--match-head-commit")
			if !slices.Contains(args, "--merge") || idx < 0 || idx+1 == len(args) || args[idx+1] != a.Head || len(a.Reviews) != 2 || a.Reviews[1]["commit_id"] != a.Head {
				return fmt.Errorf("merge lacks exact-head approval")
			}
			if _, e := git("checkout", "main"); e != nil {
				return e
			}
			if _, e := git("merge", "--no-ff", "--no-edit", a.Head); e != nil {
				return e
			}
			sha, e := git("rev-parse", "HEAD")
			if e != nil {
				return e
			}
			if _, e = git("push", "origin", "main"); e != nil {
				return e
			}
			a.MergeCommit = sha
			a.Merged = true
			return nil
		case joined == "api user --jq .login":
			fmt.Println("desk")
			return nil
		case len(args) > 1 && args[0] == "api" && args[1] == "repos/o/r/pulls/7/reviews" && fields["commit_id"] != "":
			if (fields["commit_id"] != a.Head && fields["commit_id"] != a.Base) || fields["event"] != "COMMENT" {
				return fmt.Errorf("invalid review publication")
			}
			r := map[string]any{"node_id": fmt.Sprint("REVIEW", len(a.Reviews)+1), "id": len(a.Reviews) + 1, "html_url": fmt.Sprint("https://github.com/o/r/pull/7#pullrequestreview-", len(a.Reviews)+1), "pull_request_url": "https://api.github.com/repos/o/r/pulls/7", "body": fields["body"], "state": "COMMENTED", "commit_id": fields["commit_id"], "submitted_at": time.Now().UTC().Format(time.RFC3339Nano), "author_association": "MEMBER", "user": map[string]any{"login": "independent-reviewer"}}
			a.Reviews = append(a.Reviews, r)
			return emit(r)
		case joined == "api repos/o/r/pulls/7/reviews/2":
			if len(a.Reviews) == 2 {
				return emit(a.Reviews[1])
			}
		case strings.HasPrefix(joined, "api graphql "):
			q := fields["query"]
			switch {
			case strings.Contains(q, "parent{url}"):
				return data(map[string]any{"repository": map[string]any{"issue": map[string]any{"parent": nil}}})
			case strings.Contains(q, "fields(first:50)"):
				return data(map[string]any{"repositoryOwner": map[string]any{"projectV2": map[string]any{"id": "PROJECT", "fields": map[string]any{"nodes": []any{map[string]any{"id": "STATUS", "name": "Status", "options": []any{map[string]any{"id": "ready", "name": "Ready"}, map[string]any{"id": "progress", "name": "In Progress"}, map[string]any{"id": "done", "name": "Done"}}}}}}}})
			case strings.Contains(q, "items(first:80"):
				nodes := []any{}
				if !a.Merged {
					nodes = append(nodes, item(a))
				}
				return data(map[string]any{"repositoryOwner": map[string]any{"projectV2": map[string]any{"items": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false}}}}})
			case strings.Contains(q, "updateProjectV2ItemFieldValue"):
				switch fields["v"] {
				case "progress":
					a.Status = "In Progress"
				case "done":
					a.Status = "Done"
				default:
					return fmt.Errorf("unknown board status")
				}
				return data(map[string]any{"updateProjectV2ItemFieldValue": map[string]any{"projectV2Item": map[string]any{"id": "ITEM"}}})
			case strings.Contains(q, "issue(number:$n){id state projectItems"):
				return data(map[string]any{"repository": map[string]any{"issue": map[string]any{"id": "ISSUE", "projectItems": map[string]any{"nodes": []any{item(a)}}}}})
			case strings.Contains(q, "closingIssuesReferences(first:20)"):
				return data(map[string]any{"r0": map[string]any{"issueOrPullRequest": map[string]any{"closingIssuesReferences": map[string]any{"nodes": []any{map[string]any{"number": 10, "repository": map[string]any{"nameWithOwner": "o/r"}}}}}}})
			case strings.Contains(q, "issueOrPullRequest(number:10)"):
				state := "OPEN"
				if a.Merged {
					state = "CLOSED"
				}
				return data(map[string]any{"r0": map[string]any{"issueOrPullRequest": map[string]any{"__typename": "Issue", "state": state, "timelineItems": map[string]any{"nodes": []any{map[string]any{"closer": map[string]any{"__typename": "PullRequest", "number": 7, "repository": map[string]any{"nameWithOwner": "o/r"}}}}}}}})
			case strings.Contains(q, "issueOrPullRequest(number:7)"):
				state := "OPEN"
				if a.Merged {
					state = "MERGED"
				}
				return data(map[string]any{"r0": map[string]any{"issueOrPullRequest": map[string]any{"__typename": "PullRequest", "state": state}}})
			case strings.Contains(q, "mergeQueue(branch:"):
				return data(map[string]any{"repository": map[string]any{"mergeQueue": nil}})
			case strings.Contains(q, "viewer { login }"):
				date := "2026-01-01T00:00:00Z"
				if a.Head != "" {
					if out, e := git("show", "-s", "--format=%cI", a.Head); e == nil {
						date = out
					}
				}
				return data(map[string]any{
					"viewer": map[string]any{"login": "desk"},
					"repository": map[string]any{"pullRequest": map[string]any{
						"id":         "PR_7",
						"headRefOid": a.Head,
						"author":     map[string]any{"login": "desk", "type": "User", "id": 1},
						"commits":    map[string]any{"nodes": []any{map[string]any{"commit": map[string]any{"oid": a.Head, "committedDate": date}}}},
					}},
				})
			case strings.Contains(q, `node(id:"PR_7")`):
				nodes := []any{}
				if strings.Contains(q, "items:reviews") {
					for _, r := range a.Reviews {
						nodes = append(nodes, map[string]any{
							"id":                 r["node_id"],
							"user":               r["user"],
							"state":              r["state"],
							"body":               r["body"],
							"submitted_at":       r["submitted_at"],
							"commit":             map[string]any{"oid": r["commit_id"]},
							"html_url":           r["html_url"],
							"author_association": r["author_association"],
							"comments":           map[string]any{"totalCount": 0},
						})
					}
				}
				return data(map[string]any{"node": map[string]any{"items": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false}}}})
			}
		}
		a.Unknown = joined
		return fmt.Errorf("unknown gh call: %s", joined)
	})
}

// This is the GitHub transport adapter, not a replacement for Git. Git's
// remote-helper connect protocol hands its real upload/receive-pack wire
// protocol to a local bare origin, preserving the production remote URL.
func gitRemote(args []string) error {
	if len(args) != 2 || args[1] != "https://github.com/o/r.git" {
		return fmt.Errorf("unknown Git remote: %v", args)
	}
	scan := bufio.NewScanner(os.Stdin)
	for scan.Scan() {
		switch line := scan.Text(); {
		case line == "capabilities":
			fmt.Print("connect\n\n")
		case strings.HasPrefix(line, "option "):
			fmt.Println("unsupported")
		case line == "connect git-upload-pack" || line == "connect git-receive-pack":
			fmt.Println()
			return syscall.Exec("/usr/bin/git", []string{"git", strings.TrimPrefix(line, "connect git-"), "/work/origin.git"}, os.Environ()) //nolint:gosec // the test binary and fixture commands
		default:
			return fmt.Errorf("unknown Git transport call %q", line)
		}
	}
	return scan.Err()
}
