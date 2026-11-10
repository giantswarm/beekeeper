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

	"github.com/google/uuid"
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
	deliveryScenario = "delivery"
	dataField        = "data"
	nodesField       = "nodes"
	statusField      = "status"
	numberField      = "number"
	repositoryField  = "repository"
	sessionIDField   = "sessionId"
	fixtureRepo      = "fixture/project"
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if os.Getenv("FIXTURE_CANDIDATE") != "" && slices.Contains(os.Args[1:], "get-url") && slices.Contains(os.Args[1:], originRemote) {
		check := exec.CommandContext(ctx, os.Getenv("FIXTURE_GIT"), os.Args[1:]...) //nolint:gosec // scratch fixture git check
		if check.Run() == nil {
			fmt.Println("https://github.com/fixture/project.git")
			return 0
		}
	}
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
	if code, handled := deliveryGitHub(args); handled {
		return code
	}
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
	case starts(ghAPI, "--include") && len(args) >= 3 && args[2] == "repos/fixture/project/pulls?state=all&sort=updated&direction=desc&per_page=1":
		fmt.Print("HTTP/2.0 200 OK\r\nETag: fixture\r\nContent-Type: application/json\r\n\r\n[]")
		return 0
	case starts(ghAPI, ghGraphQL) && slices.ContainsFunc(args, func(s string) bool { return strings.Contains(s, "pullRequests(states:OPEN,") }):
		return emit(map[string]any{dataField: map[string]any{repositoryField: map[string]any{"pullRequests": map[string]any{nodesField: []any{}, "pageInfo": map[string]any{"hasNextPage": false}}}}})
	case starts(ghPR, "list"):
		return emit([]any{})
	// This fixture repository has a readable main branch and no release
	// workflows. Delivery recovery must observe that explicitly, without
	// allowing unrelated or unrecognized external requests.
	case starts(ghAPI, "-H", "Accept: application/vnd.github.raw") && len(args) == 4 && fixtureWorkflowRead(args[3]):
		fmt.Fprintln(os.Stderr, "gh: Not Found (HTTP 404)")
		return 1
	case starts(ghAPI, "repos/fixture/project/branches/main", "--jq", ".name") && len(args) == 4:
		fmt.Println(mainBranch)
		return 0
	case starts(ghAPI, "user"):
		fmt.Println(fixtureBot)
		return 0
	case starts(ghAPI, "repos/fixture/project/pulls/1"):
		return emit(map[string]any{"head": map[string]any{"sha": p["headRefOid"], "ref": p["headRefName"], "repo": map[string]any{"full_name": fixtureRepo}}, "user": map[string]any{loginField: fixtureBot}})
	case starts(ghAPI, ghGraphQL) && slices.ContainsFunc(args, func(s string) bool { return strings.Contains(s, "issueOrPullRequest(number:") }):
		return emit(map[string]any{dataField: map[string]any{"r0": map[string]any{"issueOrPullRequest": map[string]any{"__typename": "PullRequest", stateField: p[stateField]}}}})
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
		return emit(map[string]any{dataField: map[string]any{repositoryField: map[string]any{"mergeQueue": nil}}})
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
		recordUnknownCall(root, raw)
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
	case os.Getenv("FIXTURE_CASE") == deliveryScenario && slices.Contains(args, "SendMessage"):
		// The fixture worker accepts the supervisor's findings notification;
		// the integration test then drives that worker's deterministic turn.
		fmt.Println(`{"type":"user","message":{"content":[{"type":"tool_result","content":"{\"success\":true}"}]}}`)
		return 0
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
	case os.Getenv("FIXTURE_CASE") == deliveryScenario && slices.Contains(args, "--bg") && slices.Contains(args, "--resume"):
		idx := slices.Index(args, "--resume")
		if idx+1 >= len(args) {
			panic("fixture resume lacks session")
		}
		id := args[idx+1]
		fmt.Printf("backgrounded · %s · fixture resumed worker\n", id[:min(8, len(id))])
		return 0
	case slices.Contains(args, "--bg"):
		name := "worker"
		if idx := slices.Index(args, "-n"); idx >= 0 && idx+1 < len(args) {
			name = args[idx+1]
		}
		rows := readSessions()
		// Retired sessions disappear from rows; their IDs must never be reused.
		sessionID := uuid.NewString()
		short := sessionID[:8]
		rows = append(rows, map[string]any{
			"id":           short,
			sessionIDField: sessionID,
			nameField:      name,
			"kind":         "bg",
			"pid":          1,
		})
		writeSessions(rows)
		if os.Getenv("FIXTURE_CASE") == deliveryScenario {
			// Real starts write a transcript before a parked session can resume.
			// Keep it in the fixture's isolated Claude project tree.
			dir := filepath.Join(root, "home", ".claude", "projects", "fixture")
			if err := os.MkdirAll(dir, 0700); err != nil { //nolint:gosec // isolated scratch fixture
				panic(err)
			}
			cwd, err := os.Getwd()
			if err != nil {
				panic(err)
			}
			raw, err := json.Marshal(map[string]any{"cwd": cwd, sessionIDField: sessionID, "type": "assistant"})
			if err != nil {
				panic(err)
			}
			if err := os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), append(raw, '\n'), 0600); err != nil { //nolint:gosec // isolated scratch fixture transcript
				panic(err)
			}
		}
		if strings.HasPrefix(name, "Review ") && os.Getenv("FIXTURE_CASE") != deliveryScenario {
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
			c := exec.CommandContext(ctx, filepath.Join(root, "bin", ghCmd), ghPR, "review", "1", "--repo", fixtureRepo, "--comment", "--body", body) //nolint:gosec // scratch fixture reviewer command
			c.Env = append(os.Environ(), "FIXTURE_DIR="+root)
			if out, err := c.CombinedOutput(); err != nil {
				panic(fmt.Sprintf("fixture reviewer gh pr review: %v: %s", err, out))
			}
		}
		fmt.Printf("backgrounded · %s · %s\n", short, name)
		return 0
	default:
		raw, _ := json.Marshal(append([]string{claudeCmd}, args...))
		recordUnknownCall(root, raw)
		fmt.Fprintln(os.Stderr, "unknown fixture claude command: "+string(raw))
		return 1
	}
}

// fixtureWorkflowRead recognizes raw workflow reads only in the isolated
// repository at its main ref. Unknown endpoints still fail closed.
func fixtureWorkflowRead(endpoint string) bool {
	name, ok := strings.CutPrefix(endpoint, "repos/fixture/project/contents/.github/workflows/")
	if !ok {
		return false
	}
	name, ok = strings.CutSuffix(name, "?ref=main")
	return ok && name != "" && !strings.ContainsAny(name, "/?%") && name != "." && name != ".."
}

// recordUnknownCall preserves the first unsupported operation: follow-up calls
// while handling that failure must not overwrite its cause.
func recordUnknownCall(root string, raw []byte) {
	f, err := os.OpenFile(filepath.Join(root, "unknown-call"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600) //nolint:gosec // scratch fixture state
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(raw)
}
