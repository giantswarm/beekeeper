package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const worktreeVisitor = "visitor"

func TestWorktreeOwnerCommands(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		cmd, want string
		create    bool
	}{
		{"git commit -am change", dir, false},
		{"git push origin feature", dir, false},
		{"git -C " + dir + " commit -m change", dir, false},
		{"cd " + dir + " && git push origin feature", dir, false},
		{"sh -c 'git commit -am change'", dir, false},
		{"git worktree add -b feature ../new origin/main", filepath.Join(filepath.Dir(dir), "new"), true},
		{"git worktree add --detach -- ../new origin/main", filepath.Join(filepath.Dir(dir), "new"), true},
		{"git worktree add --orphan ../new", filepath.Join(filepath.Dir(dir), "new"), true},
	} {
		t.Run(tc.cmd, func(t *testing.T) {
			calls := 0
			h := Hook{mergeSession: worktreeVisitor, WorktreeOwner: func(session, path string, create bool) string {
				calls++
				if session != worktreeVisitor || path != tc.want || create != tc.create {
					t.Fatalf("got %q %q %v", session, path, create)
				}
				return "Refused: owner-session"
			}}
			if r := h.pushRefusal(tc.cmd, dir); !strings.Contains(r, "owner-session") || calls != 1 {
				t.Fatalf("result %q calls %d", r, calls)
			}
		})
	}
	h := Hook{mergeSession: worktreeVisitor, WorktreeOwner: func(string, string, bool) string { t.Fatal("read-only command checked"); return "" }}
	if r := h.pushRefusal("git status", dir); r != "" {
		t.Fatal(r)
	}
}

func TestWorktreeOwnerHook(t *testing.T) {
	h := Hook{WorktreeOwner: func(session, dir string, create bool) string {
		if session != worktreeVisitor {
			t.Fatalf("session %q", session)
		}
		return "Refused: owner-session"
	}}
	raw, err := json.Marshal(map[string]any{sessionKey: worktreeVisitor, ompWorkingDir: t.TempDir(), executionToolName: bashTool, executionToolInput: map[string]string{commandKey: "git commit -am change"}})
	if err != nil {
		t.Fatal(err)
	}
	if out := string(h.Decide(raw)); !strings.Contains(out, "owner-session") || !strings.Contains(out, "deny") {
		t.Fatal(out)
	}
}

// Exercise both guards with real Git discovery, including a beekeeper-named
// path and a launch directory different from the worker's repository.
func TestOwnedWorktreeCompoundPush(t *testing.T) {
	launch := t.TempDir()
	repo := initGitRepo(t, "feature", "https://github.com/giantswarm/beekeeper.git")
	link := filepath.Join(t.TempDir(), "beekeeper")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		"cd " + link + " && git push origin feature",
		"cd \"" + link + "\" && git push origin feature",
		"dir=" + link + "; cd \"${dir:?}\" && git push origin feature",
		"dir=" + link + "; cd \"${dir:?worktree required}\" && git push origin feature",
		"git -C " + link + " push origin feature",
	} {
		t.Run(command, func(t *testing.T) {
			calls := 0
			h := Hook{mergeSession: worktreeVisitor, Upstream: Upstream{Orgs: []string{allowedOrg}}, WorktreeOwner: func(session, dir string, create bool) string {
				calls++
				real, err := filepath.EvalSymlinks(dir)
				if err != nil || real != repo || session != worktreeVisitor || create {
					t.Fatalf("owner check: %q %q %v, %v", session, dir, create, err)
				}
				return ""
			}}
			if w := h.Upstream.bashWrite(command, launch); w != nil {
				t.Fatalf("upstream refused: %+v", w)
			}
			if r := h.pushRefusal(command, launch); r != "" || calls != 1 {
				t.Fatalf("push: %q, owner calls: %d", r, calls)
			}
			h.WorktreeOwner = func(string, string, bool) string { return "Refused: another owner" }
			if r := h.pushRefusal(command, launch); !strings.Contains(r, "another owner") {
				t.Fatalf("foreign worktree: %q", r)
			}
		})
	}
}

func TestGuardedDirectoryExpansion(t *testing.T) {
	for _, tc := range []struct {
		word, value string
		bound, want bool
	}{
		{"${dir:?}", "/worktree", true, true},
		{"${dir?required}", "/worktree", true, true},
		{"${dir:?required}", "", true, false},
		{"${dir:?required}", "", false, false},
		{"${dir:-/fallback}", "", false, false},
	} {
		vars := map[string]string{}
		if tc.bound {
			vars["dir"] = tc.value
		}
		got, ok := expandDirWord(tc.word, vars)
		if ok != tc.want || (ok && got != tc.value) {
			t.Errorf("%q: got %q, %v", tc.word, got, ok)
		}
	}
}

func TestCompoundPushStillRefusesUnknownAndExternalDirectories(t *testing.T) {
	repo := initGitRepo(t, "feature", "https://github.com/outside/repository.git")
	h := Hook{mergeSession: worktreeVisitor, Upstream: Upstream{Orgs: []string{allowedOrg}}, WorktreeOwner: func(string, string, bool) string { return "" }}
	for _, command := range []string{
		"cd " + repo + " && git push origin feature",
		"dir=" + repo + "; cd \"${dir:?}\" && git push origin feature",
		"cd \"${UNBOUND_WORKTREE:?}\" && git push origin feature",
		"dir=; cd \"${dir:?}\" && git push origin feature",
	} {
		if w := h.Upstream.bashWrite(command, t.TempDir()); w == nil {
			t.Errorf("upstream allowed %q", command)
		}
	}
	for _, command := range []string{
		"cd \"${UNBOUND_WORKTREE:?}\" && git push origin feature",
		"dir=; cd \"${dir:?}\" && git push origin feature",
	} {
		if r := h.pushRefusal(command, t.TempDir()); r == "" {
			t.Errorf("owner allowed %q", command)
		}
	}
}
