package guard

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	testBeekeeperExe = "/opt/bin/beekeeper"
	allowedOrg       = "giantswarm"
	cmdLineKey       = "CommandLine"
	pushOriginHEAD   = "git push origin HEAD"
	plainGitPush     = "git push"
	andOp            = " && "
	orOp             = " || "
	helper           = "unlock-helper"
)

type agyDecision struct {
	Decision  string         `json:"decision"`
	Reason    string         `json:"reason"`
	Overwrite map[string]any `json:"overwrite"`
}

func decideAgyCall(t *testing.T, h Hook, cwd, name string, args map[string]any) agyDecision {
	t.Helper()
	var workspaces []string
	if cwd != "" {
		workspaces = []string{cwd}
	}
	raw, err := json.Marshal(map[string]any{
		"conversationId": "conv-test",
		"workspacePaths": workspaces,
		"toolCall": map[string]any{
			"name": name,
			"args": args,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := h.DecideAgy(raw)
	var d agyDecision
	if err := json.Unmarshal(out, &d); err != nil {
		t.Fatalf("DecideAgy returned invalid JSON %q: %v", string(out), err)
	}
	if d.Decision == "" {
		t.Fatalf("DecideAgy returned empty decision: %s", string(out))
	}
	return d
}

func decideAgyCmd(t *testing.T, h Hook, cwd, cmd string) agyDecision {
	t.Helper()
	return decideAgyCall(t, h, cwd, "run_command", map[string]any{cmdLineKey: cmd})
}

func initGitRepo(t *testing.T, branch, remoteURL string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...) //nolint:gosec // test helper
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init", "-b", branch)
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	if remoteURL != "" {
		run("remote", "add", "origin", remoteURL)
	}
	return dir
}

// DecideAgy answers in agy's PreToolUse contract (`decision`: allow|deny|ask,
// `reason`, `overwrite.CommandLine`) across run_command, file-write and read
// tools, MCP calls, secret checks, allowed-org checks, and merge/build rewrites.
func TestAgyHookFormatAndChecks(t *testing.T) {
	mainRepo := initGitRepo(t, "main", "https://github.com/giantswarm/beekeeper.git")
	featRepo := initGitRepo(t, "feat-x", "https://github.com/giantswarm/beekeeper.git")
	otherRepo := initGitRepo(t, "feat-x", "https://github.com/other-org/project.git")

	h := Hook{
		Self:           testBeekeeperExe,
		Shell:          zshCmd,
		UnlockCommands: []string{helper},
		SecretFiles:    []string{filepath.Join(featRepo, "*.secret.yaml")},
		Upstream:       Upstream{Orgs: []string{allowedOrg}},
		Outbound:       Outbound{Phrases: []string{"super-secret-phrase"}, Paths: []string{featRepo}},
	}

	// Malformed JSON and run_command without string CommandLine fail closed.
	var bad agyDecision
	if err := json.Unmarshal(h.DecideAgy([]byte("{not json")), &bad); err != nil || bad.Decision != decisionDeny {
		t.Errorf("malformed JSON: got %+v, err %v", bad, err)
	}
	if d := decideAgyCall(t, h, featRepo, "run_command", map[string]any{cmdLineKey: 123}); d.Decision != decisionDeny {
		t.Errorf("non-string CommandLine: got %+v", d)
	}

	// Ordinary commands and unknown tools answer {"decision":"ask"}.
	if d := decideAgyCmd(t, h, featRepo, "git status"); d.Decision != decisionAsk {
		t.Errorf("git status: got %+v, want ask", d)
	}
	if d := decideAgyCall(t, h, featRepo, "unknown_tool", map[string]any{"x": "y"}); d.Decision != decisionAsk {
		t.Errorf("unknown_tool: got %+v, want ask", d)
	}

	// Default-branch pushes and force-pushes are denied.
	for _, cmd := range []string{
		"git push origin main",
		"git push origin master",
		"git push origin HEAD:main",
		"git push origin feat-x:refs/heads/main",
		"git push origin feat-x:refs/heads/master",
		"git push origin :main",
		"git push origin --delete main",
		"git push -d origin master",
		"git push --all origin",
		"git push --branches origin",
		"git push --mirror origin",
		"git push -f origin feat-x",
		"git push --force origin feat-x",
		"git push --force-with-lease origin feat-x",
		"git push --force-with-lease=feat-x:abc origin feat-x",
		"git push --force-if-includes origin feat-x",
		"git push -uf origin feat-x",
		"git push origin +feat-x",
		"git push origin +HEAD:feat-x",
	} {
		if d := decideAgyCmd(t, h, featRepo, cmd); d.Decision != decisionDeny {
			t.Errorf("%q: got %+v, want deny", cmd, d)
		}
	}

	// Pushing a feature branch in an allowed org passes with ask.
	for _, cmd := range []string{
		"git push origin feat-x",
		"git push -u origin feat-x",
		pushOriginHEAD,
		plainGitPush,
	} {
		if d := decideAgyCmd(t, h, featRepo, cmd); d.Decision != decisionAsk {
			t.Errorf("%q in featRepo: got %+v, want ask", cmd, d)
		}
	}
	// Bare/HEAD push on main is denied.
	for _, cmd := range []string{
		plainGitPush,
		"git push origin",
		pushOriginHEAD,
		"git push origin @",
		"git push origin $(git branch --show-current)",
		"git push origin $(git rev-parse --abbrev-ref HEAD)",
	} {
		if d := decideAgyCmd(t, h, mainRepo, cmd); d.Decision != decisionDeny {
			t.Errorf("%q in mainRepo: got %+v, want deny", cmd, d)
		}
	}

	// Writes outside github.allowedOrgs are denied; inside allowedOrgs pass.
	for _, cmd := range []string{
		"gh issue create -R other-org/project --title t --body b",
		"gh pr create --repo other-org/project --title t --body b",
		"gh pr merge 12 -R other-org/project --squash",
	} {
		if d := decideAgyCmd(t, h, featRepo, cmd); d.Decision != decisionDeny || !strings.Contains(d.Reason, "other-org/project") {
			t.Errorf("disallowed org %q: got %+v", cmd, d)
		}
	}
	if d := decideAgyCmd(t, h, otherRepo, "git push origin feat-x"); d.Decision != decisionDeny || !strings.Contains(d.Reason, "other-org/project") {
		t.Errorf("git push in disallowed org repo: got %+v", d)
	}
	if d := decideAgyCmd(t, h, featRepo, "gh issue create -R giantswarm/beekeeper --title t --body b"); d.Decision != decisionAsk {
		t.Errorf("allowed org gh issue create: got %+v, want ask", d)
	}

	// Merge outside the gate is rewritten to run through beekeeper gate, or
	// refused when hidden inside sh -c.
	if d := decideAgyCmd(t, h, featRepo, "devctl pr merge giantswarm/beekeeper 7"); d.Decision != decisionAllow ||
		!strings.Contains(d.Overwrite[cmdLineKey].(string), testBeekeeperExe+" gate -- devctl pr merge giantswarm/beekeeper 7") {
		t.Errorf("devctl pr merge rewrite: got %+v", d)
	}
	if d := decideAgyCmd(t, h, featRepo, "sh -c 'devctl pr merge giantswarm/beekeeper 7'"); d.Decision != decisionDeny {
		t.Errorf("hidden devctl pr merge: got %+v, want deny", d)
	}

	// Build commands are rewritten to run through beekeeper run, with
	// BEEKEEPER_BACKGROUND=1 when IsDaemon is true.
	if d := decideAgyCall(t, h, featRepo, "run_command", map[string]any{
		cmdLineKey: "go test ./cmd/...",
		"IsDaemon": true,
	}); d.Decision != decisionAllow ||
		!strings.HasPrefix(d.Overwrite[cmdLineKey].(string), "export "+BackgroundEnv+"=1; "+testBeekeeperExe+" run -- ") {
		t.Errorf("daemon build rewrite: got %+v", d)
	}

	// Secret leaks and unlock commands are denied.
	for _, cmd := range []string{
		"kubectl get secret app-secret -o yaml",
		"sops -d secrets.enc.yaml",
		"op read op://vault/item/password",
		helper,
	} {
		if d := decideAgyCmd(t, h, featRepo, cmd); d.Decision != decisionDeny {
			t.Errorf("secret command %q: got %+v, want deny", cmd, d)
		}
	}

	// File-read and file-write tools in agy's format are checked too.
	if d := decideAgyCall(t, h, featRepo, "view_file", map[string]any{
		"AbsolutePath": filepath.Join(featRepo, "prod.secret.yaml"),
	}); d.Decision != decisionDeny {
		t.Errorf("view_file on secret file: got %+v, want deny", d)
	}
	if d := decideAgyCall(t, h, featRepo, "view_file", map[string]any{
		"AbsolutePath": filepath.Join(featRepo, "README.md"),
	}); d.Decision != decisionAsk {
		t.Errorf("view_file on README.md: got %+v, want ask", d)
	}
	if d := decideAgyCall(t, h, featRepo, "write_to_file", map[string]any{
		"TargetFile":  filepath.Join(featRepo, "notes.md"),
		"CodeContent": "contains super-secret-phrase here",
	}); d.Decision != decisionDeny {
		t.Errorf("write_to_file with secret phrase: got %+v, want deny", d)
	}
}

// Linked git worktrees placed outside guard.roots are in scope when their
// main repository is under guard.roots.
func TestUnderResolvesLinkedGitWorktree(t *testing.T) {
	root := t.TempDir()
	mainRepo := filepath.Join(root, "projects", "repo")
	if err := os.MkdirAll(mainRepo, 0o750); err != nil {
		t.Fatal(err)
	}
	c := exec.Command("git", "init", "-b", "main")
	c.Dir = mainRepo
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	c = exec.Command("git", "config", "user.email", "test@example.com")
	c.Dir = mainRepo
	_ = c.Run()
	c = exec.Command("git", "config", "user.name", "Test")
	c.Dir = mainRepo
	_ = c.Run()
	c = exec.Command("git", "commit", "--allow-empty", "-m", "init")
	c.Dir = mainRepo
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v (%s)", err, out)
	}
	wtDir := filepath.Join(t.TempDir(), "worktrees", "repo", "feat")
	c = exec.Command("git", "worktree", "add", "-b", "feat", wtDir) //nolint:gosec // test helper
	c.Dir = mainRepo
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v (%s)", err, out)
	}
	roots := []string{filepath.Join(root, "projects")}
	if !Under(roots, wtDir) {
		t.Errorf("Under(%v, %q) = false, want true for worktree of %q", roots, wtDir, mainRepo)
	}
}

// Comment 1 (https://github.com/giantswarm/beekeeper/issues/1683#issuecomment-6448816736):
// bare/HEAD git push and gh pr merge are judged by the directory the command
// runs in after cd/pushd/popd/cd - and git -C, and fail closed where the
// shell does not keep cd or the guard cannot follow it.
func TestAgyPushAndMergeDirectoryResolution(t *testing.T) {
	mainRepo := initGitRepo(t, "main", "https://github.com/giantswarm/beekeeper.git")
	featRepo := initGitRepo(t, "feat-branch", "https://github.com/giantswarm/beekeeper.git")

	h := Hook{
		Self:     testBeekeeperExe,
		Shell:    zshCmd,
		Upstream: Upstream{Orgs: []string{allowedOrg}},
	}

	// Launch on main, cd to featRepo -> allowed (`ask`).
	for _, cmd := range []string{
		"cd " + featRepo + " && git push",
		"cd " + featRepo + "; git push origin HEAD",
		"pushd " + featRepo + " && git push",
		"cd " + featRepo + " && cd " + mainRepo + " && cd - && git push",
		"pushd " + featRepo + " && pushd " + mainRepo + " && popd && git push",
		"HOME=" + featRepo + "; cd && git push",
		"HOME=" + featRepo + "; cd ~/ && git push",
		"D=" + featRepo + "; cd \"$D\" && git push",
		"git -C " + featRepo + " push",
		"git -C " + featRepo + " push origin HEAD",
	} {
		if d := decideAgyCmd(t, h, mainRepo, cmd); d.Decision != decisionAsk {
			t.Errorf("from mainRepo %q: got %+v, want ask", cmd, d)
		}
	}

	// Launch on featRepo, cd to mainRepo -> denied.
	for _, cmd := range []string{
		"cd " + mainRepo + " && git push",
		"cd " + mainRepo + "; git push origin HEAD",
		"pushd " + mainRepo + " && git push",
		"cd " + mainRepo + " && cd " + featRepo + " && cd - && git push",
		"pushd " + mainRepo + " && pushd " + featRepo + " && popd && git push",
		"D=" + mainRepo + "; cd $D && git push",
		"git -C " + mainRepo + " push",
		"cd " + featRepo + " && git -C " + mainRepo + " push",
	} {
		if d := decideAgyCmd(t, h, featRepo, cmd); d.Decision != decisionDeny {
			t.Errorf("from featRepo %q: got %+v, want deny", cmd, d)
		}
	}

	// Subshells, command substitutions, pipelines, background jobs, compound
	// commands, conditional chains, missing directories, and unset variables
	// either leave the directory unchanged (on mainRepo) or unknown -> denied.
	for _, cmd := range []string{
		"(cd " + featRepo + ") && git push",
		"echo $(cd " + featRepo + ") && git push",
		"echo `cd " + featRepo + "` && git push",
		"cd " + featRepo + " | true; git push",
		"true | cd " + featRepo + "; git push",
		"cd " + featRepo + " & wait; git push",
		"true && cd " + featRepo + " & wait; git push",
		"sh -c 'cd " + featRepo + "' && git push",
		"bash -c 'cd " + featRepo + "' && git push",
		"eval 'cd " + featRepo + "' && git push",
		"if true; then cd " + featRepo + "; fi; git push",
		"for d in " + featRepo + "; do cd \"$d\"; done; git push",
		"while true; do cd " + featRepo + "; break; done; git push",
		"until false; do cd " + featRepo + "; break; done; git push",
		"case x in *) cd " + featRepo + ";; esac; git push",
		"{ cd " + featRepo + "; }; git push",
		"! cd " + featRepo + "; git push",
		"coproc cd " + featRepo + "; git push",
		"f() { cd " + featRepo + "; }; f; git push",
		"false && cd " + featRepo + "; git push",
		"true || cd " + featRepo + " && git push",
		"cd /nonexistent-guard-test-dir && git push",
		"cd \"$UNSET_DIR_VAR\" && git push",
		"source ./env.sh && git push",
		". ./env.sh && git push",
		"CDPATH=/tmp && cd sub && git push",
		"OLDPWD=" + featRepo + " && cd - && git push",
	} {
		if d := decideAgyCmd(t, h, mainRepo, cmd); d.Decision != decisionDeny {
			t.Errorf("unkept/unknown cd %q: got %+v, want deny", cmd, d)
		}
	}

	// Even when launched in featRepo, an unfollowed cd makes the working
	// directory unknown, so a bare/HEAD push is refused unless the branch is
	// named explicitly.
	for _, cmd := range []string{
		"if true; then cd " + featRepo + "; fi; git push",
		"{ cd " + featRepo + "; }; git push origin HEAD",
		"false && cd " + featRepo + "; git push",
		"true || cd " + featRepo + " && git push",
		"cd \"$UNSET_DIR_VAR\"; git push",
	} {
		if d := decideAgyCmd(t, h, featRepo, cmd); d.Decision != decisionDeny {
			t.Errorf("unknown dir from featRepo %q: got %+v, want deny", cmd, d)
		}
	}
	if d := decideAgyCmd(t, h, featRepo, "if true; then cd "+featRepo+"; fi; git push origin feat-branch"); d.Decision != decisionAsk {
		t.Errorf("explicit branch after unfollowed cd: got %+v, want ask", d)
	}

	// gh pr merge <number> after an unfollowed cd is refused unless it names
	// the repository (-R/--repo, GH_REPO, or a URL).
	if d := decideAgyCmd(t, h, featRepo, "if true; then cd "+featRepo+"; fi; gh pr merge 42 --squash"); d.Decision != decisionDeny {
		t.Errorf("gh pr merge without repo after unfollowed cd: got %+v, want deny", d)
	}
	for _, cmd := range []string{
		"if true; then cd " + featRepo + "; fi; gh pr merge 42 -R giantswarm/beekeeper --squash",
		"if true; then cd " + featRepo + "; fi; gh pr merge 42 --repo=giantswarm/beekeeper --squash",
		"if true; then cd " + featRepo + "; fi; GH_REPO=giantswarm/beekeeper gh pr merge 42 --squash",
		"if true; then cd " + featRepo + "; fi; gh pr merge https://github.com/giantswarm/beekeeper/pull/42 --squash",
		"cd " + featRepo + " && gh pr merge 42 --squash",
	} {
		if d := decideAgyCmd(t, h, featRepo, cmd); d.Decision != decisionAsk {
			t.Errorf("gh pr merge with known repo %q: got %+v, want ask", cmd, d)
		}
	}
}

// Comment 1 (https://github.com/giantswarm/beekeeper/issues/1683#issuecomment-6448816736):
// bash comment boundaries, backslash-newlines, prefix wrappers, and fail-closed
// unmodeled wrappers.
func TestAgyBashCommentsContinuationsAndWrappers(t *testing.T) {
	featRepo := initGitRepo(t, "feat-branch", "https://github.com/giantswarm/beekeeper.git")
	h := Hook{
		Self:           testBeekeeperExe,
		Shell:          zshCmd,
		UnlockCommands: []string{helper},
		Upstream:       Upstream{Orgs: []string{allowedOrg}},
	}

	// '#' that does NOT start a comment must not hide a following command, and
	// '#\' at the end of a comment does not join the next line into the comment.
	for _, cmd := range []string{
		`git commit -m fix\ #35 && git push origin main`,
		`echo $(date)# && git push origin main`,
		"echo `echo #` && git push origin main",
		`echo "#" && git push origin main`,
		`echo '#' && git push origin main`,
		`echo a#b && git push origin main`,
		`echo $# ${#HOME} && git push origin main`,
		`echo $((1+2))# && git push origin main`,
		`x=(a b)#c && git push origin main`,
		`cat <(true)# && git push origin main`,
		`echo >(true)# && git push origin main`,
		"echo $( # comment inside subst\ntrue ) && git push origin main",
		"true #\\\ngit push origin main",
		"echo '\\\n' && git push origin main",
		"echo \"\\\n\" && git push origin main",
		"git \\\npush origin main",
		"git push \\\norigin main",
	} {
		if d := decideAgyCmd(t, h, featRepo, cmd); d.Decision != decisionDeny {
			t.Errorf("comment/continuation case %q: got %+v, want deny", cmd, d)
		}
	}

	// Single quotes never contain commands, and a real '#' comment at word
	// start is ignored.
	for _, cmd := range []string{
		`echo 'git push origin main'`,
		`echo 'unlock-helper'`,
		"true # git push origin main",
	} {
		if d := decideAgyCmd(t, h, featRepo, cmd); d.Decision != decisionAsk {
			t.Errorf("benign literal/comment %q: got %+v, want ask", cmd, d)
		}
	}

	// Modeled prefix commands and short-option clusters are unwrapped.
	for _, cmd := range []string{
		"env -u FOO git push origin main",
		"env -uFOO git push origin main",
		"env -iu FOO git push origin main",
		"env -C " + featRepo + " git push origin main",
		"env -S 'git push origin main'",
		"sudo -Eu root git push origin main",
		"sudo -u root -E git push origin main",
		"nice -n 5 git push origin main",
		"nice -5 git push origin main",
		"timeout -s KILL 9 git push origin main",
		"timeout -vs KILL 9 git push origin main",
		"stdbuf -oL git push origin main",
		"ionice -c3 git push origin main",
		"flock -w 5 /tmp/lock git push origin main",
		"flock /tmp/lock -c 'git push origin main'",
		"chrt -b 0 git push origin main",
		"command -v git && command git push origin main",
	} {
		if d := decideAgyCmd(t, h, featRepo, cmd); d.Decision != decisionDeny {
			t.Errorf("wrapped main push %q: got %+v, want deny", cmd, d)
		}
	}

	// The same modeled prefix commands around a valid feature push pass.
	for _, cmd := range []string{
		"env -u FOO git push origin feat-branch",
		"env -iu FOO git push origin feat-branch",
		"nice -n 5 git push origin feat-branch",
		"timeout -vs KILL 9 git push origin feat-branch",
		"stdbuf -oL git push origin feat-branch",
		"flock -w 5 /tmp/lock git push origin feat-branch",
	} {
		if d := decideAgyCmd(t, h, featRepo, cmd); d.Decision != decisionAsk {
			t.Errorf("wrapped feat push %q: got %+v, want ask", cmd, d)
		}
	}

	// Unmodeled or unreadable wrappers on a line naming a guarded tool, push
	// or merge fail closed.
	for _, cmd := range []string{
		"sudo -Ezu root git push origin feat-branch",
		"env --unknown-opt=1 git push origin feat-branch",
		"xargs git push origin feat-branch",
		"watch git push origin feat-branch",
		"su -c 'git push origin feat-branch'",
		"sudo -Ezu root unlock-helper",
	} {
		if d := decideAgyCmd(t, h, featRepo, cmd); d.Decision != decisionDeny {
			t.Errorf("unmodeled wrapper %q: got %+v, want deny", cmd, d)
		}
	}
}

// Comment 2 (https://github.com/giantswarm/beekeeper/issues/1683#issuecomment-6449405917):
// ordinary reads of paths containing a guarded tool's name pass, while runs via
// stdin, here-strings, here-docs, pipes into shells, copies/links made on the
// same line, git aliases, variables, and loops are refused, and a preexisting
// symlink whose own argv[0] name is not in UnlockCommands stays allowed.
func TestAgyGuardedToolVsData(t *testing.T) {
	dir := initGitRepo(t, "feat-branch", "https://github.com/giantswarm/beekeeper.git")
	scriptDir := filepath.Join(dir, "scripts")
	if err := os.MkdirAll(scriptDir, 0o750); err != nil {
		t.Fatal(err)
	}
	toolPath := filepath.Join(scriptDir, helper)
	if err := os.WriteFile(toolPath, []byte("#!/bin/sh\necho secret\n"), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	// Preexisting symlink whose own name ("vault-status") is NOT in
	// UnlockCommands, pointing to the same file on disk.
	roLink := filepath.Join(scriptDir, "vault-status")
	if err := os.Symlink(toolPath, roLink); err != nil {
		t.Fatal(err)
	}

	h := Hook{
		Self:           testBeekeeperExe,
		Shell:          zshCmd,
		UnlockCommands: []string{helper},
		Upstream:       Upstream{Orgs: []string{allowedOrg}},
	}

	// Ordinary reads and inspection of paths naming the guarded tool pass.
	for _, cmd := range []string{
		"git add " + toolPath,
		"git diff -- " + toolPath,
		"git log -p -- " + toolPath,
		"git grep secret -- " + scriptDir + "/" + helper,
		"grep -n secret " + toolPath,
		"cat " + toolPath,
		"head -n 5 " + toolPath,
		"tail -n 5 " + toolPath,
		"wc -l " + toolPath,
		"stat " + toolPath,
		"ls -la " + toolPath,
		"python3 -m py_compile " + toolPath,
		// Preexisting symlink whose argv[0] basename is not in UnlockCommands.
		roLink,
	} {
		if d := decideAgyCmd(t, h, dir, cmd); d.Decision != decisionAsk {
			t.Errorf("read/inspect %q: got %+v, want ask", cmd, d)
		}
	}

	// Running the guarded tool directly, via stdin/here-string/here-doc/pipe
	// into a shell, via a copy or symlink created on the same line, via a git
	// alias, or via a variable or loop is denied.
	for _, cmd := range []string{
		helper,
		toolPath,
		"bash " + toolPath,
		"sh " + toolPath,
		"source " + toolPath,
		". " + toolPath,
		"bash < " + toolPath,
		"sh -s < " + toolPath,
		"python3 - < " + toolPath,
		"bash <<< '" + helper + "'",
		"sh <<< \"" + helper + "\"",
		"cat <<'EOF' | sh\n" + helper + "\nEOF",
		"cat " + toolPath + " | sh",
		"cat " + toolPath + " | bash -s",
		"cp " + toolPath + " /tmp/x && /tmp/x",
		"mv " + toolPath + " /tmp/x && /tmp/x",
		"ln -s " + toolPath + " /tmp/y; /tmp/y",
		"cat " + toolPath + " > /tmp/z; bash /tmp/z",
		"tee /tmp/z < " + toolPath + " && /tmp/z",
		"dd if=" + toolPath + " of=/tmp/z && /tmp/z",
		"git -c alias.x='!" + helper + "' x",
		"git -c alias.x='!" + toolPath + "' x",
		"X=" + helper + "; $X",
		"X=" + toolPath + " && \"$X\"",
		"for c in " + helper + "; do $c; done",
		"for c in " + toolPath + "; do \"$c\"; done",
	} {
		if d := decideAgyCmd(t, h, dir, cmd); d.Decision != decisionDeny {
			t.Errorf("guarded tool run %q: got %+v, want deny", cmd, d)
		}
	}
}

// Comment 1 differential test: generate 200+ command lines mixing cd/pushd/popd/cd -,
// true/false, git push, and shell operators (; && || | & \n ( ) { } $( ) eval !
// comments and backslash-newlines), run each in real bash against two temporary
// repositories (one on main, one on feat), and verify the guard never allows a
// command line that pushed main in real bash.
func TestPushDifferentialBash(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	mainRepo := initGitRepo(t, "main", "https://github.com/giantswarm/beekeeper.git")
	featRepo := initGitRepo(t, "feat", "https://github.com/giantswarm/beekeeper.git")

	// Fake git wrapper that records the branch of the working directory
	// whenever `git push` (or `git -C <dir> push`) runs in bash.
	binDir := t.TempDir()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(binDir, "git")
	script := "#!/usr/bin/env bash\n" +
		"dir=\"$PWD\"\n" +
		"args=(\"$@\")\n" +
		"i=0\n" +
		"is_push=0\n" +
		"while [ $i -lt ${#args[@]} ]; do\n" +
		"  a=\"${args[$i]}\"\n" +
		"  if [ \"$a\" = \"-C\" ]; then\n" +
		"    i=$((i+1))\n" +
		"    dir=\"${args[$i]}\"\n" +
		"  elif [[ \"$a\" == -C* ]]; then\n" +
		"    dir=\"${a#-C}\"\n" +
		"  elif [ \"$a\" = \"push\" ]; then\n" +
		"    is_push=1\n" +
		"    break\n" +
		"  fi\n" +
		"  i=$((i+1))\n" +
		"done\n" +
		"if [ \"$is_push\" = \"1\" ]; then\n" +
		"  b=$(\"" + realGit + "\" -C \"$dir\" rev-parse --abbrev-ref HEAD 2>/dev/null)\n" +
		"  echo \"$b\" >> \"$PUSH_LOG\"\n" +
		"  exit 0\n" +
		"fi\n" +
		"exec \"" + realGit + "\" \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil { //nolint:gosec // test wrapper script
		t.Fatal(err)
	}

	var lines []string
	dirs := []string{mainRepo, featRepo}
	cdForms := []func(string) string{
		func(d string) string { return "cd " + d },
		func(d string) string { return "pushd " + d + " >/dev/null" },
		func(d string) string { return "(cd " + d + ")" },
		func(d string) string { return "{ cd " + d + "; }" },
		func(d string) string { return "echo $(cd " + d + ") >/dev/null" },
		func(d string) string { return "cd " + d + " | true" },
		func(d string) string { return "eval 'cd " + d + "'" },
		func(d string) string { return "! cd " + d },
		func(d string) string { return "if true; then cd " + d + "; fi" },
	}
	ops := []string{"; ", andOp, orOp, "\n"}
	pushes := []string{plainGitPush, pushOriginHEAD, "git \\\npush"}

	for _, d := range dirs {
		for _, cf := range cdForms {
			for _, op := range ops {
				for _, p := range pushes {
					lines = append(lines, cf(d)+op+p)
				}
			}
		}
	}
	for _, prefix := range []string{flagOn, "false"} {
		for _, op1 := range []string{andOp, orOp, "; "} {
			for _, d := range dirs {
				for _, op2 := range []string{andOp, orOp, "; ", "\n"} {
					lines = append(lines, prefix+op1+"cd "+d+op2+"git push")
				}
			}
		}
	}
	// Additional multi-step pushd/popd/cd -/background/comment variations.
	lines = append(lines,
		"cd "+featRepo+" && cd "+mainRepo+" && cd - && git push",
		"cd "+mainRepo+" && cd "+featRepo+" && cd - && git push",
		"pushd "+featRepo+" >/dev/null && pushd "+mainRepo+" >/dev/null && popd >/dev/null && git push",
		"pushd "+mainRepo+" >/dev/null && pushd "+featRepo+" >/dev/null && popd >/dev/null && git push",
		"cd "+featRepo+" & wait; git push",
		"true && cd "+featRepo+" & wait; git push",
		"cd "+featRepo+" # comment\ngit push",
		"true # cd "+featRepo+"\ngit push",
		"true #\\\ngit push",
		"D="+featRepo+"; cd \"$D\" && git push",
		"D="+mainRepo+"; cd \"$D\" && git push",
	)

	if len(lines) < 200 {
		t.Fatalf("generated %d lines, want >= 200", len(lines))
	}

	h := Hook{
		Self:     testBeekeeperExe,
		Shell:    zshCmd,
		Upstream: Upstream{Orgs: []string{allowedOrg}},
	}
	logFile := filepath.Join(t.TempDir(), "push.log")
	pathEnv := binDir + string(os.PathListSeparator) + os.Getenv("PATH")

	for _, startDir := range []string{mainRepo, featRepo} {
		for _, line := range lines {
			_ = os.Remove(logFile)
			c := exec.Command("bash", "-c", line) //nolint:gosec // test helper
			c.Dir = startDir
			c.Env = append(os.Environ(), "PATH="+pathEnv, "PUSH_LOG="+logFile)
			_ = c.Run()

			raw, _ := os.ReadFile(logFile) //nolint:gosec // test helper
			pushedMain := slices.Contains(strings.Fields(string(raw)), "main")
			if !pushedMain {
				continue
			}
			if d := decideAgyCmd(t, h, startDir, line); d.Decision != decisionDeny {
				t.Errorf("start=%s line=%q pushed main in bash, but DecideAgy returned %+v", startDir, line, d)
			}
		}
	}
}
