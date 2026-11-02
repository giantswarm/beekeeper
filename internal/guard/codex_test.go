package guard

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func decideCodexRaw(t *testing.T, h Hook, raw []byte) codexSpecificOutput {
	t.Helper()
	out := h.DecideCodex(raw)
	if len(out) == 0 {
		t.Fatal("DecideCodex returned empty output")
	}
	if strings.TrimSpace(string(out)) == "{}" {
		return codexSpecificOutput{}
	}
	var parsed codexOutput
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("DecideCodex returned invalid JSON %q: %v", string(out), err)
	}
	if parsed.Specific.Event != PreToolUseEvent {
		t.Fatalf("hookEventName = %q, want %q in %s", parsed.Specific.Event, PreToolUseEvent, string(out))
	}
	return parsed.Specific
}

func decideCodexCall(t *testing.T, h Hook, cwd, tool string, input map[string]any) codexSpecificOutput {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		sessionKey:   "sess-1",
		"turn_id":    "turn-1",
		"cwd":        cwd,
		hookEventKey: PreToolUseEvent,
		toolNameKey:  tool,
		toolInputKey: input,
	})
	if err != nil {
		t.Fatal(err)
	}
	return decideCodexRaw(t, h, raw)
}

func decideCodexCmd(t *testing.T, h Hook, cwd, cmd string) codexSpecificOutput {
	t.Helper()
	return decideCodexCall(t, h, cwd, bashTool, map[string]any{commandKey: cmd})
}

// Comment 1 Finding 1: DecideCodex fails closed on malformed JSON, missing
// fields, unexpected tool shapes, NUL bytes, configuration errors, and panics.
func TestCodexFailClosed(t *testing.T) {
	featRepo := initGitRepo(t, "feat-x", "https://github.com/giantswarm/beekeeper.git")
	h := Hook{
		Self:     testBeekeeperExe,
		Shell:    zshCmd,
		Upstream: Upstream{Orgs: []string{allowedOrg}},
	}

	for name, raw := range map[string][]byte{
		"empty":              []byte(""),
		"whitespace":         []byte("   \n\t "),
		"malformed_json":     []byte("{not json"),
		"json_array":         []byte(`["Bash"]`),
		"json_null":          []byte(`null`),
		"wrong_event":        []byte(`{"hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"true"}}`),
		"missing_event":      []byte(`{"tool_name":"Bash","tool_input":{"command":"true"}}`),
		"unknown_tool":       []byte(`{"hook_event_name":"PreToolUse","tool_name":"UnknownTool","tool_input":{}}`),
		"missing_tool_name":  []byte(`{"hook_event_name":"PreToolUse","tool_input":{"command":"true"}}`),
		"empty_tool_name":    []byte(`{"hook_event_name":"PreToolUse","tool_name":"  ","tool_input":{"command":"true"}}`),
		"non_obj_tool_input": []byte(`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":"str"}`),
		"missing_tool_input": []byte(`{"hook_event_name":"PreToolUse","tool_name":"Bash"}`),
		"non_string_cwd":     []byte(`{"hook_event_name":"PreToolUse","cwd":123,"tool_name":"Bash","tool_input":{"command":"true"}}`),
		"bash_no_command":    []byte(`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{}}`),
		"bash_int_command":   []byte(`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":42}}`),
		"patch_empty":        []byte(`{"hook_event_name":"PreToolUse","tool_name":"apply_patch","tool_input":{"command":""}}`),
		"patch_no_header":    []byte(`{"hook_event_name":"PreToolUse","tool_name":"apply_patch","tool_input":{"command":"*** Begin Patch\n+hello\n*** End Patch"}}`),
		"stdin_no_chars":     []byte(`{"hook_event_name":"PreToolUse","tool_name":"write_stdin","tool_input":{"session_id":1}}`),
		"stdin_int_chars":    []byte(`{"hook_event_name":"PreToolUse","tool_name":"write_stdin","tool_input":{"chars":1}}`),
		"image_no_path":      []byte(`{"hook_event_name":"PreToolUse","tool_name":"view_image","tool_input":{}}`),
		"image_nul_byte":     []byte("{\"hook_event_name\":\"PreToolUse\",\"tool_name\":\"view_image\",\"tool_input\":{\"path\":\"a\\u0000b\"}}"),
		"bash_nul_byte":      []byte("{\"hook_event_name\":\"PreToolUse\",\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"echo a\\u0000b\"}}"),
	} {
		d := decideCodexRaw(t, h, raw)
		if d.PermissionDecision != decisionDeny || d.Reason == "" {
			t.Errorf("%s: got %+v, want deny with reason", name, d)
		}
	}

	// ConfigErr fails closed.
	errHook := h
	errHook.ConfigErr = errors.New("broken config.yaml")
	if d := decideCodexCmd(t, errHook, featRepo, "git status"); d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, "broken config.yaml") {
		t.Errorf("ConfigErr: got %+v, want deny naming config error", d)
	}

	// Panic inside a hook callback is recovered and fails closed.
	panicHook := h
	panicHook.Clusters = func() []string { panic("simulated callback panic") }
	if d := decideCodexCmd(t, panicHook, featRepo, "kind create cluster"); d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, "simulated callback panic") {
		t.Errorf("panic recovery: got %+v, want deny naming panic", d)
	}
}

// Comment 1 Finding 2: Hooked tools in Codex (Bash, apply_patch, write_stdin,
// view_image) are checked and Bash rewrites use updatedInput.command.
func TestCodexHookToolsAndRewrites(t *testing.T) {
	mainRepo := initGitRepo(t, "main", "https://github.com/giantswarm/beekeeper.git")
	featRepo := initGitRepo(t, "feat-x", "https://github.com/giantswarm/beekeeper.git")
	personDir := t.TempDir()

	h := Hook{
		Self:           testBeekeeperExe,
		Shell:          zshCmd,
		UnlockCommands: []string{helper},
		SecretFiles:    []string{filepath.Join(featRepo, "*.secret.yaml")},
		PersonFolders:  []string{personDir},
		Upstream:       Upstream{Orgs: []string{allowedOrg}},
		Outbound:       Outbound{Phrases: []string{"codex-secret-phrase"}, Paths: []string{featRepo}},
	}

	// Benign Bash command returns {} (empty codexSpecificOutput).
	if d := decideCodexCmd(t, h, featRepo, "git status"); d.PermissionDecision != "" || d.UpdatedInput != nil {
		t.Errorf("git status: got %+v, want pass {}", d)
	}

	// Main push and force push are denied.
	for _, cmd := range []string{
		"git push origin master",
		"git push -f origin feat-x",
		"gh issue create -R external-org/project --title t --body b",
	} {
		if d := decideCodexCmd(t, h, featRepo, cmd); d.PermissionDecision != decisionDeny {
			t.Errorf("%q: got %+v, want deny", cmd, d)
		}
	}
	if d := decideCodexCmd(t, h, mainRepo, "git push"); d.PermissionDecision != decisionDeny {
		t.Errorf("git push on main: got %+v, want deny", d)
	}

	// Build commands and merges are rewritten via updatedInput.command with permissionDecision: "allow".
	if d := decideCodexCmd(t, h, featRepo, "go test ./..."); d.PermissionDecision != decisionAllow ||
		!strings.Contains(d.UpdatedInput[commandKey].(string), testBeekeeperExe+" run -- ") {
		t.Errorf("go test rewrite: got %+v", d)
	}
	if d := decideCodexCmd(t, h, featRepo, "devctl pr merge giantswarm/beekeeper 7"); d.PermissionDecision != decisionAllow ||
		!strings.Contains(d.UpdatedInput[commandKey].(string), testBeekeeperExe+" gate -- devctl pr merge giantswarm/beekeeper 7") {
		t.Errorf("devctl pr merge rewrite: got %+v", d)
	}

	// apply_patch checks target paths, PersonFolders, SecretFiles, Outbound, and Delete.
	okPatch := "*** Begin Patch\n*** Update File: " + filepath.Join(featRepo, "README.md") + "\n@@\n+clean line\n*** End Patch"
	if d := decideCodexCall(t, h, featRepo, codexApplyPatchTool, map[string]any{commandKey: okPatch}); d.PermissionDecision != "" {
		t.Errorf("apply_patch clean: got %+v, want pass", d)
	}
	for _, badPatch := range []string{
		"*** Begin Patch\n*** Update File: " + filepath.Join(personDir, "notes.md") + "\n@@\n+x\n*** End Patch",
		"*** Begin Patch\n*** Delete File: " + filepath.Join(personDir, "notes.md") + "\n*** End Patch",
		"*** Begin Patch\n*** Move to: " + filepath.Join(personDir, "moved.md") + "\n*** End Patch",
		"*** Begin Patch\n*** Add File: " + filepath.Join(featRepo, "prod.secret.yaml") + "\n+x\n*** End Patch",
		"*** Begin Patch\n*** Update File: " + filepath.Join(featRepo, "README.md") + "\n@@\n+codex-secret-phrase\n*** End Patch",
	} {
		if d := decideCodexCall(t, h, featRepo, codexApplyPatchTool, map[string]any{commandKey: badPatch}); d.PermissionDecision != decisionDeny {
			t.Errorf("apply_patch %q: got %+v, want deny", badPatch, d)
		}
	}

	// write_stdin checks chars as a Bash command (allowing empty chars, refusing denied commands).
	if d := decideCodexCall(t, h, featRepo, codexWriteStdinTool, map[string]any{codexCharsKey: ""}); d.PermissionDecision != "" {
		t.Errorf("write_stdin empty: got %+v, want pass", d)
	}
	if d := decideCodexCall(t, h, featRepo, codexWriteStdinTool, map[string]any{codexCharsKey: "git status\n"}); d.PermissionDecision != "" || d.UpdatedInput != nil {
		t.Errorf("write_stdin benign: got %+v, want pass", d)
	}
	for _, badChars := range []string{
		"git push origin main\n",
		helper + "\n",
		"kubectl get secret app -o yaml\n",
	} {
		if d := decideCodexCall(t, h, featRepo, codexWriteStdinTool, map[string]any{codexCharsKey: badChars}); d.PermissionDecision != decisionDeny {
			t.Errorf("write_stdin %q: got %+v, want deny", badChars, d)
		}
	}

	// view_image checks secret files and guarded paths.
	if d := decideCodexCall(t, h, featRepo, codexViewImageTool, map[string]any{pathKey: filepath.Join(featRepo, "diagram.png")}); d.PermissionDecision != "" {
		t.Errorf("view_image normal: got %+v, want pass", d)
	}
	if d := decideCodexCall(t, h, featRepo, codexViewImageTool, map[string]any{pathKey: filepath.Join(featRepo, "prod.secret.yaml")}); d.PermissionDecision != decisionDeny {
		t.Errorf("view_image secret file: got %+v, want deny", d)
	}
}

// Comment 1 Finding 3 & Comment 2 Finding 3: Symlinked auth.json and guarded
// Codex homes ($CODEX_HOME and ~/.codex) are protected against reads of
// auth.json and against any write, patch, delete, redirect, or option abuse,
// while plain beekeeper text subcommands mentioning guarded paths pass.
func TestCodexHomeAndAuthGuard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	userCodex := filepath.Join(home, ".codex")
	workerHome := filepath.Join(home, ".local", "state", "beekeeper", "codex", "home")
	outsideDir := t.TempDir()
	for _, d := range []string{userCodex, workerHome} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CODEX_HOME", workerHome)

	// Create auth.json in outsideDir and symlink both ~/.codex/auth.json and
	// $CODEX_HOME/auth.json to it (testing symlinked auth.json whose target is
	// outside CODEX_HOME).
	realAuth := filepath.Join(outsideDir, "real-auth.json")
	if err := os.WriteFile(realAuth, []byte(`{"access_token":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	userAuth := filepath.Join(userCodex, "auth.json")
	workerAuth := filepath.Join(workerHome, "auth.json")
	if err := os.Symlink(realAuth, userAuth); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(userAuth, workerAuth); err != nil {
		t.Fatal(err)
	}
	// External symlink pointing to ~/.codex/hooks.json.
	extHooksLink := filepath.Join(outsideDir, "link-hooks.json")
	if err := os.Symlink(filepath.Join(userCodex, "hooks.json"), extHooksLink); err != nil {
		t.Fatal(err)
	}

	featRepo := initGitRepo(t, "feat-x", "https://github.com/giantswarm/beekeeper.git")
	h := Hook{
		Self:     testBeekeeperExe,
		Shell:    zshCmd,
		Upstream: Upstream{Orgs: []string{allowedOrg}},
	}

	// Reading ~/.codex/auth.json or $CODEX_HOME/auth.json (even when symlinked
	// to outsideDir) is denied across Bash, Read, and view_image.
	for _, cmd := range []string{
		"cat ~/.codex/auth.json",
		"cat " + userAuth,
		"cat " + workerAuth,
		"head -n 5 " + workerAuth,
		"beekeeper note add -- 'test' < ~/.codex/auth.json",
		"beekeeper note add -- 'test' < " + workerAuth,
		"beekeeper --config ~/.codex/auth.json status",
		"beekeeper --config=" + workerAuth + " status",
		"beekeeper --as $(cat ~/.codex/auth.json) status",
	} {
		if d := decideCodexCmd(t, h, featRepo, cmd); d.PermissionDecision != decisionDeny {
			t.Errorf("auth.json read/leak %q: got %+v, want deny", cmd, d)
		}
	}
	for _, p := range []string{userAuth, workerAuth, "~/.codex/auth.json"} {
		if d := decideCodexCall(t, h, featRepo, readTool, map[string]any{filePathKey: p}); d.PermissionDecision != decisionDeny {
			t.Errorf("Read(%q): got %+v, want deny", p, d)
		}
		if d := decideCodexCall(t, h, featRepo, codexViewImageTool, map[string]any{pathKey: p}); d.PermissionDecision != decisionDeny {
			t.Errorf("view_image(%q): got %+v, want deny", p, d)
		}
	}

	// Writing to ~/.codex or $CODEX_HOME (directly, via symlink, via apply_patch,
	// or via redirections on exempted beekeeper text commands) is denied.
	for _, target := range []string{
		filepath.Join(userCodex, "hooks.json"),
		filepath.Join(workerHome, "hooks.json"),
		filepath.Join(workerHome, "config.toml"),
		userAuth,
		workerAuth,
		extHooksLink,
	} {
		patch := "*** Begin Patch\n*** Update File: " + target + "\n@@\n+x\n*** End Patch"
		if d := decideCodexCall(t, h, featRepo, codexApplyPatchTool, map[string]any{commandKey: patch}); d.PermissionDecision != decisionDeny {
			t.Errorf("apply_patch on %s: got %+v, want deny", target, d)
		}
		if d := decideCodexCall(t, h, featRepo, writeTool, map[string]any{filePathKey: target, contentKey: "x"}); d.PermissionDecision != decisionDeny {
			t.Errorf("Write on %s: got %+v, want deny", target, d)
		}
	}

	for _, cmd := range []string{
		`beekeeper note add -- "test" > ~/.codex/hooks.json`,
		`beekeeper note add -- "test" 2> ~/.codex/hooks.json`,
		`beekeeper note add -- "test" 2>> ~/.codex/hooks.json`,
		`beekeeper note add -- "test" 2>| ~/.codex/hooks.json`,
		`beekeeper note add -- "test" &> ~/.codex/hooks.json`,
		`beekeeper note add -- "test" <> ~/.codex/hooks.json`,
		`beekeeper note add -- "test" <<< ~/.codex/hooks.json`,
		`beekeeper note add -- "test" > ` + workerHome + `/hooks.json`,
		`beekeeper note add -- "test" 2> ` + extHooksLink,
		`beekeeper --config ~/.codex/config.toml note add "x"`,
		`beekeeper --as "~/.local/state/beekeeper/codex/home" note add "x"`,
		`beekeeper --as="~/.codex/hooks.json" note add "x"`,
		`cp /tmp/x ~/.codex/hooks.json`,
		`rm -f ` + workerHome + `/hooks.json`,
		`sed -i 's/a/b/' ~/.codex/config.toml`,
		`python3 -c 'open("~/.codex/hooks.json", "w").write("{}")'`,
	} {
		if d := decideCodexCmd(t, h, featRepo, cmd); d.PermissionDecision != decisionDeny {
			t.Errorf("codex home write/abuse %q: got %+v, want deny", cmd, d)
		}
	}

	// Plain beekeeper text subcommands mentioning ~/.codex or CODEX_HOME in
	// positional arguments pass.
	for _, cmd := range []string{
		`beekeeper note add --for "Supervisor" --ref giantswarm/beekeeper#1708 --due 12:30 --default "x" --status-quo "y" --why "z" "writes ~/.local/state/beekeeper/codex/home and ~/.codex/hooks.json"`,
		`beekeeper log add "configured ~/.local/state/beekeeper/codex/home"`,
		`beekeeper agents idle --done --report "PR ready: ~/.local/state/beekeeper/codex/home"`,
		`beekeeper agents park --on giantswarm/beekeeper#1708 "waiting on ~/.codex/hooks.json"`,
	} {
		if d := decideCodexCmd(t, h, featRepo, cmd); d.PermissionDecision != "" {
			t.Errorf("plain beekeeper text subcommand %q: got %+v, want pass", cmd, d)
		}
	}
}

// Comment 3 Finding 2: Repository files whose basename matches an UnlockCommands
// entry can be read, inspected with git/rg/grep/sed -n, and patched via
// apply_patch, while executing, sourcing, piping to shell, copying, redirecting,
// or touching the installed ~/.local/bin/<helper> is denied.
func TestCodexRepoUnlockHelperReadsAndPatchesPassRunsDenied(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	localBin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(localBin, 0o750); err != nil {
		t.Fatal(err)
	}
	installedHelper := filepath.Join(localBin, helper)
	if err := os.WriteFile(installedHelper, []byte("#!/bin/sh\necho installed\n"), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}

	repo := initGitRepo(t, "feat-x", "https://github.com/giantswarm/beekeeper.git")
	rel := filepath.Join("scripts", "install", "files", "bin", helper)
	full := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("#!/usr/bin/env bash\necho guard\n"), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}

	h := Hook{
		Self:           testBeekeeperExe,
		Shell:          zshCmd,
		UnlockCommands: []string{helper},
		Upstream:       Upstream{Orgs: []string{allowedOrg}},
	}

	// Read-only inspection and git staging/inspection of the repo file pass.
	for _, cmd := range []string{
		"cat " + rel,
		"sed -n '1,120p' " + rel,
		"head -n 40 " + rel,
		"tail -n 40 " + rel,
		"grep -n guard " + rel,
		"rg guard " + rel,
		"nl -ba " + rel + " | sed -n '1,20p'",
		"git show HEAD:" + rel,
		"git diff " + rel,
		"git diff " + rel + " > /tmp/diff.out",
		"git log -p -1 " + rel,
		"git status " + rel,
		"git blame " + rel,
		"git add " + rel,
		"rg -g '!" + helper + "' foo",
		"rg -g '!" + helper + "' foo | xargs wc -l",
	} {
		if d := decideCodexCmd(t, h, repo, cmd); d.PermissionDecision != "" {
			t.Errorf("repo helper read/inspect %q: got %+v, want pass", cmd, d)
		}
	}

	// Patching the repository file via apply_patch tool or pure Bash apply_patch passes.
	patch := "*** Begin Patch\n*** Update File: " + rel + "\n@@\n-echo guard\n+echo updated\n*** End Patch"
	if d := decideCodexCall(t, h, repo, codexApplyPatchTool, map[string]any{commandKey: patch}); d.PermissionDecision != "" {
		t.Errorf("apply_patch tool on repo helper: got %+v, want pass", d)
	}
	if d := decideCodexCmd(t, h, repo, "apply_patch <<'EOF'\n"+patch+"\nEOF"); d.PermissionDecision != "" {
		t.Errorf("Bash apply_patch on repo helper: got %+v, want pass", d)
	}
	if d := decideCodexCmd(t, h, filepath.Dir(repo), "cd "+repo+" && apply_patch <<'EOF'\n"+patch+"\nEOF"); d.PermissionDecision != "" {
		t.Errorf("Bash cd && apply_patch on repo helper: got %+v, want pass", d)
	}

	// Running, sourcing, piping to shell, copying, redirecting, sed e/-i, or
	// reading/patching ~/.local/bin/<helper> is denied.
	for _, bad := range []string{
		helper,
		"~/.local/bin/" + helper,
		"cat ~/.local/bin/" + helper,
		"./" + rel,
		full,
		"bash " + rel,
		"sh < " + rel,
		"python3 - < " + rel,
		`bash <<< "$(cat ` + rel + `)"`,
		`sh -c "$(cat ` + rel + `)"`,
		`eval "$(cat ` + rel + `)"`,
		"cat <<'EOF' | sh\n" + full + "\nEOF",
		"cat " + rel + " | sh",
		"git show HEAD:" + rel + " | sh",
		"cat " + rel + " > /tmp/x && bash /tmp/x",
		"cp " + rel + " /tmp/x && /tmp/x",
		"ln -s " + full + " /tmp/x && /tmp/x",
		"sed '1e id' " + rel,
		"sed -i 's/a/b/' " + rel,
		"git -c core.pager=sh show HEAD:" + rel,
	} {
		if d := decideCodexCmd(t, h, repo, bad); d.PermissionDecision != decisionDeny {
			t.Errorf("repo helper execution/copy %q: got %+v, want deny", bad, d)
		}
	}

	badPatch := "*** Begin Patch\n*** Update File: ~/.local/bin/" + helper + "\n@@\n+x\n*** End Patch"
	if d := decideCodexCall(t, h, repo, codexApplyPatchTool, map[string]any{commandKey: badPatch}); d.PermissionDecision != decisionDeny {
		t.Errorf("apply_patch on ~/.local/bin/%s: got %+v, want deny", helper, d)
	}
	if d := decideCodexCmd(t, h, repo, "apply_patch <<'EOF'\n"+badPatch+"\nEOF"); d.PermissionDecision != decisionDeny {
		t.Errorf("Bash apply_patch on ~/.local/bin/%s: got %+v, want deny", helper, d)
	}

	// CodexPaths and CodexSession extract paths and session_id.
	raw, _ := json.Marshal(map[string]any{
		sessionKey:   "sess-codex-1",
		"cwd":        repo,
		hookEventKey: PreToolUseEvent,
		toolNameKey:  codexApplyPatchTool,
		toolInputKey: map[string]any{commandKey: patch},
	})
	if got := CodexSession(raw); got != "sess-codex-1" {
		t.Errorf("CodexSession = %q, want sess-codex-1", got)
	}
	if paths := CodexPaths(raw); !slices.Contains(paths, full) {
		t.Errorf("CodexPaths = %v, want %s included", paths, full)
	}
}

// Desk review R1 regressions:
// 1. Unquoted heredoc command substitutions (`apply_patch <<EOF` with `$(...)` or “ `...` “) are denied.
// 2. `codexBashPureApplyPatch` returns `isPure == true` and preserves `additionalContext` (`repoReads`) on `Bash` `apply_patch <<'EOF'`.
// 3. Indented or malformed `***` headers in `apply_patch` are checked and denied.
// 4. Shell tool aliases (`exec_command`, `shell_command`, `shell`, `local_shell`, `bash`) and `update_plan` are handled.
// 5. `decideCodexEvent` attaches `repoReads` (`additionalContext`) on `Bash` `git commit` and `Write` in another repository.
func TestCodexReviewR1Regressions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	userCodex := filepath.Join(home, ".codex")
	if err := os.MkdirAll(userCodex, 0o700); err != nil {
		t.Fatal(err)
	}

	proj := repoWith(t, filepath.Join(t.TempDir(), "proj"), map[string]string{
		claudeMD: projectRules,
	})
	otherA := repoWith(t, filepath.Join(t.TempDir(), "other-a"), map[string]string{
		claudeMD: otherRules,
	})
	otherB := repoWith(t, filepath.Join(t.TempDir(), "other-b"), map[string]string{
		claudeMD: "other-b rules",
	})
	otherC := repoWith(t, filepath.Join(t.TempDir(), "other-c"), map[string]string{
		claudeMD: "other-c rules",
	})
	otherD := repoWith(t, filepath.Join(t.TempDir(), "other-d"), map[string]string{
		claudeMD: "other-d rules",
	})

	h := readsHook(t, proj)
	h.Self = testBeekeeperExe
	h.Shell = zshCmd
	h.SecretFiles = []string{"~/.ssh/id_*"}
	h.Upstream = Upstream{Orgs: []string{allowedOrg}}

	// 1. Unquoted heredocs on apply_patch (with or without command substitutions)
	// and unquoted heredocs carrying forbidden command substitutions are denied.
	for _, badCmd := range []string{
		"apply_patch <<EOF\n*** Begin Patch\n*** Update File: README.md\n@@\n+$(git push --force origin main)\n*** End Patch\nEOF",
		"apply_patch <<EOF\n*** Begin Patch\n*** Update File: README.md\n@@\n+$(cat ~/.ssh/id_ed25519)\n*** End Patch\nEOF",
		"apply_patch <<-EOF\n*** Begin Patch\n*** Update File: README.md\n@@\n+`cat ~/.ssh/id_ed25519`\n*** End Patch\nEOF",
		"apply_patch <<EOF\n*** Begin Patch\n*** Update File: README.md\n@@\n+plain line\n*** End Patch\nEOF",
		"echo start && apply_patch <<EOF\n*** Begin Patch\n*** Update File: README.md\n@@\n+$(git push --force origin main)\n*** End Patch\nEOF",
		"cat <<EOF > /tmp/out\n$(git push --force origin main)\nEOF",
		"cat <<EOF > /tmp/out\n$(cat ~/.ssh/id_ed25519)\nEOF",
		"cat <<EOF > /tmp/out\n$(cat ~/.codex/auth.json)\nEOF",
	} {
		if d := decideCodexCmd(t, h, proj, badCmd); d.PermissionDecision != decisionDeny {
			t.Errorf("unquoted heredoc %q: got %+v, want deny", badCmd, d)
		}
	}

	// 2. Pure Bash apply_patch <<'EOF' and cd && apply_patch <<'EOF' return isPure == true
	// and return repoReads in additionalContext when writing to another repository.
	patchA := "*** Begin Patch\n*** Update File: a.txt\n@@\n+hello\n*** End Patch"
	pureCmd := "apply_patch <<'EOF'\n" + patchA + "\nEOF"
	if isPure, reason, _ := h.codexBashPureApplyPatch(event{CWD: proj}, pureCmd, proj); !isPure || reason != "" {
		t.Fatalf("codexBashPureApplyPatch(pureCmd) = (%v, %q), want (true, \"\")", isPure, reason)
	}
	if d := decideCodexCmd(t, h, otherA, pureCmd); d.PermissionDecision != "" || !strings.Contains(d.Context, otherRules) {
		t.Errorf("Bash apply_patch in otherA: got %+v, want pass with %q in additionalContext", d, otherRules)
	}
	cdPureCmd := "cd " + otherB + " && apply_patch <<'EOF'\n" + patchA + "\nEOF"
	if d := decideCodexCmd(t, h, proj, cdPureCmd); d.PermissionDecision != "" || !strings.Contains(d.Context, "other-b rules") {
		t.Errorf("Bash cd && apply_patch in otherB: got %+v, want pass with other-b rules in additionalContext", d)
	}

	// 3. Indented or malformed `***` headers in apply_patch are checked and denied.
	for _, badPatch := range []string{
		"*** Begin Patch\n*** Update File: README.md\n@@\n+ok\n  *** Update File: ~/.codex/hooks.json\n@@\n+{}\n*** End Patch",
		"\t*** Update File: ~/.codex/hooks.json\n@@\n+{}",
		"*** Begin Patch\n*** Update File: README.md\n@@\n+ok\n*** Add File:~/.codex/hooks.json\n+{}\n*** End Patch",
		"*** Begin Patch\n*** Update File: README.md\n@@\n+ok\n*** Unknown Header: foo\n*** End Patch",
	} {
		if d := decideCodexCall(t, h, proj, codexApplyPatchTool, map[string]any{commandKey: badPatch}); d.PermissionDecision != decisionDeny {
			t.Errorf("indented/malformed apply_patch %q: got %+v, want deny", badPatch, d)
		}
		if d := decideCodexCmd(t, h, proj, "apply_patch <<'EOF'\n"+badPatch+"\nEOF"); d.PermissionDecision != decisionDeny {
			t.Errorf("Bash indented/malformed apply_patch %q: got %+v, want deny", badPatch, d)
		}
	}

	// 4. Documented shell tool aliases and update_plan are handled; unknown tools are denied.
	for _, shellTool := range []string{"exec_command", "shell_command", "shell", "local_shell", "bash"} {
		if d := decideCodexCall(t, h, proj, shellTool, map[string]any{codexCmdKey: "git push --force origin main"}); d.PermissionDecision != decisionDeny {
			t.Errorf("%s force push: got %+v, want deny", shellTool, d)
		}
		d := decideCodexCall(t, h, proj, shellTool, map[string]any{codexCmdKey: "go test ./internal/..."})
		if d.PermissionDecision != decisionAllow || !strings.Contains(d.UpdatedInput[codexCmdKey].(string), testBeekeeperExe+" run -- ") {
			t.Errorf("%s build rewrite: got %+v", shellTool, d)
		}
	}
	if d := decideCodexCall(t, h, proj, codexUpdatePlanTool, map[string]any{"plan": []any{}}); d.PermissionDecision != "" {
		t.Errorf("update_plan: got %+v, want pass", d)
	}

	// 5. decideCodexEvent attaches repoReads on Bash git commit and Write in another repository.
	if d := decideCodexCmd(t, h, proj, "git -C "+otherC+" commit -m msg"); d.PermissionDecision != "" || !strings.Contains(d.Context, "other-c rules") {
		t.Errorf("Bash git commit in otherC: got %+v, want pass with other-c rules in additionalContext", d)
	}
	if d := decideCodexCall(t, h, proj, writeTool, map[string]any{filePathKey: filepath.Join(otherD, "main.go"), contentKey: "package main\n"}); d.PermissionDecision != "" || !strings.Contains(d.Context, "other-d rules") {
		t.Errorf("Write in otherD: got %+v, want pass with other-d rules in additionalContext", d)
	}
}

// Web tools use the shared guard rather than being rejected as unknown.
func TestCodexWebTools(t *testing.T) {
	for _, tool := range []string{codexWebTool, "WebFetch", webSearchTool} {
		t.Run(tool, func(t *testing.T) {
			input := map[string]any{"search_query": []any{map[string]any{"q": executionFixture}}}
			raw := executionCodexEvent(tool, input, executionThread, t.TempDir())
			if reason := ValidateCodexInput(raw); reason != "" {
				t.Fatal(reason)
			}
			if d := decideCodexRaw(t, Hook{}, raw); d.PermissionDecision == decisionDeny {
				t.Fatalf("web tool denied: %+v", d)
			}
			if d := decideCodexRaw(t, Hook{ConfigErr: errors.New("invalid configuration")}, raw); d.PermissionDecision != decisionDeny {
				t.Fatal("configuration error did not fail closed")
			}
		})
	}
	for _, input := range []any{nil, "invalid", map[string]any{"q": "a\x00b"}} {
		raw, _ := json.Marshal(map[string]any{hookEventKey: PreToolUseEvent, toolNameKey: "webrun", toolInputKey: input})
		if reason := ValidateCodexInput(raw); reason == "" {
			t.Fatalf("invalid web input accepted: %s", raw)
		}
	}
}

func TestCodexExecCommandWorkdirBodyFile(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "session")
	worktreeDir := filepath.Join(sessionDir, "worktrees", "beekeeper")
	for _, d := range []string{sessionDir, worktreeDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(worktreeDir, "PR_BODY.md"), []byte("Closes https://github.com/giantswarm/beekeeper/issues/1874\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktreeDir, "BAD_BODY.md"), []byte("Closes #1874\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "SESSION_ONLY.md"), []byte("Closes https://github.com/giantswarm/beekeeper/issues/1874\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	h := Hook{
		Self:     testBeekeeperExe,
		Shell:    zshCmd,
		Upstream: Upstream{Orgs: []string{allowedOrg}},
	}

	const createCmd = `gh pr create --repo giantswarm/beekeeper --title "fix: resolve workdir" --body-file PR_BODY.md`
	for _, wd := range []string{worktreeDir, filepath.Join("worktrees", "beekeeper")} {
		d := decideCodexCall(t, h, sessionDir, codexExecCommand, map[string]any{
			codexCmdKey:     createCmd,
			codexWorkdirKey: wd,
		})
		if d.PermissionDecision == decisionDeny {
			t.Errorf("workdir=%q good body-file denied: %s", wd, d.Reason)
		}
	}

	badD := decideCodexCall(t, h, sessionDir, codexExecCommand, map[string]any{
		codexCmdKey:     `gh pr create --repo giantswarm/beekeeper --title "fix: resolve workdir" --body-file BAD_BODY.md`,
		codexWorkdirKey: worktreeDir,
	})
	if badD.PermissionDecision != decisionDeny || !strings.Contains(badD.Reason, "#1874 without its full URL") {
		t.Errorf("unlinked ref in workdir body-file: got %+v, want deny naming #1874", badD)
	}

	sessionOnlyD := decideCodexCall(t, h, sessionDir, codexExecCommand, map[string]any{
		codexCmdKey:     `gh pr create --repo giantswarm/beekeeper --title "fix: resolve workdir" --body-file SESSION_ONLY.md`,
		codexWorkdirKey: worktreeDir,
	})
	if sessionOnlyD.PermissionDecision != decisionDeny || !strings.Contains(sessionOnlyD.Reason, "--body-file SESSION_ONLY.md cannot be read") {
		t.Errorf("file only in sessionDir with workdir set: got %+v, want unreadable --body-file refusal", sessionOnlyD)
	}

	if err := os.WriteFile(filepath.Join(worktreeDir, "MENTION_BODY.md"), []byte("Closes https://github.com/giantswarm/beekeeper/issues/1874 cc @someone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mentionD := decideCodexCall(t, h, sessionDir, codexExecCommand, map[string]any{
		codexCmdKey:     `gh pr create --repo giantswarm/beekeeper --title "fix: resolve workdir" --body-file MENTION_BODY.md`,
		codexWorkdirKey: worktreeDir,
	})
	if mentionD.PermissionDecision != decisionDeny || !strings.Contains(mentionD.Reason, "someone") {
		t.Errorf("mention in workdir body-file: got %+v, want deny naming someone", mentionD)
	}

	raw, _ := json.Marshal(map[string]any{
		sessionKey:   "sess-codex-wd",
		"cwd":        sessionDir,
		hookEventKey: PreToolUseEvent,
		toolNameKey:  codexExecCommand,
		toolInputKey: map[string]any{
			codexCmdKey:     createCmd,
			codexWorkdirKey: filepath.Join("worktrees", "beekeeper"),
		},
	})
	if paths := CodexPaths(raw); !slices.Contains(paths, worktreeDir) {
		t.Errorf("CodexPaths = %v, want %s included", paths, worktreeDir)
	}

	badWD, _ := json.Marshal(map[string]any{
		hookEventKey: PreToolUseEvent,
		toolNameKey:  codexExecCommand,
		toolInputKey: map[string]any{
			codexCmdKey:     "git status",
			codexWorkdirKey: 123,
		},
	})
	if d := decideCodexRaw(t, h, badWD); d.PermissionDecision != decisionDeny {
		t.Errorf("non-string workdir: got %+v, want deny", d)
	}
}

