package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoWith makes a git repository under dir holding files.
func repoWith(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// readsHook is a hook whose project is project and whose markers live in
// a temporary directory.
func readsHook(t *testing.T, project string) Hook {
	h := hook()
	h.Project = project
	h.Reads = ReadsMarker{Dir: t.TempDir()}.First
	return h
}

// call is a PreToolUse event as Claude Code sends it, in session s1 unless
// it names another.
type call struct {
	Tool    string         `json:"tool_name"`
	Input   map[string]any `json:"tool_input"`
	CWD     string         `json:"cwd"`
	Session string         `json:"session_id"`
	Agent   string         `json:"agent_id,omitempty"`
}

func shell(cwd, command string) call {
	return call{Tool: bashTool, Input: map[string]any{commandKey: command}, CWD: cwd, Session: "s1"}
}

func write(tool, key, path string) call {
	return call{Tool: tool, Input: map[string]any{key: path}, CWD: "/", Session: "s1"}
}

func edit(path string) call { return write(editTool, filePathKey, path) }

// contextOf is the additional context of the hook's answer, "" when none.
func contextOf(t *testing.T, h Hook, ev call) (string, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(ev)
	out := h.Decide(raw)
	if out == nil {
		return "", nil
	}
	var o struct {
		D map[string]any `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &o); err != nil {
		t.Fatalf("hook output %q: %v", out, err)
	}
	ctx, _ := o.D["additionalContext"].(string)
	return ctx, o.D
}

func TestReadsOnFirstWriteOnly(t *testing.T) {
	root := t.TempDir()
	project := repoWith(t, filepath.Join(root, "project"), map[string]string{claudeMD: "project rules"})
	other := repoWith(t, filepath.Join(root, "other"), map[string]string{
		"AGENTS.md":                   "Use conventional commits.\nRead `docs/style.md` first.\nSee @docs/imported.md\nMail me@example.com, ping @giantswarm/team.",
		"docs/style.md":               "style guide",
		"docs/imported.md":            "imported text",
		".claude/rules/go.md":         "go rule",
		".claude/rules/sub/deep.md":   "deep rule",
		".claude/settings.json":       `{"env":{"TOKEN":"do-not-show"},"hooks":{"PreToolUse":[]},"permissions":{"allow":["Bash(make:*)"]}}`,
		".claude/settings.local.json": `{"permissions":{"allow":["local"]}}`,
		"CLAUDE.local.md":             "personal",
	})
	h := readsHook(t, project)

	ctx, d := contextOf(t, h, edit(filepath.Join(other, "pkg", "new", "file.go")))
	for _, want := range []string{"=== AGENTS.md ===", "conventional commits", "=== docs/style.md ===", "style guide",
		"=== docs/imported.md ===", "=== .claude/rules/go.md ===", "=== .claude/rules/sub/deep.md ===", "Bash(make:*)", "do not run in your session"} {
		if !strings.Contains(ctx, want) {
			t.Errorf("first write: context lacks %q:\n%s", want, ctx)
		}
	}
	for _, not := range []string{"do-not-show", "personal", "local\"", "project rules"} {
		if strings.Contains(ctx, not) {
			t.Errorf("first write: context has %q:\n%s", not, ctx)
		}
	}
	if _, ok := d["permissionDecision"]; ok {
		t.Errorf("reads must not decide the permission: %+v", d)
	}
	if ctx, _ := contextOf(t, h, edit(filepath.Join(other, "README.md"))); ctx != "" {
		t.Errorf("second write: want nothing, got %q", ctx)
	}
	// Another session, and a subagent of the first, get it again.
	ev := edit(filepath.Join(other, "README.md"))
	ev.Session = "s2"
	if ctx, _ := contextOf(t, h, ev); ctx == "" {
		t.Error("another session: want the reads")
	}
	ev = edit(filepath.Join(other, "README.md"))
	ev.Agent = "a1"
	if ctx, _ := contextOf(t, h, ev); ctx == "" {
		t.Error("a subagent: want the reads")
	}
	if ctx, _ := contextOf(t, h, edit(filepath.Join(project, "x.md"))); ctx != "" {
		t.Errorf("own project: want nothing, got %q", ctx)
	}
	if ctx, _ := contextOf(t, h, edit(filepath.Join(root, "loose.txt"))); ctx != "" {
		t.Errorf("outside a repository: want nothing, got %q", ctx)
	}
}

func TestReadsTools(t *testing.T) {
	root := t.TempDir()
	project := repoWith(t, filepath.Join(root, "project"), nil)
	other := repoWith(t, filepath.Join(root, "other"), map[string]string{claudeMD: "other rules"})
	relative := write(writeTool, filePathKey, "sub/a")
	relative.CWD = other
	for name, ev := range map[string]call{
		"write":    write(writeTool, filePathKey, filepath.Join(other, "a")),
		"relative": relative,
		"notebook": write(notebookTool, notebookPathKey, filepath.Join(other, "n.ipynb")),
		"cd":       shell(project, "cd "+other+" && git add -A && git commit -m 'x'"),
		"dash-C":   shell(project, "git -C "+other+" commit -m x"),
		"in cwd":   shell(other, "git commit --amend --no-edit"),
	} {
		if ctx, _ := contextOf(t, readsHook(t, project), ev); !strings.Contains(ctx, "other rules") {
			t.Errorf("%s: want the reads, got %q", name, ctx)
		}
	}
	noSession := edit(filepath.Join(other, "a"))
	noSession.Session = ""
	for name, ev := range map[string]call{
		"read":       write("Read", filePathKey, filepath.Join(other, "a")),
		"git status": shell(project, "git -C "+other+" status"),
		"echo":       shell(other, "echo git commit"),
		"no session": noSession,
	} {
		if ctx, _ := contextOf(t, readsHook(t, project), ev); ctx != "" {
			t.Errorf("%s: want nothing, got %q", name, ctx)
		}
	}
}

func TestReadsBesideARewriteAndNotOnADeny(t *testing.T) {
	root := t.TempDir()
	other := repoWith(t, filepath.Join(root, "other"), map[string]string{claudeMD: "other rules"})
	h := readsHook(t, filepath.Join(root, "project"))
	ctx, d := contextOf(t, h, shell(other, "make test && git commit -m x"))
	if !strings.Contains(ctx, "other rules") || d["permissionDecision"] != decisionAllow {
		t.Fatalf("want the rewrite and the reads, got %+v", d)
	}
	if u, _ := d["updatedInput"].(map[string]any); !strings.Contains(u[commandKey].(string), " run -- zsh -c ") {
		t.Errorf("rewrite lost: %+v", u)
	}

	h = readsHook(t, filepath.Join(root, "project"))
	deny := shell(other, "kubectl get secret x -o yaml; git commit -m x")
	if ctx, d := contextOf(t, h, deny); ctx != "" || d["permissionDecision"] != decisionDeny {
		t.Fatalf("a refused call: want the deny alone, got %+v", d)
	}
	if ctx, _ := contextOf(t, h, edit(filepath.Join(other, "a"))); ctx == "" {
		t.Error("a refused call must not count as the first write")
	}
}

func TestReadsBounded(t *testing.T) {
	root := t.TempDir()
	big := strings.Repeat("line of the big file\n", 1000)
	other := repoWith(t, filepath.Join(root, "other"), map[string]string{
		claudeMD:              big,
		".claude/rules/a.md":  "rule a",
		".claude/rules/b.md":  "rule b",
		"docs/secret-plan.md": "hidden",
		"AGENTS.md":           "You must read `docs/secret-plan.md` and [the link](../outside.md).",
	})
	if err := os.WriteFile(filepath.Join(root, "outside.md"), []byte("outside the repository"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "outside.md"), filepath.Join(other, ".claude", "rules", "c.md")); err != nil {
		t.Fatal(err)
	}
	got := MandatoryReads(other, ReadsBudget)
	if len(got) > ReadsBudget {
		t.Errorf("reads are %d bytes, over the %d budget", len(got), ReadsBudget)
	}
	for _, want := range []string{"[truncated at ", "of 21000 bytes: read " + filepath.Join(other, claudeMD), "Left out for size, read them yourself: AGENTS.md, .claude/rules/a.md, .claude/rules/b.md"} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q in:\n%s", want, got[len(got)-400:])
		}
	}
	for _, not := range []string{"hidden", "outside the repository"} {
		if strings.Contains(MandatoryReads(other, 1<<20), not) {
			t.Errorf("%q must never be read", not)
		}
	}
	if got := MandatoryReads(repoWith(t, filepath.Join(root, "bare"), map[string]string{"README.md": "x"}), ReadsBudget); got != "" {
		t.Errorf("a repository without instructions: want nothing, got %q", got)
	}
}

func TestRepoRootWorktree(t *testing.T) {
	root := t.TempDir()
	wt := filepath.Join(root, "wt")
	if err := os.MkdirAll(filepath.Join(wt, "a", "b"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(wt)
	if got := RepoRoot(filepath.Join(wt, "a", "b", "missing.go")); got != real {
		t.Errorf("RepoRoot = %q, want %q", got, real)
	}
	if got := RepoRoot(filepath.Join(root, "x")); got != "" {
		t.Errorf("outside: RepoRoot = %q", got)
	}
}

func TestReadsMarker(t *testing.T) {
	m := ReadsMarker{Dir: t.TempDir()}
	if !m.First("s", "/r") || m.First("s", "/r") || !m.First("s", "/q") || !m.First("t", "/r") {
		t.Error("want first once per session and repository")
	}
	for _, bad := range []string{"", "..", "a/b"} {
		if m.First(bad, "/r") {
			t.Errorf("session %q: want refused", bad)
		}
	}
}
