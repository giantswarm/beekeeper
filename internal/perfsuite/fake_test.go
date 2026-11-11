//go:build perf && linux

package perfsuite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// fakeDirEnv names the fixture directory to a fake: its trace and its
// unknown calls go there. A process without it is the test binary itself.
const fakeDirEnv = "PERF_FAKE_DIR"

// fakes are the executables a desk's PATH holds, each this test binary under
// that name. Every call is traced, so the suite counts the processes a
// command starts.
var fakes = []string{"gh", "git", "ssh", "claude", "codex", "agy", "omp", "devctl", "journalctl"}

const fixtureRepo = "fixture/project"

func serveFake() (int, bool) {
	dir := os.Getenv(fakeDirEnv)
	if dir == "" {
		return 0, false
	}
	name := filepath.Base(os.Args[0])
	if !slices.Contains(fakes, name) {
		return 0, false
	}
	args := os.Args[1:]
	trace(dir, append([]string{name}, args...))
	switch name {
	case "gh":
		return fakeGitHub(dir, args), true
	case "git":
		return fakeGit(args), true
	case "ssh":
		fmt.Fprintln(os.Stderr, "ssh: connect to host: fixture has no network")
		return 255, true
	case "codex":
		return fakeCodex(args), true
	case "journalctl":
		return 0, true
	}
	unknown(dir, name, args)
	fmt.Fprintf(os.Stderr, "%s: not available in the performance fixture\n", name)
	return 1, true
}

func trace(dir string, args []string) {
	raw, err := json.Marshal(args)
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "trace.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // fixture trace
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(append(raw, '\n'))
}

// unknown keeps a call no fake answers. The suite reports them; production
// may handle a failed tool, so they do not fail a budget by themselves.
func unknown(dir, name string, args []string) {
	raw, err := json.Marshal(append([]string{name}, args...))
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "unknown.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // fixture trace
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(append(raw, '\n'))
}

func fakeGit(args []string) int {
	gitPath := os.Getenv("PERF_REAL_GIT")
	if gitPath == "" {
		fmt.Fprintln(os.Stderr, "git: not available in the performance fixture")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, gitPath, args...) //nolint:gosec // fixture git wrapper
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		var e *exec.ExitError
		if errors.As(err, &e) {
			return e.ExitCode()
		}
		return 1
	}
	return 0
}

// fakeCodex stands in for codex exec in a footprint runner: it starts a
// thread and a turn, then works (sleeps) for PERF_CODEX_SLEEP.
func fakeCodex(args []string) int {
	if len(args) == 0 || args[0] != "exec" {
		fmt.Fprintln(os.Stderr, "codex: not available in the performance fixture")
		return 1
	}
	_, _ = os.Stdin.Read(make([]byte, 4096))
	fmt.Println(`{"type":"thread.started","thread_id":"0199a000-0000-7000-8000-000000000001"}`)
	fmt.Println(`{"type":"turn.started"}`)
	if d, err := time.ParseDuration(os.Getenv("PERF_CODEX_SLEEP")); err == nil {
		time.Sleep(d)
	}
	fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`)
	return 0
}

func pullCount() int {
	n, err := strconv.Atoi(os.Getenv("PERF_PULLS"))
	if err != nil {
		return 150
	}
	return n
}

// fakePull is an open pull request of the desk's in the shape gh's --json
// fields and the GraphQL nodes share.
func fakePull(i int) map[string]any {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
	head := fmt.Sprintf("%040x", 0xbee0000+i)
	return map[string]any{
		"number": 1000 + i, "url": fmt.Sprintf("https://github.com/%s/pull/%d", fixtureRepo, 1000+i),
		"title": fmt.Sprintf("fix: fixture change %d", i), "state": "OPEN", "isDraft": false,
		"author":      map[string]any{"login": "fixture-bot"},
		"repository":  map[string]any{"nameWithOwner": fixtureRepo, "name": "project"},
		"headRefName": fmt.Sprintf("fixture-%d", i), "headRefOid": head, "baseRefName": "main",
		"createdAt": at, "updatedAt": at, "labels": []any{}, "body": "Closes https://github.com/" + fixtureRepo + "/issues/" + strconv.Itoa(i+1),
		"mergeable": "MERGEABLE", "mergeStateStatus": "CLEAN", "changedFiles": 1,
		"statusCheckRollup": []any{map[string]any{"name": "test", "status": "COMPLETED", "conclusion": "SUCCESS"}},
		"reviews":           []any{},
	}
}

func fakePulls() []any {
	out := make([]any, 0, pullCount())
	for i := range pullCount() {
		out = append(out, fakePull(i))
	}
	return out
}

func fakeBoardItems() []any {
	out := make([]any, 0, pullCount())
	statuses := []string{"Ready", "In Progress", "Blocked", "Inbox"}
	for i := range pullCount() {
		out = append(out, map[string]any{
			"id": fmt.Sprintf("I%d", i), "status": map[string]any{"name": statuses[i%len(statuses)]},
			"content": map[string]any{
				"number": i + 1, "url": fmt.Sprintf("https://github.com/%s/issues/%d", fixtureRepo, i+1),
				"title": fmt.Sprintf("fixture task %d", i+1), "state": "OPEN",
				"createdAt": "2026-11-01T00:00:00Z", "updatedAt": "2026-11-01T00:00:00Z",
				"repository":       map[string]any{"nameWithOwner": fixtureRepo},
				"blockedBy":        map[string]any{"totalCount": 0, "nodes": []any{}},
				"subIssuesSummary": map[string]any{"total": 0, "completed": 0},
			},
		})
	}
	return out
}

func fakeGitHub(dir string, args []string) int {
	starts := func(prefix ...string) bool {
		return len(args) >= len(prefix) && slices.Equal(args[:len(prefix)], prefix)
	}
	has := func(s string) bool {
		return slices.ContainsFunc(args, func(a string) bool { return strings.Contains(a, s) })
	}
	emit := func(v any) int {
		if err := json.NewEncoder(os.Stdout).Encode(v); err != nil {
			return 1
		}
		return 0
	}
	switch {
	case starts("auth", "token"):
		fmt.Println("fixture-token")
		return 0
	case starts("auth", "status"):
		fmt.Println("github.com: logged in to github.com account fixture-bot")
		return 0
	case starts("api", "user"):
		if has("--jq") {
			fmt.Println("fixture-bot")
			return 0
		}
		return emit(map[string]any{"login": "fixture-bot"})
	case starts("search", "prs"), starts("pr", "list"):
		return emit(fakePulls())
	case starts("pr", "view"):
		n := 1000
		if len(args) > 2 {
			ref := args[2][strings.LastIndexByte(args[2], '/')+1:]
			if v, err := strconv.Atoi(ref); err == nil {
				n = v
			}
		}
		return emit(fakePull(n - 1000))
	case starts("api", "--paginate") && len(args) > 2 && strings.Contains(args[2], "/actions/runs"):
		return emit(map[string]any{"total_count": 0, "workflow_runs": []any{}})
	case starts("api", "rate_limit"):
		return emit(map[string]any{"resources": map[string]any{
			"core":        map[string]any{"limit": 5000, "remaining": 4900, "reset": time.Now().Add(time.Hour).Unix()},
			"graphql":     map[string]any{"limit": 5000, "remaining": 4900, "reset": time.Now().Add(time.Hour).Unix()},
			"search":      map[string]any{"limit": 30, "remaining": 30, "reset": time.Now().Add(time.Minute).Unix()},
			"code_search": map[string]any{"limit": 10, "remaining": 10, "reset": time.Now().Add(time.Minute).Unix()},
		}})
	case starts("api", "graphql") && has("fields(first:"):
		fmt.Println(`{"data":{"repositoryOwner":{"projectV2":{"id":"P","fields":{"nodes":[{"id":"S","name":"Status","options":[{"id":"ready","name":"Ready"},{"id":"progress","name":"In Progress"},{"id":"blocked","name":"Blocked"},{"id":"inbox","name":"Inbox"},{"id":"done","name":"Done"}]}]}}}}}`)
		return 0
	case starts("api", "graphql") && has("items(first:"):
		return emit(map[string]any{"data": map[string]any{"repositoryOwner": map[string]any{"projectV2": map[string]any{"items": map[string]any{"pageInfo": map[string]any{"hasNextPage": false}, "nodes": fakeBoardItems()}}}}})
	case starts("api", "graphql") && has("pullRequests("):
		return emit(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequests": map[string]any{"nodes": fakePulls(), "pageInfo": map[string]any{"hasNextPage": false}}}}})
	case starts("api", "graphql") && has("rateLimit"):
		return emit(map[string]any{"data": map[string]any{"viewer": map[string]any{"login": "fixture-bot"}, "rateLimit": map[string]any{"limit": 5000, "remaining": 4900, "used": 100, "resetAt": time.Now().Add(time.Hour).Format(time.RFC3339)}}})
	}
	unknown(dir, "gh", args)
	fmt.Fprintln(os.Stderr, "gh: Not Found (HTTP 404)")
	return 1
}
