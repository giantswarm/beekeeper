//go:build integration

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
	"testing"
	"time"
)

// The test executable doubles only as the external GitHub CLI and a Git
// tracing wrapper. Beekeeper itself is always a separately built binary.
func TestMain(m *testing.M) {
	switch filepath.Base(os.Args[0]) {
	case "gh":
		os.Exit(serveGitHub())
	case "git":
		os.Exit(traceGit())
	default:
		os.Exit(m.Run())
	}
}

func trace(args []string) {
	f, err := os.OpenFile(filepath.Join(os.Getenv("FIXTURE_DIR"), "trace.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if err = json.NewEncoder(f).Encode(args); err != nil {
		panic(err)
	}
}

func traceGit() int {
	trace(append([]string{"git"}, os.Args[1:]...))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, os.Getenv("FIXTURE_GIT"), os.Args[1:]...)
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
	trace(append([]string{"gh"}, args...))
	root := os.Getenv("FIXTURE_DIR")
	raw, err := os.ReadFile(filepath.Join(root, "pull.json"))
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
	case starts("api", "user"):
		fmt.Println("fixture-bot")
		return 0
	case starts("api", "repos/fixture/project/pulls/1"):
		return emit(map[string]any{"head": map[string]any{"sha": p["headRefOid"]}, "user": map[string]any{"login": "fixture-bot"}})
	case starts("api", "-i") && len(args) == 3 && slices.Contains([]string{"repos/fixture/project/issues/1/comments?per_page=100&page=1", "repos/fixture/project/issues/2/comments?per_page=100&page=1"}, args[2]):
		fmt.Print("HTTP/2.0 200 OK\r\nContent-Type: application/json\r\n\r\n[]")
		return 0
	case starts("api", "--paginate") && len(args) == 3 && slices.Contains([]string{"repos/fixture/project/pulls/1/reviews", "repos/fixture/project/pulls/1/comments", "repos/fixture/project/issues/1/comments"}, args[2]):
		return emit([]any{})
	case starts("api") && len(args) == 4 && strings.HasPrefix(args[1], "repos/fixture/project/commits/") && args[2] == "--jq" && args[3] == ".commit.committer.date":
		fmt.Println("2026-01-01T00:00:00Z")
		return 0
	case starts("pr", "view"):
		// Model a push between the planner's read and the merge executor's read.
		if os.Getenv("FIXTURE_CASE") == "moved-head" {
			countPath := filepath.Join(root, "read-count")
			b, _ := os.ReadFile(countPath)
			if len(b) > 0 {
				p["headRefOid"] = strings.Repeat("f", 40)
			}
			if err := os.WriteFile(countPath, []byte("read"), 0600); err != nil {
				panic(err)
			}
		}
		return emit(p)
	case starts("api", "graphql") && slices.ContainsFunc(args, func(s string) bool { return strings.Contains(s, "fields(first:50)") }):
		fmt.Println(`{"data":{"repositoryOwner":{"projectV2":{"id":"P","fields":{"nodes":[{"id":"S","name":"Status","options":[{"id":"ready","name":"Ready"}]}]}}}}}`)
		return 0
	case starts("api", "graphql") && slices.ContainsFunc(args, func(s string) bool { return strings.Contains(s, "items(first:") }):
		fmt.Println(`{"data":{"repositoryOwner":{"projectV2":{"items":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":"I","status":{"name":"Ready"},"content":{"number":2,"url":"https://github.com/fixture/project/issues/2","title":"fixture task","state":"OPEN","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z","repository":{"nameWithOwner":"fixture/project"},"blockedBy":{"totalCount":0,"nodes":[]},"subIssuesSummary":{"total":0,"completed":0}}}]}}}}}`)
		return 0
	case starts("api", "graphql") && slices.ContainsFunc(args, func(s string) bool { return strings.Contains(s, "mergeQueue(branch:") }):
		if os.Getenv("FIXTURE_CASE") == "unreadable-queue" {
			return emit(map[string]any{"errors": []any{map[string]any{"message": "fixture unavailable"}}})
		}
		return emit(map[string]any{"data": map[string]any{"repository": map[string]any{"mergeQueue": nil}}})
	case starts("pr", "merge"):
		pin := slices.Index(args, "--match-head-commit")
		if pin < 0 || pin+1 >= len(args) || args[pin+1] != p["headRefOid"] {
			panic("merge lacks current exact-head pin")
		}
		// Perform the server-side squash in a local checkout and push the base.
		// A successful CLI invocation alone never constitutes merge proof.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		git := func(a ...string) string {
			c := exec.CommandContext(ctx, os.Getenv("FIXTURE_GIT"), a...)
			c.Dir = filepath.Join(root, "server")
			out, err := c.CombinedOutput()
			if err != nil {
				panic(fmt.Sprintf("server git %v: %v: %s", a, err, out))
			}
			return strings.TrimSpace(string(out))
		}
		git("fetch", "origin", "change")
		git("merge", "--squash", "FETCH_HEAD")
		git("commit", "-m", "fixture merge")
		sha := git("rev-parse", "HEAD")
		git("push", "origin", "main")
		p["state"] = "MERGED"
		p["mergeCommit"] = map[string]any{"oid": sha}
		raw, err = json.Marshal(p)
		if err != nil {
			panic(err)
		}
		if err = os.WriteFile(filepath.Join(root, "pull.json"), raw, 0600); err != nil {
			panic(err)
		}
		return emit(map[string]any{})
	default:
		// Retain unknown calls separately: production may intentionally handle a
		// tooling failure, but the harness must never silently accept one.
		raw, _ := json.Marshal(args)
		_ = os.WriteFile(filepath.Join(root, "unknown-call"), raw, 0600)
		fmt.Fprintln(os.Stderr, "unknown fixture command: "+string(raw))
		return 1
	}
}
