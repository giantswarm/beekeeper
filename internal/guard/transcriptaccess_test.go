package guard

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	transcriptOutputCount     = "count"
	transcriptOwner           = "granted-session"
	transcriptOther           = "foreign-session"
	transcriptWorker          = "reader-session"
	transcriptReasonName      = "private harness transcripts"
	transcriptConversationKey = "conversationId"
	transcriptAbsolutePath    = "AbsolutePath"
)

func transcriptHook() Hook {
	h := hook()
	h.Transcripts.Managed = func(string) (bool, error) { return true, nil }
	return h
}

func TestTranscriptReads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "worker-codex"))
	h := transcriptHook()
	paths := []string{
		"~/.codex/sessions/2026/rollout-other.jsonl", "~/.codex/archived_sessions/other.jsonl", "~/.codex/history.jsonl",
		"$CODEX_HOME/sessions/other.jsonl", "${CODEX_HOME}/archived_sessions/other.jsonl",
		"~/.claude/projects/project/other.jsonl", "~/.claude/projects/project/other/tool-results/result.txt",
		"~/.gemini/antigravity-cli/brain/other/.system_generated/logs/transcript.jsonl",
		"~/.gemini/antigravity-cli/conversations/other.db", "~/.gemini/tmp/project/chats/session.json",
		"~/.omp/agent/sessions/project/other.jsonl",
	}
	for _, p := range paths {
		for _, command := range []string{
			"cat " + p, "jq 'keys' " + p, "grep -c token_count " + p, "cp " + p + " /tmp/copy",
			"cat " + p + " | wc -l", "cat " + p + " >/dev/null", "bash -c 'head " + p + "'",
			"python3 -c 'print(open(\"" + p + "\").read())'", "f=" + p + "; cat $f",
		} {
			d := decide(t, h, home, command, nil)
			if d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, transcriptReasonName) {
				t.Errorf("not refused: %s: %+v", command, d)
			}
		}
	}
	for _, command := range []string{"rg token_count ~/.codex", "cat ~/.codex/sessions/**/*.jsonl", "cd ~/.codex/sessions && head other.jsonl", "find ~/.claude/projects -exec cat {} +"} {
		d := decide(t, h, home, command, nil)
		if d == nil || d.PermissionDecision != decisionDeny {
			t.Errorf("not refused: %s", command)
		}
	}
}

func transcriptDecision(t *testing.T, h Hook, session, tool, p string) *hookOutput {
	t.Helper()
	b, err := json.Marshal(map[string]any{sessionKey: session, toolNameKey: tool, toolInputKey: map[string]any{filePathKey: p, pathKey: p, outputModeKey: transcriptOutputCount}})
	if err != nil {
		t.Fatal(err)
	}
	out := h.Decide(b)
	if out == nil {
		return nil
	}
	var response struct {
		Output hookOutput `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &response); err != nil {
		t.Fatal(err)
	}
	return &response.Output
}

func TestTranscriptFileToolsAndGrants(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".codex", "sessions")
	own := filepath.Join(root, "own.jsonl")
	other := filepath.Join(root, "other.jsonl")
	fixture := filepath.Join(root, "fixture")
	h := transcriptHook()
	h.Transcripts.Grants = map[string][]string{transcriptOwner: {own, fixture}}
	for _, tc := range []struct {
		session, tool, p string
		deny             bool
	}{
		{transcriptOwner, readTool, own, false}, {transcriptOther, readTool, own, true}, {"", readTool, own, true},
		{transcriptOwner, readTool, other, true}, {transcriptOwner, readTool, filepath.Join(fixture, "shape.json"), false},
		{transcriptOwner, grepTool, root, true}, {transcriptOwner, globTool, root, true},
		{transcriptOwner, readTool, filepath.Join(home, "fixture.json"), false},
	} {
		d := transcriptDecision(t, h, tc.session, tc.tool, tc.p)
		if denied := d != nil && d.PermissionDecision == decisionDeny; denied != tc.deny {
			t.Errorf("%+v: %+v", tc, d)
		}
	}
	h.ConfigErr = errors.New("invalid policy")
	if d := transcriptDecision(t, h, transcriptOwner, readTool, own); d == nil || d.PermissionDecision != decisionDeny {
		t.Fatal("configuration failure passed")
	}
}

func TestTranscriptLinks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".claude", "projects")
	fixture := filepath.Join(root, "fixture")
	if err := os.MkdirAll(fixture, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(home, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, transcriptOther), filepath.Join(fixture, "escape")); err != nil {
		t.Fatal(err)
	}
	h := transcriptHook()
	h.Transcripts.Grants = map[string][]string{transcriptOwner: {fixture}}
	for _, p := range []string{filepath.Join(home, "alias", "missing.jsonl"), filepath.Join(fixture, "escape", "missing.jsonl")} {
		if d := transcriptDecision(t, h, transcriptOwner, readTool, p); d == nil || d.PermissionDecision != decisionDeny {
			t.Errorf("link passed: %s", p)
		}
	}
}

func TestTranscriptHarnessFormats(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := filepath.Join(home, ".codex", "sessions", "other.jsonl")
	h := transcriptHook()
	cases := []struct {
		format  string
		payload map[string]any
	}{
		{secretLinkCodex, map[string]any{hookEventKey: PreToolUseEvent, sessionKey: transcriptWorker, toolNameKey: "exec_command", toolInputKey: map[string]any{"cmd": "jq 'keys' " + p}}},
		{secretLinkCodex, map[string]any{hookEventKey: PreToolUseEvent, sessionKey: transcriptWorker, toolNameKey: "write_stdin", toolInputKey: map[string]any{"chars": "cat " + p + "\n"}}},
		{secretLinkCodex, map[string]any{hookEventKey: PreToolUseEvent, sessionKey: transcriptWorker, toolNameKey: "view_image", toolInputKey: map[string]any{pathKey: p}}},
		{agyHarness, map[string]any{transcriptConversationKey: transcriptWorker, "toolCall": map[string]any{"name": "run_command", "args": map[string]any{"CommandLine": "cat " + p}}}},
		{agyHarness, map[string]any{transcriptConversationKey: transcriptWorker, "toolCall": map[string]any{"name": "view_file", "args": map[string]any{transcriptAbsolutePath: p}}}},
	}
	for _, tc := range cases {
		raw, err := json.Marshal(tc.payload)
		if err != nil {
			t.Fatal(err)
		}
		var out []byte
		if tc.format == secretLinkCodex {
			out = h.DecideCodex(raw)
		} else {
			out = h.DecideAgy(raw)
		}
		if !strings.Contains(string(out), transcriptReasonName) || !strings.Contains(string(out), "deny") {
			t.Errorf("%s: %s", raw, out)
		}
	}
}

func TestTranscriptShellGrants(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := filepath.Join(home, ".omp", "agent", "sessions", "own.jsonl")
	h := transcriptHook()
	h.Transcripts.Grants = map[string][]string{transcriptOwner: {p}}
	for _, session := range []string{transcriptOwner, transcriptOther, ""} {
		ev := toolEvent(bashTool, map[string]any{commandKey: "cat " + p})
		ev[sessionKey] = session
		d := decideEvent(t, h, ev)
		if denied := d != nil && d.PermissionDecision == decisionDeny; denied != (session != transcriptOwner) {
			t.Errorf("%q: %+v", session, d)
		}
	}
	// An event cannot confer access just by claiming a transcript_path.
	ev := toolEvent(readTool, map[string]any{filePathKey: p})
	ev[sessionKey] = transcriptOther
	ev["transcript_path"] = p
	if d := decideEvent(t, h, ev); d == nil || d.PermissionDecision != decisionDeny {
		t.Fatal("self-reported transcript granted access")
	}
}

func TestTranscriptAmbiguousReaders(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	h := transcriptHook()
	for _, command := range []string{
		`python3 -c 'from pathlib import Path; print((Path.home()/".codex"/"sessions").rglob("*"))'`,
		`echo(){ cat "$1"; }; echo ~/.codex/sessions/other.jsonl`,
		`ls(){ cat "$1"; }; ls ~/.codex/sessions/other.jsonl`,
		`reader ~/.codex`,
		`cat <<EOF
$(cat ~/.codex/history.jsonl)
EOF`,
	} {
		d := decide(t, h, home, command, nil)
		if d == nil || d.PermissionDecision != decisionDeny {
			t.Errorf("reader passed: %s", command)
		}
	}
	t.Setenv("HOME", "")
	if d := transcriptDecision(t, h, transcriptOwner, readTool, "/unknown"); d == nil || d.PermissionDecision != decisionDeny {
		t.Fatal("missing home passed")
	}
}

func TestTranscriptOwnArtifacts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	h := transcriptHook()
	customClaude := filepath.Join(home, "custom-projects")
	customBrain := filepath.Join(home, "custom-brain")
	h.Transcripts.ClaudeProjects = []string{customClaude}
	h.Transcripts.AgyBrains = []string{customBrain}
	for _, root := range []string{filepath.Join(home, ".claude", "projects"), customClaude} {
		own := filepath.Join(root, "project", transcriptOwner, "tool-results", "result.txt")
		other := filepath.Join(root, "project", transcriptOther, "tool-results", "result.txt")
		for _, tool := range []string{readTool, editTool, multiEditTool, writeTool, grepTool, globTool} {
			if d := transcriptDecision(t, h, transcriptOwner, tool, own); d != nil && d.PermissionDecision == decisionDeny {
				t.Errorf("own spill refused by %s: %+v", tool, d)
			}
			if d := transcriptDecision(t, h, transcriptOwner, tool, other); d == nil || d.PermissionDecision != decisionDeny {
				t.Errorf("foreign spill passed by %s", tool)
			}
		}
		ev := toolEvent(bashTool, map[string]any{commandKey: "cat " + own})
		ev[sessionKey] = transcriptOwner
		if d := decideEvent(t, h, ev); d != nil && d.PermissionDecision == decisionDeny {
			t.Errorf("own spill shell read refused: %+v", d)
		}
		// Ownership cannot be conferred by a symlink in the session directory.
		if err := os.MkdirAll(filepath.Dir(own), 0o700); err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(filepath.Dir(own), "foreign.txt")
		if err := os.Symlink(other, alias); err != nil {
			t.Fatal(err)
		}
		if d := transcriptDecision(t, h, transcriptOwner, readTool, alias); d == nil || d.PermissionDecision != decisionDeny {
			t.Fatal("own spill symlink escaped")
		}
		for _, id := range []string{"", "../" + transcriptOwner, transcriptOther} {
			if d := transcriptDecision(t, h, id, readTool, own); d == nil || d.PermissionDecision != decisionDeny {
				t.Errorf("invalid/foreign session %q owns spill", id)
			}
		}
	}
	for _, root := range []string{filepath.Join(home, ".gemini", "antigravity-cli", "brain"), customBrain} {
		own := filepath.Join(root, transcriptOwner, "task.md")
		other := filepath.Join(root, transcriptOther, "task.md")
		for _, tool := range []string{readTool, writeTool, editTool, multiEditTool} {
			if d := transcriptDecision(t, h, transcriptOwner, tool, own); d != nil && d.PermissionDecision == decisionDeny {
				t.Errorf("own agy artifact refused by %s", tool)
			}
			if d := transcriptDecision(t, h, transcriptOwner, tool, other); d == nil || d.PermissionDecision != decisionDeny {
				t.Errorf("foreign agy artifact passed by %s", tool)
			}
		}
		patch := "*** Begin Patch\n*** Update File: " + own + "\n@@\n-old\n+new\n*** End Patch\n"
		if reason, _ := h.checkCodexApplyPatch(event{Session: transcriptOwner}, patch, home); reason != "" {
			t.Errorf("own artifact patch refused: %s", reason)
		}
	}
}

func TestTranscriptManagedScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := filepath.Join(home, ".codex", "sessions", "foreign.jsonl")
	for _, managed := range []bool{false, true} {
		h := hook()
		h.Transcripts.Managed = func(string) (bool, error) { return managed, nil }
		if d := transcriptDecision(t, h, transcriptOwner, readTool, p); (d != nil && d.PermissionDecision == decisionDeny) != managed {
			t.Errorf("file managed=%v: %+v", managed, d)
		}
		ev := toolEvent(bashTool, map[string]any{commandKey: "cat " + p})
		ev[sessionKey] = transcriptOwner
		if d := decideEvent(t, h, ev); (d != nil && d.PermissionDecision == decisionDeny) != managed {
			t.Errorf("shell managed=%v: %+v", managed, d)
		}
	}
	h := hook()
	h.Transcripts.Managed = func(string) (bool, error) { return false, errors.New("unreadable starts") }
	if d := transcriptDecision(t, h, transcriptOwner, readTool, p); d == nil || d.PermissionDecision != decisionDeny {
		t.Fatal("unreadable starts passed")
	}
}

func TestTranscriptWorktreePythonEdit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	h := transcriptHook()
	cwd := filepath.Join(home, "projects", "worktree")
	for _, command := range []string{
		`python3 -c 'from pathlib import Path; p = Path("docs/guide.md"); p.write_text(p.read_text().replace("old", "new"))'`,
		`python3 -c 'from pathlib import Path; p = Path("config.yaml"); p.write_text(p.read_text().replace(".claude", ".codex"))'`,
		"python3 <<'PY'\nfrom pathlib import Path\np = Path('docs/guide.md')\np.write_text(p.read_text().replace('old', 'new'))\nPY",
	} {
		if d := decide(t, h, cwd, command, nil); d != nil && d.PermissionDecision == decisionDeny {
			t.Errorf("worktree edit refused: %s: %+v", command, d)
		}
	}
}

func TestTranscriptConstructedPathReason(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	h := transcriptHook()
	for _, command := range []string{
		`python3 -c 'from pathlib import Path; print((Path.home()/".codex"/"sessions").rglob("*"))'`,
		`python3 -c 'import os; print(open(os.path.join(os.environ["HOME"], ".claude", "history.jsonl")).read())'`,
		`python3 -c 'from pathlib import Path; component = ".codex"; print((Path.home()/component/"history.jsonl").read_text())'`,
	} {
		d := decide(t, h, home, command, nil)
		if d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, "cannot resolve") || strings.Contains(d.Reason, transcriptReasonName) {
			t.Errorf("constructed path diagnostic: %s: %+v", command, d)
		}
	}
	// A literal relative harness root must still be checked against the cwd.
	if d := decide(t, h, home, `python3 -c 'from pathlib import Path; print(list(Path(".claude").rglob("*")))'`, nil); d == nil || d.PermissionDecision != decisionDeny {
		t.Errorf("relative protected root passed: %+v", d)
	}
}

func TestTranscriptReviewParenthesizedComponent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	h := transcriptHook()
	for _, expression := range []string{
		`Path.home() / (".codex") / "history.jsonl"`,
		`Path.home() / (((".codex"))) / "history.jsonl"`,
		`Path.home() / component / "history.jsonl"`,
		`Path.home().joinpath(((".codex")), "history.jsonl")`,
		`Path(f"{Path.home()}/{'.codex'}/history.jsonl")`,
	} {
		prefix := ""
		if strings.Contains(expression, "component") {
			prefix = `component = ".codex"; `
		}
		code := `from pathlib import Path; ` + prefix + `print((` + expression + `).read_text())`
		command := "python3 -c '" + strings.ReplaceAll(code, "'", "'\\''") + "'"
		if d := decide(t, h, filepath.Join(home, "worktree"), command, nil); d == nil || d.PermissionDecision != decisionDeny {
			t.Errorf("constructed protected path passed: %s: %+v", command, d)
		}
	}
}

func TestTranscriptReviewReplacementVariable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	h := transcriptHook()
	command := `python3 -c 'from pathlib import Path; old = ".claude"; p = Path("config.yaml"); p.write_text(p.read_text().replace(old, "new"))'`
	if d := decide(t, h, filepath.Join(home, "worktree"), command, nil); d != nil && d.PermissionDecision == decisionDeny {
		t.Errorf("replacement variable refused: %+v", d)
	}
}

func TestTranscriptRuntimeRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	h := transcriptHook()
	for _, command := range []string{
		`python3 -c 'from pathlib import Path; p = Path.home(); print([f.read_text() for f in p.rglob("*.jsonl")])'`,
		`python3 -c 'from pathlib import Path; name = ".cl" + "aude"; print(list((Path.home()/name).rglob("*.jsonl")))'`,
		`python3 -c 'import os; print(open(os.path.join(os.environ["HOME"], ".cl" + "aude", "history.jsonl")).read())'`,
		`python3 -c 'import os; print(open(os.path.join(os.getenv("CLAUDE_CONFIG_DIR"), "history.jsonl")).read())'`,
		`python3 -c 'import os; print(open(os.path.expanduser("~") + "/.cl" + "aude/history.jsonl").read())'`,
		`node -e 'const os = require("os"); console.log(require("fs").readdirSync(os.homedir()))'`,
		`node -e 'console.log(require("fs").readFileSync(process.env.HOME + "/.cl" + "aude/history.jsonl", "utf8"))'`,
		`ruby -e 'puts File.read(ENV["HOME"] + "/.cl" + "aude/history.jsonl")'`,
		`perl -e 'open(my $fh, "<", $ENV{HOME} . "/.cl" . "aude/history.jsonl"); print <$fh>;'`,
		"python3 <<'PY'\nfrom pathlib import Path\nprint(list(Path.home().rglob('*.jsonl')))\nPY",
		`python3 <<< 'from pathlib import Path; print(list(Path.home().rglob("*.jsonl")))'`,
	} {
		if d := decide(t, h, filepath.Join(home, "worktree"), command, nil); d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, "cannot resolve") {
			t.Errorf("runtime root passed: %s: %+v", command, d)
		}
	}
	// An explicit grant cannot authorize a target that cannot be resolved.
	h.Transcripts.Grants = map[string][]string{transcriptOwner: {home}}
	ev := toolEvent(bashTool, map[string]any{commandKey: `python3 -c 'from pathlib import Path; print(list(Path.home().rglob("*.jsonl")))'`})
	ev[sessionKey] = transcriptOwner
	if d := decideEvent(t, h, ev); d == nil || d.PermissionDecision != decisionDeny {
		t.Fatal("grant authorized unresolved target")
	}
	for _, code := range []string{
		`p = Path("fixture.jsonl"); print(p.read_text())`,
		`p = Path("config.yaml"); p.write_text(p.read_text().replace("Path.home()", "process.env.HOME"))`,
	} {
		command := "python3 -c '" + code + "'"
		if d := decide(t, h, filepath.Join(home, "worktree"), command, nil); d != nil && d.PermissionDecision == decisionDeny {
			t.Errorf("literal fixture/data refused: %s: %+v", command, d)
		}
	}
	if d := decide(t, h, home, "cat <<'DATA'\nPath.home()\nprocess.env.HOME\nDATA", nil); d != nil && d.PermissionDecision == decisionDeny {
		t.Errorf("here-document data refused: %+v", d)
	}
}

// A repository script's patterns and an edit's regexes are not transcript
// reads, even where the guard cannot resolve a cd and must also consider "/".
func TestTranscriptRepositoryCommands(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	h := transcriptHook()
	cwd := filepath.Join(home, "projects", "worktree")
	if err := os.MkdirAll(filepath.Join(cwd, "scripts"), 0o750); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/bash
ROOT=$(git rev-parse --show-toplevel 2>/dev/null) || exit 1
SUBDIR=$(git rev-parse --show-prefix)
while [[ $d == */* ]]; do d=${d%/*}; dirs[$d]=1; done
if [[ -n $d && $d == */* ]] || [ "$d" = */* ] || test "$d" = */*; then :; fi
( cd "$ROOT/$SUBDIR" && "$@" ) && rc=0 || rc=$?
`
	if err := os.WriteFile(filepath.Join(cwd, "scripts", "remote"), []byte(script), 0o700); err != nil { //nolint:gosec // an executable fixture
		t.Fatal(err)
	}
	for _, command := range []string{
		"REMOTE_CONTEXT=lab beekeeper run -- scripts/remote cargo fmt --check",
		"scripts/remote cargo fmt --check",
		`sed -i 's/.*foo/bar/' src/main.rs`,
		`sed -E 's/\s*foo/bar/' src/main.rs`,
		`sed 's/.*/x/' src/main.rs`,
		`sed 's/.*\///' src/main.rs`,
		`perl -pi -e 's/\s*foo/bar/' src/main.rs`,
		`perl -pi -e 's/\s*$//' src/main.rs`,
		`perl -pe 's/\s*$/\n/' src/main.rs`,
		`perl -0pi -e 's/a\n.*b/c/s' src/main.rs`,
		`awk '/^.*foo/ {print}' src/main.rs`,
		`grep -rn 'a/.*b' src`,
		`sed -i '/foo.*bar/d' src/main.rs`,
		`grep -n '/usr/.*bin' src/main.rs`,
		`awk '/a.*b/' src/main.rs`,
	} {
		if d := decide(t, h, cwd, command, nil); d != nil && d.PermissionDecision == decisionDeny {
			t.Errorf("repository command refused: %s: %+v", command, d)
		}
	}
}

// What the repository commands above may pass must not open a real read:
// embedded globs that reach a store, and every whole-word glob as before.
func TestTranscriptGlobsStayClosed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	h := transcriptHook()
	cwd := filepath.Join(home, "projects", "worktree")
	if err := os.MkdirAll(cwd, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "projects", "link")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		"cat /*/.codex/history.jsonl", "cat /*/*/*/*/*/.codex/history.jsonl", "cat ~/.c*/history.jsonl",
		"grep -r token /*", "tar cf - /*/*", "ROOT=$(git rev-parse --show-toplevel); SUBDIR=$(git rev-parse --show-prefix); ( cd \"$ROOT/$SUBDIR\" && cat */* )",
		`python3 -c 'import glob; print(glob.glob("/*/.codex/history.jsonl"))'`,
		`python3 -c 'import glob; print(glob.glob("/**/*.jsonl", recursive=True))'`,
		`python3 -c 'p = "x" + "/.c*/history.jsonl"; print(p)'`,
		`perl -e 'print glob("/.*/sessions/other.jsonl")'`,
		`perl -e 'print glob("~/.c*/history.jsonl")'`,
		`perl -e 'print glob("` + link + `/.c*/history.jsonl")'`,
		`awk 'BEGIN { system("cat /*/.codex/history.jsonl") }'`,
		`sed '1e cat /*/.codex/history.jsonl' src/main.rs`,
		`sed 'r ~/.codex/history.jsonl' src/main.rs`,
		"sed p /*/.codex/history.jsonl", "grep token /*/.codex/history.jsonl", "grep -r token " + home[:2] + "*", "awk 1 /*/.codex/history.jsonl",
		`sed 's/x/y/;r ` + filepath.Join(home, ".codex", "history.jsonl") + `' src/main.rs`,
		`[[ $(cat ~/.codex/history.jsonl) == x ]]`,
		`[[ -n $d && $(cat ~/.codex/history.jsonl) == x ]]`,
		`[[ $(<~/.codex/history.jsonl) == x ]]`,
		`[ "$(head ~/.codex/history.jsonl)" = x ]`,
		`test() { cat "$@"; }; test ~/.codex/history.jsonl`,
	} {
		if d := decide(t, h, cwd, command, nil); d == nil || d.PermissionDecision != decisionDeny {
			t.Errorf("not refused: %s: %+v", command, d)
		}
	}
}

func TestTranscriptInertRuntimeArguments(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := filepath.Join(home, "worktree")
	for _, command := range []string{
		`echo process.env.HOME`,
		`rg -n process.env.HOME src`,
		`echo 'process.env.HOME'`,
		`rg -n 'process.env.HOME' src`,
		`sh -c 'echo process.env.HOME'`,
	} {
		if d := decide(t, transcriptHook(), cwd, command, nil); d != nil && d.PermissionDecision == decisionDeny {
			t.Errorf("inert argument refused: %s: %+v", command, d)
		}
	}
}

func TestTranscriptRuntimeProgramInputs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, tc := range []struct{ name, code string }{
		{"python3", `import os; print(os.environ["HOME"])`},
		{"node", `console.log(process.env.HOME)`},
		{"ruby", `puts ENV["HOME"]`},
		{"perl", `print $ENV{HOME};`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := filepath.Join(home, "program")
			if err := os.WriteFile(script, []byte("#!/usr/bin/env "+tc.name+"\n"+tc.code), 0700); err != nil {
				t.Fatal(err)
			}
			for _, command := range []string{
				tc.name + " <<'CODE'\n" + tc.code + "\nCODE",
				tc.name + " <<< '" + tc.code + "'",
				"printf '%s' '" + tc.code + "' | " + tc.name,
				tc.name + " " + script,
				tc.name + " < " + script,
				script,
			} {
				if d := decide(t, transcriptHook(), home, command, nil); d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, "cannot resolve") {
					t.Errorf("runtime program passed: %s: %+v", command, d)
				}
			}
		})
	}
}
