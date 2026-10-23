package guard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// personDesk sets up a home whose projects folder holds a main checkout
// (a tracked file, an ignored one and the person's untracked draft), an
// organization folder and a linked worktree of the checkout outside the
// folders, and returns the home, the checkout, the worktree and the hook
// guarding the folders.
func personDesk(t *testing.T) (home, checkout, worktree string, h Hook) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	projects := filepath.Join(home, "projects")
	checkout = filepath.Join(projects, "lab")
	worktree = filepath.Join(home, ".local", "state", "worktrees", "lab", "change")
	for _, d := range []string{checkout, filepath.Join(projects, "giantswarm"), filepath.Dir(worktree)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{"tracked.md": "t\n", ".gitignore": "local.md\n", "i1.md": "the person's draft\n", "local.md": "l\n"} {
		if err := os.WriteFile(filepath.Join(checkout, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, ".zshrc"), []byte("# rc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...) //nolint:gosec // the test's own git commands
		c.Dir = checkout
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("add", "tracked.md", ".gitignore")
	git("commit", "-q", "-m", "init")
	git("worktree", "add", "-q", worktree)
	h = hook()
	h.PersonFolders = []string{home, projects, filepath.Join(projects, "*")}
	return home, checkout, worktree, h
}

func denied(d *decision) bool { return d != nil && d.PermissionDecision == decisionDeny }

func TestFolderGuardBash(t *testing.T) {
	home, checkout, worktree, h := personDesk(t)
	t.Setenv("BK_EMPTY", "")
	scratch := filepath.Join(home, ".local", "state", "notes")
	for _, cmd := range []string{
		"cat > body.md <<'EOF'\n## Problem\nEOF",
		"echo x >| i1.md",
		"echo x >> i1.md",
		"echo x &> out.log",
		`cd "$BK_EMPTY" && cat >| b.md <<'EOF'` + "\nx\nEOF",
		`printf x > "${BK_EMPTY}m.md"`,
		"echo x | tee -a notes.md",
		"zsh -c 'echo x > m.md'",
		"D=" + checkout + "; echo x > \"$D/c.md\"",
		"cd /tmp && cd " + checkout + " && echo x > c.md",
		"echo x > " + home + "/draft.md",
		"echo x > ~/projects/draft.md",
		"echo x > ~/projects/giantswarm/draft.md",
		"echo x > $HOME/new.md",
		// a worker's draft with TMPDIR empty: cd "" succeeds, the fallback never runs
		`cd "$BK_EMPTY" 2>/dev/null || cd /tmp/s` + "\ncat >| m.md <<'EOF'\n## Problem\nEOF",
		`cd "$BK_EMPTY" 2>/dev/null || cd /tmp/s` + "\nS=$PWD\ncat >| $S/i1.md <<'EOF'\nx\nEOF",
		`P="$HOME/projects/lab"; cd "$P" && echo x > draft.md`,
		`export D=` + checkout + `; echo x >| "$D/draft.md"`,
	} {
		d := decide(t, h, checkout, cmd, nil)
		if !denied(d) {
			t.Errorf("%q passes, want refused", cmd)
			continue
		}
		if !strings.Contains(d.Reason, "scratchpad") || !strings.Contains(d.Reason, `"${dir:?}"`) {
			t.Errorf("%q: the refusal names no scratchpad or guarded cd:\n%s", cmd, d.Reason)
		}
	}
	for _, cmd := range []string{
		"echo x > tracked.md",
		"echo x > local.md",
		"echo x > /dev/null 2>&1",
		"ls 2>&1 | head",
		`cd "${dir:?}" && echo x > b.md`,
		"cd $(mktemp -d) && echo x > b.md",
		"cd - && echo x > b.md",
		"mkdir -p " + scratch + " && echo x >| " + scratch + "/body.md",
		"echo x >> ~/.zshrc",
		"cd " + worktree + " && echo x > new.md",
		"echo x > .git/info/exclude",
		"[[ a > b ]] && echo yes",
		"git commit -m 'a > b.md'",
		"echo 'x > y.md'",
		// variables the command line sets: known, or unknown, never empty
		`W="$HOME/.local/state/worktrees/lab/change"; cd "$W" && cat >| new.md <<'EOF'` + "\nx\nEOF",
		`d=$(mktemp -d); cd "$d"` + "\ncat >| 1.md <<'E'\nx\nE",
		`sp=/tmp/s; g="${sp:?}/gen"; mkdir -p "$g"; cd "$g" && echo x > tag.yml`,
		`s=/tmp/s/pr.md; sed -e s/a/b/ "$s" >| "${s%.md}-1.md"`,
		`L=/tmp/s/make.log; sh -c "make test > '$L' 2>&1"`,
		`for f in a b; do echo x > "$f"; done`,
		// a dot-directory of a checkout is configuration
		"echo x > .config/tool.yaml",
		`cd "${dir:?}" 2>/dev/null || cd /tmp/s && echo x > b.md`,
		"cd /tmp/s && echo x > $PWD/b.md",
	} {
		if d := decide(t, h, checkout, cmd, nil); denied(d) {
			t.Errorf("%q refused, want it to pass:\n%s", cmd, d.Reason)
		}
	}
	if d := decide(t, h, worktree, "echo x > new.md", nil); denied(d) {
		t.Errorf("a new file in a linked worktree refused:\n%s", d.Reason)
	}
}

func TestFolderGuardFileTools(t *testing.T) {
	home, checkout, worktree, h := personDesk(t)
	for _, tc := range []struct {
		tool, file string
		refuse     bool
	}{
		{writeTool, filepath.Join(checkout, "body.md"), true},
		{writeTool, filepath.Join(checkout, "i1.md"), true},
		{editTool, filepath.Join(checkout, "i1.md"), true},
		{writeTool, filepath.Join(checkout, "docs", "new.md"), true},
		{writeTool, filepath.Join(home, "projects", "b.md"), true},
		{writeTool, filepath.Join(home, "notes.md"), true},
		{writeTool, filepath.Join(home, ".newrc"), true},
		{editTool, filepath.Join(checkout, "tracked.md"), false},
		{writeTool, filepath.Join(checkout, "local.md"), false},
		{writeTool, filepath.Join(worktree, "body.md"), false},
		{editTool, filepath.Join(home, ".zshrc"), false},
		{writeTool, filepath.Join(home, ".local", "state", "beekeeper", "x.md"), false},
	} {
		ev := toolEvent(tc.tool, map[string]any{filePathKey: tc.file})
		ev["cwd"] = checkout
		if got := denied(decideEvent(t, h, ev)); got != tc.refuse {
			t.Errorf("%s %s: refused %v, want %v", tc.tool, tc.file, got, tc.refuse)
		}
	}
	h.PersonFolders = nil
	ev := toolEvent(writeTool, map[string]any{filePathKey: filepath.Join(checkout, "body.md")})
	if denied(decideEvent(t, h, ev)) {
		t.Error("refused without hooks.personFolders")
	}
}

// The refusal names the session's scratchpad by absolute path: the one
// Claude Code made, else the one it makes for the project.
func TestFolderGuardNamesTheScratchpad(t *testing.T) {
	_, checkout, _, h := personDesk(t)
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)
	h.Project = "/home/u/projects/klaus-lab"
	ev := toolEvent(writeTool, map[string]any{filePathKey: filepath.Join(checkout, "body.md")})
	ev["cwd"], ev["session_id"] = checkout, "8bf5cba8-50ec-4608-b1bc-a266c56e1e6a"
	want := filepath.Join(tmp, "claude-"+strconv.Itoa(os.Getuid()), "-home-u-projects-klaus-lab", "8bf5cba8-50ec-4608-b1bc-a266c56e1e6a", "scratchpad")
	if d := decideEvent(t, h, ev); !denied(d) || !strings.Contains(d.Reason, want) {
		t.Fatalf("the refusal does not name %s:\n%v", want, d)
	}
	made := filepath.Join(tmp, "claude-"+strconv.Itoa(os.Getuid()), "-elsewhere", "8bf5cba8-50ec-4608-b1bc-a266c56e1e6a", "scratchpad")
	if err := os.MkdirAll(made, 0o700); err != nil {
		t.Fatal(err)
	}
	if d := decideEvent(t, h, ev); !denied(d) || !strings.Contains(d.Reason, made) {
		t.Fatalf("the refusal does not name the scratchpad made, %s:\n%v", made, d)
	}
}
