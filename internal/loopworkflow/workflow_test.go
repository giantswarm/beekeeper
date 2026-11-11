//go:build integration

// These tests exercise the public CLI across the loop/parser/merge boundary. No
// production function variable is replaced by them.
package loopworkflow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

func buildCandidate(t *testing.T, overlay string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "beekeeper")
	args := []string{"build", "-o", bin}
	if overlay != "" {
		args = append(args, "-overlay", overlay)
	}
	c := exec.CommandContext(ctx, "go", append(args, ".")...)
	c.Dir = root
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("build candidate: %v\n%s", err, out)
	}
	return bin
}

func TestWorkflow(t *testing.T) {
	binary := os.Getenv("BEEKEEPER_TEST_LOOP_BINARY")
	if binary == "" {
		binary = buildCandidate(t, "")
	}
	for _, scenario := range []string{"no-checkout", "non-go", "checkout", "diverged-go", "red-merge-result", "moved-head", "red-checks", "missing-base", "unavailable-validation", "unreadable-queue", "budget-hold", "failed-start"} {
		t.Run(scenario, func(t *testing.T) { runWorkflow(t, binary, scenario) })
	}
}

func runWorkflow(t *testing.T, binary, scenario string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, text string) {
		t.Helper()
		path := filepath.Join(dir, name)
		must(os.MkdirAll(filepath.Dir(path), 0700))
		must(os.WriteFile(path, []byte(text), 0600))
	}
	must(os.MkdirAll(filepath.Join(dir, "bin"), 0700))
	self, err := os.Executable()
	must(err)
	for _, name := range []string{"gh", "git"} {
		must(os.Symlink(self, filepath.Join(dir, "bin", name)))
	}
	realGit, err := exec.LookPath("git")
	must(err)
	realGo, err := exec.LookPath("go")
	must(err)
	// The command PATH contains only explicitly selected executables. Neither
	// credentials nor production configuration are inherited. Git can use only
	// file transport; Go cannot download modules or toolchains.
	must(os.Symlink(realGo, filepath.Join(dir, "bin", "go")))
	env := []string{
		"HOME=" + filepath.Join(dir, "home"), "XDG_CONFIG_HOME=" + filepath.Join(dir, "home"), "XDG_STATE_HOME=" + filepath.Join(dir, "home"),
		"PATH=" + filepath.Join(dir, "bin"), "LANG=C.UTF-8", "TZ=UTC", envLoadavg, "BEEKEEPER_CONFIG=" + filepath.Join(dir, "config.yaml"),
		"FIXTURE_DIR=" + dir, "FIXTURE_GIT=" + realGit, "FIXTURE_CASE=" + scenario,
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_ALLOW_PROTOCOL=file", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid",
		"GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0",
		"HTTP_PROXY=http://127.0.0.1:1", "HTTPS_PROXY=http://127.0.0.1:1", "NO_PROXY=127.0.0.1",
	}
	var output []byte
	// Preserve only known fixture data. Never upload the ambient environment,
	// user configuration or arbitrary files from the runner.
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		sanitize := func(b []byte) []byte { return []byte(strings.ReplaceAll(string(b), dir, "<fixture>")) }
		trace, _ := os.ReadFile(filepath.Join(dir, "trace.jsonl"))
		events, _ := os.ReadFile(filepath.Join(dir, "state", "events.jsonl"))
		t.Logf("output:\n%s\ncommands:\n%s\nevents:\n%s", sanitize(output), sanitize(trace), sanitize(events))
		if artifacts := os.Getenv("BEEKEEPER_TEST_LOOP_ARTIFACTS"); artifacts != "" {
			dest := filepath.Join(artifacts, strings.ReplaceAll(t.Name(), "/", "-"))
			must(os.MkdirAll(dest, 0700))
			for name, data := range map[string][]byte{"output.log": output, "trace.jsonl": trace, "events.jsonl": events} {
				must(os.WriteFile(filepath.Join(dest, name), sanitize(data), 0600))
			}
			refCtx, refCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer refCancel()
			c := exec.CommandContext(refCtx, realGit, "--git-dir="+filepath.Join(dir, "origin.git"), "show-ref")
			c.Env = env
			refs, _ := c.CombinedOutput()
			must(os.WriteFile(filepath.Join(dest, "refs.txt"), sanitize(refs), 0600))
			bundle := exec.CommandContext(refCtx, realGit, "--git-dir="+filepath.Join(dir, "origin.git"), "bundle", "create", filepath.Join(dest, "origin.bundle"), "--all")
			bundle.Env = env
			if out, err := bundle.CombinedOutput(); err != nil {
				t.Errorf("retaining fixture refs: %v: %s", err, sanitize(out))
			}
		}
	})
	git := func(cwd string, args ...string) string {
		t.Helper()
		c := exec.CommandContext(ctx, realGit, args...)
		c.Dir = filepath.Join(dir, cwd)
		c.Env = env
		b, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("fixture git %v: %v\n%s", args, err, b)
		}
		return strings.TrimSpace(string(b))
	}
	must(os.MkdirAll(filepath.Join(dir, "server"), 0700))
	git("server", "init", "--bare", filepath.Join(dir, "origin.git"))
	git("server", "init", "-b", "main")
	git("server", "remote", "add", "origin", filepath.Join(dir, "origin.git"))
	write("server/README.md", "fixture base\n")
	goRepo := scenario == "checkout" || scenario == "diverged-go" || scenario == "red-merge-result" || scenario == "unavailable-validation"
	if goRepo {
		write("server/go.mod", "module fixture.invalid/project\n\ngo 1.26\n")
		write("server/a.go", "package fixture\nvar A=0\n")
		write("server/b.go", "package fixture\nvar B=0\n")
		write("server/values_test.go", "package fixture\nimport \"testing\"\nfunc TestCombined(t *testing.T) {if A+B>=3 {t.Fatal(\"combined result is red\")}}\n")
	}
	git("server", "add", ".")
	git("server", "commit", "-m", "fixture base")
	git("server", "push", "origin", "main")
	git("server", "checkout", "-b", "change")
	if goRepo {
		write("server/b.go", "package fixture\nvar B=1\n")
	} else {
		write("server/README.md", "fixture change\n")
	}
	git("server", "add", ".")
	git("server", "commit", "-m", "fixture change")
	head := git("server", "rev-parse", "HEAD")
	git("server", "push", "origin", "change")
	git("server", "push", "origin", head+":refs/pull/1/head")
	git("server", "checkout", "main")
	if scenario == "diverged-go" || scenario == "red-merge-result" {
		value := "1"
		if scenario == "red-merge-result" {
			value = "2"
		}
		write("server/a.go", "package fixture\nvar A="+value+"\n")
		git("server", "add", ".")
		git("server", "commit", "-m", "fixture base advanced")
		git("server", "push", "origin", "main")
	}
	base := git("server", "rev-parse", "HEAD")
	checkout := ""
	if scenario != "no-checkout" && scenario != "missing-base" && scenario != "budget-hold" && scenario != "failed-start" && scenario != "moved-head" && scenario != "red-checks" && scenario != "unreadable-queue" {
		git("server", "clone", filepath.Join(dir, "origin.git"), filepath.Join(dir, "checkout"), "--branch", "main")
		// The fetch origin stays local; only its push URL identifies the GitHub
		// repository to production checkout discovery. File-only transport prevents
		// even an accidental push to that URL.
		git("checkout", "remote", "set-url", "--push", "origin", "https://github.com/fixture/project.git")
		checkout = filepath.Join(dir, "checkout")
	}
	if scenario == "unavailable-validation" {
		git("checkout", "remote", "set-url", "origin", filepath.Join(dir, "missing.git"))
	}
	// Lint is the only validation tool adapter; Go tests execute the real Go
	// toolchain on the actual combined tree, with no module dependencies.
	must(os.WriteFile(filepath.Join(dir, "bin", "golangci-lint"), []byte("#!/bin/sh\nexit 0\n"), 0700))
	// /bin/sh is the interpreter only, not a general command search path.
	now := time.Now().UTC()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4900")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(now.Add(time.Hour).Unix()))
		switch r.URL.Path {
		case "/repos/giantswarm/beekeeper":
			fmt.Fprint(w, `{}`)
		case "/graphql":
			fmt.Fprintf(w, `{"data":{"viewer":{"login":"fixture-bot"},"rateLimit":{"limit":5000,"remaining":4900,"used":100,"resetAt":%q}}}`, now.Add(time.Hour).Format(time.RFC3339))
		case "/rate_limit":
			fmt.Fprint(w, `{"resources":{"search":{"limit":30,"remaining":30},"code_search":{"limit":10,"remaining":10}}}`)
		default:
			t.Errorf("unknown HTTP call %s", r.URL.Path)
			http.Error(w, "unknown fixture call", 500)
		}
	}))
	defer api.Close()
	env = append(env, "BEEKEEPER_GITHUB_API="+api.URL)
	floor := -1
	if scenario == "budget-hold" {
		floor = 100
	}
	write("config.yaml", fmt.Sprintf("board:\n  owner: fixture\n  project: 1\n  tidyEvery: -1s\n  order:\n  - name: ready\n    status: [Ready]\ncapacity:\n  ceiling: 0\nidentity:\n  host: fixture\nstateDir: %s\ndisk:\n  checkouts: [%q]\ngithub:\n  floor: %d\n  allowedOrgs: [fixture]\nsupervisor:\n  ownership:\n    authors: [fixture-bot]\n  reviewRounds: 1\n", filepath.Join(dir, "state"), checkout, floor))
	pull := map[string]any{
		"state": "OPEN", "url": "https://github.com/fixture/project/pull/1", "baseRefName": "main", "headRefName": "change", "headRefOid": head,
		"author": map[string]any{"login": "fixture-bot"}, "title": "fixture change", "isDraft": false, "mergeable": "MERGEABLE", "mergeStateStatus": "CLEAN",
		"changedFiles": 1, "files": []any{map[string]any{"path": "b.go"}},
		"statusCheckRollup": []any{map[string]any{"name": "test", "status": "COMPLETED", "conclusion": "SUCCESS"}},
		"reviews":           []any{map[string]any{"id": "R1", "author": map[string]any{"login": "fixture-bot"}, "authorAssociation": "MEMBER", "state": "COMMENTED", "body": "Review: approved\nHead reviewed: `" + head + "`", "commit": map[string]any{"oid": head}, "submittedAt": now.Format(time.RFC3339)}},
	}
	if !goRepo {
		pull["files"] = []any{map[string]any{"path": "README.md"}}
	}
	if scenario == "red-checks" {
		pull["statusCheckRollup"] = []any{map[string]any{"name": "test", "status": "COMPLETED", "conclusion": "FAILURE"}}
	}
	if scenario == "missing-base" {
		delete(pull, "baseRefName")
	}
	raw, err := json.Marshal(pull)
	must(err)
	write("pull.json", string(raw))
	store, err := state.Open(filepath.Join(dir, "state"))
	must(err)
	must(store.Update(func(st *state.State) ([]state.Event, error) {
		st.AutoInstallPaused = true
		st.ReviewParks = []state.ReviewPark{{On: "fixture/project#1", At: now, ReviewHead: head, Agent: "fixture-worker"}}
		st.Budget = &state.Budget{Remaining: 5000, Limit: 5000, Reset: now.Add(time.Hour), At: now, GraphQL: &state.GraphQL{Remaining: 5000, Limit: 5000, Reset: now.Add(time.Hour), At: now}}
		if scenario == "budget-hold" {
			st.Budget.GraphQL.Remaining = 0
		}
		return nil, nil
	}))
	record := func(verb, subject, detail string, at time.Time) {
		must(store.Record(state.Event{At: at, By: state.Party{Name: "supervisor loop"}, Verb: verb, Subject: subject, Detail: detail}))
	}
	record("supervisor.loop", "", "started", now.Add(-time.Minute))
	record("watch.budget-state", "", "false", now)
	if scenario == "failed-start" {
		// Exercise the public start command with no worker executable available.
		// Force bypasses incidental runner capacity; it cannot manufacture a
		// registration when production cannot launch a worker.
		write("worker.md", "Perform fixture maintenance.\n")
		start := exec.CommandContext(ctx, binary, "--as", "fixture-supervisor", "agents", "start", "fixture-worker", filepath.Join(dir, "worker.md"), "--branch", "fixture-task", "--harness", "codex", "--model", "fixture-model", "--force", "--dir", filepath.Join(dir, "server"))
		start.Env = env
		start.Dir = dir
		started, startErr := start.CombinedOutput()
		output = append(output, started...)
		if !strings.Contains(string(started), "executable file not found") {
			t.Fatalf("did not reach missing worker executable refusal: %s", started)
		}
		if startErr == nil {
			t.Fatal("worker unexpectedly started without its executable")
		}
		current, readErr := store.Read()
		must(readErr)
		if len(current.Starts) != 0 || len(current.Agents) != 0 {
			t.Fatal("failed worker start created a durable registration")
		}
	}
	if scenario != "failed-start" {
		record("supervisor.review", "fixture/project#1", "[review fixture/project#1@"+head+"] fixture-reviewer", now)
	}
	// Read raw board metadata and issue responses through the public command.
	// Starts are capped at zero in this boundary fixture, so the task remains
	// available without relying on the runner's capacity or a live subscription.
	boardCmd := exec.CommandContext(ctx, binary, "board", "next", "--json")
	boardCmd.Env = env
	boardCmd.Dir = dir
	boardOutput, boardErr := boardCmd.CombinedOutput()
	output = append(output, boardOutput...)
	must(boardErr)
	if !strings.Contains(string(boardOutput), "fixture/project#2") {
		t.Fatal("raw board response did not reach candidate selection")
	}
	c := exec.CommandContext(ctx, binary, "supervisor", "loop", "--once")
	c.Env = env
	c.Dir = dir
	loopOutput, runErr := c.CombinedOutput()
	output = append(output, loopOutput...)
	err = runErr
	must(err)
	wantMerge := scenario == "no-checkout" || scenario == "non-go" || scenario == "checkout" || scenario == "diverged-go"
	if wantMerge {
		next := exec.CommandContext(ctx, binary, "supervisor", "loop", "--once")
		next.Env = env
		next.Dir = dir
		more, err := next.CombinedOutput()
		output = append(output, more...)
		must(err)
	}
	trace, _ := os.ReadFile(filepath.Join(dir, "trace.jsonl"))
	events, _ := os.ReadFile(filepath.Join(dir, "state", "events.jsonl"))
	if unknown, err := os.ReadFile(filepath.Join(dir, "unknown-call")); err == nil {
		t.Fatalf("unrecognized external command: %s", unknown)
	}
	merged := strings.Contains(string(events), `"verb":"supervisor.merge"`) || strings.Contains(string(events), `"verb":"merged"`)
	after := git("server", "--git-dir="+filepath.Join(dir, "origin.git"), "rev-parse", "refs/heads/main")
	invoked := strings.Contains(string(trace), `"gh","pr","merge"`)
	if wantMerge {
		if strings.Count(string(trace), `"gh","pr","merge"`) != 1 {
			t.Fatal("merge mutation did not happen exactly once")
		}
		current, err := store.Read()
		must(err)
		if len(current.ReviewParks) != 0 || !strings.Contains(string(events), `"verb":"supervisor.end"`) {
			t.Fatal("merged review park did not durably complete")
		}
		if !merged || !invoked || after == base {
			t.Fatal("workflow did not prove a merge of the reviewed change")
		}
		resultRaw, err := os.ReadFile(filepath.Join(dir, "pull.json"))
		must(err)
		var result struct {
			MergeCommit struct {
				OID string `json:"oid"`
			} `json:"mergeCommit"`
		}
		must(json.Unmarshal(resultRaw, &result))
		if after != result.MergeCommit.OID {
			t.Fatal("merge proof does not match the origin base")
		}
		if scenario == "diverged-go" && (!strings.Contains(string(trace), `"worktree","add"`) || git("server", "show", after+":a.go") != "package fixture\nvar A=1") {
			t.Fatal("diverged workflow did not validate and preserve the advanced base")
		}
		if !goRepo && git("server", "show", after+":README.md") != "fixture change" {
			t.Fatal("merged tree lacks reviewed non-Go change")
		}
		if goRepo && git("server", "show", after+":b.go") != "package fixture\nvar B=1" {
			t.Fatal("merged tree lacks reviewed change")
		}
	} else if merged || invoked || after != base {
		t.Fatal("refusal produced merge proof or changed the base")
	}
	for _, check := range []struct{ scenario, text string }{
		{"missing-base", "has no base branch"}, {"red-merge-result", "combined result is red"},
		{"unavailable-validation", "fetching merge-result refs"}, {"budget-hold", "GITHUB HELD"},
		{"failed-start", "no room for a start"}, {"moved-head", "head moved"}, {"unreadable-queue", "reading merge queue"},
	} {
		if scenario == check.scenario && !strings.Contains(string(output)+string(events), check.text) {
			t.Fatalf("did not exercise expected boundary %q", check.text)
		}
	}
}

// Rebuild the real candidate with the historical parser omission. This control
// must refuse before any merge, while the identical complete fixture passes in
// TestWorkflow/no-checkout. It detects a test that bypasses parseLoopPull.
func TestHistoricalParserRegression(t *testing.T) {
	source, err := filepath.Abs("../../cmd/supervisorloop.go")
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	const before = "Base: j.Base,"
	if strings.Count(string(original), before) != 1 {
		t.Fatal("parser mutation no longer targets exactly one assignment")
	}
	mutated := strings.Replace(string(original), before, `Base: "",`, 1)
	dir := t.TempDir()
	replacement := filepath.Join(dir, "supervisorloop.go")
	if err = os.WriteFile(replacement, []byte(mutated), 0600); err != nil {
		t.Fatal(err)
	}
	overlay, err := json.Marshal(map[string]any{"Replace": map[string]string{source: replacement}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "overlay.json")
	if err = os.WriteFile(path, overlay, 0600); err != nil {
		t.Fatal(err)
	}
	mutant := buildCandidate(t, path)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	artifacts := filepath.Join(dir, "artifacts")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	probe := exec.CommandContext(ctx, self, "-test.run=^TestWorkflow$/^no-checkout$", "-test.count=1", "-test.timeout=90s")
	probe.Env = append(os.Environ(), "BEEKEEPER_TEST_LOOP_BINARY="+mutant, "BEEKEEPER_TEST_LOOP_ARTIFACTS="+artifacts)
	out, probeErr := probe.CombinedOutput()
	if probeErr == nil {
		t.Fatal("historical parser mutation did not fail the workflow")
	}
	if ctx.Err() != nil {
		t.Fatalf("mutation control exceeded deadline: %s", out)
	}
	proofDir := filepath.Join(artifacts, "TestWorkflow-no-checkout")
	events, err := os.ReadFile(filepath.Join(proofDir, "events.jsonl"))
	if err != nil {
		t.Fatalf("mutation failure did not retain events: %v\n%s", err, out)
	}
	trace, err := os.ReadFile(filepath.Join(proofDir, "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), "has no base branch") || strings.Contains(string(trace), `"gh","pr","merge"`) {
		t.Fatalf("mutation did not stop before merge: %s", out)
	}
	refs, err := os.ReadFile(filepath.Join(proofDir, "refs.txt"))
	if err != nil || !strings.Contains(string(refs), "refs/heads/main") {
		t.Fatal("mutation failure did not retain Git refs")
	}
	if strings.Contains(string(events), "/tmp/") || strings.Contains(string(trace), "/tmp/") {
		t.Fatal("failure artifacts contain unsanitized fixture paths")
	}
	bundle, err := os.ReadFile(filepath.Join(proofDir, "origin.bundle"))
	if err != nil || len(bundle) == 0 {
		t.Fatal("mutation failure did not retain a Git bundle")
	}
	// Keep the expected failure proof available in CI too.
	if dest := os.Getenv("BEEKEEPER_TEST_LOOP_ARTIFACTS"); dest != "" {
		dest = filepath.Join(dest, "historical-control")
		if err := os.MkdirAll(dest, 0700); err != nil {
			t.Fatal(err)
		}
		for name, raw := range map[string][]byte{"events.jsonl": events, "trace.jsonl": trace, "refs.txt": refs, "origin.bundle": bundle} {
			if err := os.WriteFile(filepath.Join(dest, name), raw, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}

}
