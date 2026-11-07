// Package loopworkflow is a scratch GitHub and Git origin that exercises a real
// beekeeper binary across the loop, parser and merge boundary: the integration
// tests drive every scenario, and a recovery controller's probe drives one on
// an idle desk. Nothing here touches a real desk, account or network.
package loopworkflow

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	gitCmd           = "git"
	ghCmd            = "gh"
	claudeCmd        = "claude"
	fixtureBot       = "fixture-bot"
	changeBranch     = "change"
	mainBranch       = "main"
	gitInit          = "init"
	gitCheckout      = "checkout"
	gitCommit        = "commit"
	gitPush          = "push"
	gitRevParse      = "rev-parse"
	gitHead          = "HEAD"
	originRemote     = "origin"
	serverDir        = "server"
	pullJSONFile     = "pull.json"
	loginField       = "login"
	stateField       = "state"
	authorField      = "author"
	oidField         = "oid"
	nameField        = "name"
	reviewsField     = "reviews"
	openState        = "OPEN"
	mergedState      = "MERGED"
	cleanStatus      = "CLEAN"
	completedStatus  = "COMPLETED"
	readmeFile       = "README.md"
	fixtureChangeMsg = "fixture change"
	ghAPI            = "api"
	ghGraphQL        = "graphql"
	ghPR             = "pr"
)

// Serve runs this process as the fixture's GitHub CLI, Git wrapper or Claude CLI
// and returns its exit code. Only a process started by that name inside a fixture is one.
func Serve() (int, bool) {
	if os.Getenv("CANARY_SANDBOX_DIR") != "" {
		switch filepath.Base(os.Args[0]) {
		case claudeCmd:
			return serveCanaryClaude(), true
		case gitCmd:
			return serveCanaryGit(), true
		}
	}
	if os.Getenv("FIXTURE_DIR") == "" {
		return 0, false
	}
	switch filepath.Base(os.Args[0]) {
	case ghCmd:
		return serveGitHub(), true
	case gitCmd:
		return traceGit(), true
	case claudeCmd:
		return serveClaude(), true
	}
	return 0, false
}

func trace(args []string) {
	f, err := os.OpenFile(filepath.Join(os.Getenv("FIXTURE_DIR"), "trace.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600) //nolint:gosec // scratch fixture trace
	if err != nil {
		panic(err)
	}
	defer func() { _ = f.Close() }()
	if err = json.NewEncoder(f).Encode(args); err != nil {
		panic(err)
	}
}

func traceGit() int {
	trace(append([]string{gitCmd}, os.Args[1:]...))
	if slices.Contains(os.Args[1:], "get-url") && slices.Contains(os.Args[1:], originRemote) {
		fmt.Println("https://github.com/fixture/project.git")
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, os.Getenv("FIXTURE_GIT"), os.Args[1:]...) //nolint:gosec // scratch fixture git wrapper
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			return e.ExitCode()
		}
		return 1
	}
	return 0
}

func serveGitHub() int {
	args := os.Args[1:]
	trace(append([]string{ghCmd}, args...))
	root := os.Getenv("FIXTURE_DIR")
	raw, err := os.ReadFile(filepath.Join(root, pullJSONFile)) //nolint:gosec // scratch fixture state
	if err != nil {
		panic(err)
	}
	var p map[string]any
	if err = json.Unmarshal(raw, &p); err != nil {
		panic(err)
	}
	starts := func(prefix ...string) bool {
		return len(args) >= len(prefix) && slices.Equal(args[:len(prefix)], prefix)
	}
	emit := func(v any) int {
		if err := json.NewEncoder(os.Stdout).Encode(v); err != nil {
			panic(err)
		}
		return 0
	}
	switch {
	case starts("auth", "token"):
		fmt.Println("fixture-token")
		return 0
	case starts("search", "prs"):
		return emit([]any{})
	case starts(ghPR, "list"):
		return emit([]any{})
	case starts(ghAPI, "user"):
		fmt.Println(fixtureBot)
		return 0
	case starts(ghAPI, "repos/fixture/project/pulls/1"):
		return emit(map[string]any{"head": map[string]any{"sha": p["headRefOid"], "ref": p["headRefName"], "repo": map[string]any{"full_name": "fixture/project"}}, "user": map[string]any{loginField: fixtureBot}})
	case starts(ghAPI, ghGraphQL) && slices.ContainsFunc(args, func(s string) bool { return strings.Contains(s, "issueOrPullRequest(number:") }):
		return emit(map[string]any{"data": map[string]any{"r0": map[string]any{"issueOrPullRequest": map[string]any{"__typename": "PullRequest", stateField: p[stateField]}}}})
	case starts(ghPR, "review"):
		head, _ := p["headRefOid"].(string)
		body := "Desk review (opus): approved\nHead reviewed: `" + head + "`"
		if idx := slices.Index(args, "--body-file"); idx >= 0 && idx+1 < len(args) {
			if b, rerr := os.ReadFile(args[idx+1]); rerr == nil { //nolint:gosec // scratch fixture review file
				body = strings.TrimSpace(string(b))
			}
		} else if idx := slices.Index(args, "--body"); idx >= 0 && idx+1 < len(args) {
			body = args[idx+1]
		}
		p[reviewsField] = []any{map[string]any{
			"id":                "R1",
			authorField:         map[string]any{loginField: fixtureBot},
			"authorAssociation": "MEMBER",
			stateField:          "COMMENTED",
			"body":              body,
			gitCommit:           map[string]any{oidField: head},
			"submittedAt":       time.Now().UTC().Add(2 * time.Second).Format(time.RFC3339),
		}}
		raw, err = json.Marshal(p)
		if err != nil {
			panic(err)
		}
		if err = os.WriteFile(filepath.Join(root, pullJSONFile), raw, 0600); err != nil { //nolint:gosec // scratch fixture state
			panic(err)
		}
		fmt.Println("https://github.com/fixture/project/pull/1#pullrequestreview-1")
		return 0
	case starts(ghAPI, "-i") && len(args) == 3 && slices.Contains([]string{"repos/fixture/project/issues/1/comments?per_page=100&page=1", "repos/fixture/project/issues/2/comments?per_page=100&page=1"}, args[2]):
		fmt.Print("HTTP/2.0 200 OK\r\nContent-Type: application/json\r\n\r\n[]")
		return 0
	case starts(ghAPI, "--paginate") && len(args) == 3 && slices.Contains([]string{"repos/fixture/project/pulls/1/reviews", "repos/fixture/project/pulls/1/comments", "repos/fixture/project/issues/1/comments"}, args[2]):
		return emit([]any{})
	case starts(ghAPI) && len(args) == 4 && strings.HasPrefix(args[1], "repos/fixture/project/commits/") && args[2] == "--jq" && args[3] == ".commit.committer.date":
		fmt.Println("2026-01-01T00:00:00Z")
		return 0
	case starts(ghPR, "view"):
		// Model a push between the planner's read and the merge executor's read.
		if os.Getenv("FIXTURE_CASE") == "moved-head" {
			countPath := filepath.Join(root, "read-count")
			b, _ := os.ReadFile(countPath) //nolint:gosec // scratch fixture state
			if len(b) > 0 {
				p["headRefOid"] = strings.Repeat("f", 40)
			}
			if err := os.WriteFile(countPath, []byte("read"), 0600); err != nil { //nolint:gosec // scratch fixture state
				panic(err)
			}
		}
		return emit(p)
	case starts(ghAPI, ghGraphQL) && slices.ContainsFunc(args, func(s string) bool { return strings.Contains(s, "fields(first:50)") }):
		fmt.Println(`{"data":{"repositoryOwner":{"projectV2":{"id":"P","fields":{"nodes":[{"id":"S","name":"Status","options":[{"id":"ready","name":"Ready"}]}]}}}}}`)
		return 0
	case starts(ghAPI, ghGraphQL) && slices.ContainsFunc(args, func(s string) bool { return strings.Contains(s, "items(first:") }):
		fmt.Println(`{"data":{"repositoryOwner":{"projectV2":{"items":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":"I","status":{"name":"Ready"},"content":{"number":2,"url":"https://github.com/fixture/project/issues/2","title":"fixture task","state":"OPEN","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z","repository":{"nameWithOwner":"fixture/project"},"blockedBy":{"totalCount":0,"nodes":[]},"subIssuesSummary":{"total":0,"completed":0}}}]}}}}}`)
		return 0
	case starts(ghAPI, ghGraphQL) && slices.ContainsFunc(args, func(s string) bool { return strings.Contains(s, "mergeQueue(branch:") }):
		if os.Getenv("FIXTURE_CASE") == "unreadable-queue" {
			return emit(map[string]any{"errors": []any{map[string]any{"message": "fixture unavailable"}}})
		}
		return emit(map[string]any{"data": map[string]any{"repository": map[string]any{"mergeQueue": nil}}})
	case starts(ghPR, "merge"):
		pin := slices.Index(args, "--match-head-commit")
		if pin < 0 || pin+1 >= len(args) || args[pin+1] != p["headRefOid"] {
			panic("merge lacks current exact-head pin")
		}
		// Perform the server-side squash in a local checkout and push the base.
		// A successful CLI invocation alone never constitutes merge proof.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		git := func(a ...string) string {
			c := exec.CommandContext(ctx, os.Getenv("FIXTURE_GIT"), a...) //nolint:gosec // scratch fixture git
			c.Dir = filepath.Join(root, serverDir)
			out, err := c.CombinedOutput()
			if err != nil {
				panic(fmt.Sprintf("server git %v: %v: %s", a, err, out))
			}
			return strings.TrimSpace(string(out))
		}
		git("fetch", originRemote, changeBranch)
		git("merge", "--squash", "FETCH_HEAD")
		git(gitCommit, "-m", "fixture merge")
		sha := git(gitRevParse, gitHead)
		git(gitPush, originRemote, mainBranch)
		p[stateField] = mergedState
		p["mergeCommit"] = map[string]any{oidField: sha}
		raw, err = json.Marshal(p)
		if err != nil {
			panic(err)
		}
		if err = os.WriteFile(filepath.Join(root, pullJSONFile), raw, 0600); err != nil { //nolint:gosec // scratch fixture state
			panic(err)
		}
		return emit(map[string]any{})
	default:
		// Retain unknown calls separately: production may intentionally handle a
		// tooling failure, but the harness must never silently accept one.
		raw, _ := json.Marshal(args)
		_ = os.WriteFile(filepath.Join(root, "unknown-call"), raw, 0600) //nolint:gosec // scratch fixture state
		fmt.Fprintln(os.Stderr, "unknown fixture command: "+string(raw))
		return 1
	}
}

func serveClaude() int {
	args := os.Args[1:]
	trace(append([]string{claudeCmd}, args...))
	root := os.Getenv("FIXTURE_DIR")
	sessionsPath := filepath.Join(root, "sessions.json")
	readSessions := func() []map[string]any {
		raw, err := os.ReadFile(sessionsPath) //nolint:gosec // scratch fixture state
		if err != nil {
			return []map[string]any{}
		}
		var rows []map[string]any
		_ = json.Unmarshal(raw, &rows)
		return rows
	}
	writeSessions := func(rows []map[string]any) {
		raw, err := json.Marshal(rows)
		if err != nil {
			panic(err)
		}
		if err := os.WriteFile(sessionsPath, raw, 0600); err != nil { //nolint:gosec // scratch fixture state
			panic(err)
		}
	}
	switch {
	case len(args) > 0 && args[0] == "plugin":
		fmt.Println("[]")
		return 0
	case len(args) > 0 && args[0] == "agents":
		if err := json.NewEncoder(os.Stdout).Encode(readSessions()); err != nil {
			panic(err)
		}
		return 0
	case len(args) > 0 && args[0] == "stop":
		if len(args) > 1 {
			short := args[1]
			rows := slices.DeleteFunc(readSessions(), func(r map[string]any) bool {
				id, _ := r["id"].(string)
				return id == short
			})
			writeSessions(rows)
		}
		return 0
	case slices.Contains(args, "--bg"):
		name := "worker"
		if idx := slices.Index(args, "-n"); idx >= 0 && idx+1 < len(args) {
			name = args[idx+1]
		}
		rows := readSessions()
		short := fmt.Sprintf("a1b2c3%02x", len(rows)+1)
		rows = append(rows, map[string]any{
			"id":        short,
			"sessionId": short + "-0000-0000-0000-000000000001",
			nameField:   name,
			"kind":      "bg",
			"pid":       1,
		})
		writeSessions(rows)
		if strings.HasPrefix(name, "Review ") {
			raw, err := os.ReadFile(filepath.Join(root, pullJSONFile)) //nolint:gosec // scratch fixture state
			if err != nil {
				panic(err)
			}
			var p map[string]any
			if err := json.Unmarshal(raw, &p); err != nil {
				panic(err)
			}
			head, _ := p["headRefOid"].(string)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			body := "Desk review (opus): approved\nHead reviewed: `" + head + "`"
			c := exec.CommandContext(ctx, filepath.Join(root, "bin", ghCmd), ghPR, "review", "1", "--repo", "fixture/project", "--comment", "--body", body) //nolint:gosec // scratch fixture reviewer command
			c.Env = append(os.Environ(), "FIXTURE_DIR="+root)
			if out, err := c.CombinedOutput(); err != nil {
				panic(fmt.Sprintf("fixture reviewer gh pr review: %v: %s", err, out))
			}
		}
		fmt.Printf("backgrounded · %s · %s\n", short, name)
		return 0
	default:
		raw, _ := json.Marshal(append([]string{claudeCmd}, args...))
		_ = os.WriteFile(filepath.Join(root, "unknown-call"), raw, 0600) //nolint:gosec // scratch fixture state
		fmt.Fprintln(os.Stderr, "unknown fixture claude command: "+string(raw))
		return 1
	}
}
