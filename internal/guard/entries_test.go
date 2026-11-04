package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProtectedEntries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", "")
	projects := filepath.Join(home, "projects")
	clone := filepath.Join(projects, "org", "foo")
	for _, d := range []string{filepath.Join(clone, ".git")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{".zshrc", "repos.txt"} {
		if err := os.WriteFile(filepath.Join(home, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h := hook()
	h.ProjectsDir = projects
	h.WorktreePlaces = []string{filepath.Join(projects, "worktrees")}
	h.HomePlaces = []string{filepath.Join(home, ".local", "state")}
	for _, cmd := range []string{
		"git worktree add ~/projects/foo-wt-1",
		"cd ~/projects/org/foo && git worktree add ../foo-wt-1",
		"git -C ~/projects/foo worktree add $HOME/projects/foo-2 -b x",
		"cd ~/projects && git clone git@github.com:org/foo.git",
		"cd ~/projects && gh repo clone org/foo",
		"git clone https://example.invalid/org/foo.git ~/projects/org/foo-copy",
		"cp -r ~/projects/foo ~/projects/foo-copy",
		"mkdir ~/projects/org/foo-wt-2",
		"cd ~ && devctl pr wait o/r 1 2>wait.err",
		"cd && cat >| b1.md <<EOF\nx\nEOF",
		"cd $TMPDIR; cat >| draft.md",
		"cp ~/repos.txt ~/repos.txt.bak",
		"mkdir ~/foo-lab",
		"echo x > ~/projects/org/body.md",
		"touch ~/result.md",
		"mv ~/repos.txt ~/new.txt",
		"echo x | tee ~/new.txt",
		"echo x &> ~/new.txt",
		"cp ~/projects/org/foo/README.md ~",
		"cp -t ~ ~/projects/foo/README.md",
		"git -C ~/projects/org/foo -C .. worktree add new",
		"cd ~/projects && git clone --depth 1 --branch main https://example.invalid/org/new.git",
		"cd ~/projects && gh repo clone org/new -- --depth 1",
	} {
		t.Run(cmd, func(t *testing.T) {
			d := decide(t, h, clone, cmd, nil)
			if !denied(d) {
				t.Fatal("expected refusal")
			}
			for _, text := range []string{h.HomePlaces[0], h.WorktreePlaces[0], "scratchpad", "<repo>/<branch>", "git -C <clone>", "go-ahead"} {
				if !strings.Contains(d.Reason, text) {
					t.Errorf("refusal missing %q: %s", text, d.Reason)
				}
			}
		})
	}
	for _, cmd := range []string{
		"git worktree add ~/projects/worktrees/foo/x",
		"git clone https://example.invalid/org/foo.git ~/.local/state/scratch/foo",
		"mkdir -p ~/projects/worktrees/foo",
		"mkdir -p ~/projects/worktrees",
		"git worktree add /tmp/foo",
		"git worktree list",
		"git worktree prune",
		"echo x >> ~/.zshrc",
		"mkdir -p ~/.local/state/foo",
		"cp ~/repos.txt ~/repos.txt",
		"touch ~/projects/org/foo/new.md",
		"cd \"${dir:?}\" && touch new.md",
	} {
		t.Run(cmd, func(t *testing.T) {
			if d := decide(t, h, clone, cmd, nil); denied(d) {
				t.Fatalf("unexpected refusal: %s", d.Reason)
			}
		})
	}
	for _, tc := range []struct {
		tool, file string
		refuse     bool
	}{
		{writeTool, "~/result.md", true},
		{notebookTool, "~/result.ipynb", true},
		{writeTool, "~/projects/org/foo/README.md", false},
		{writeTool, "~/repos.txt", false},
		{writeTool, "~/.local/state/new.md", false},
	} {
		ev := toolEvent(tc.tool, map[string]any{filePathKey: tc.file})
		ev["cwd"] = clone
		if got := denied(decideEvent(t, h, ev)); got != tc.refuse {
			t.Errorf("%s %s: refusal %v, want %v", tc.tool, tc.file, got, tc.refuse)
		}
	}
}

func TestProtectedEntriesCustomPlaces(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	h := hook()
	h.ProjectsDir = filepath.Join(home, "clones")
	h.WorktreePlaces = []string{filepath.Join(home, "sessions")}
	h.HomePlaces = []string{filepath.Join(home, "state")}
	if d := decide(t, h, home, "mkdir -p ~/sessions", nil); denied(d) {
		t.Fatalf("place itself refused: %s", d.Reason)
	}
	if d := decide(t, h, home, "mkdir ~/new", nil); !denied(d) || !strings.Contains(d.Reason, h.HomePlaces[0]) {
		t.Fatalf("missing configured places: %v", d)
	}
}
