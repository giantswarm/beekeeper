package guard

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGuardPanicNamesCheckAndLogsStack(t *testing.T) {
	for _, format := range []string{"claude", "codex", agyHarness} {
		t.Run(format, func(t *testing.T) {
			lt := newLiveTree(t)
			var check, stack string
			h := Hook{Self: self, Shell: testShell, LiveDirs: func() []string { panic("injected") }, Panic: func(c string, s []byte) { check, stack = c, string(s) }}
			ev := toolEvent(bashTool, map[string]any{commandKey: "git worktree move " + lt.live + " /aside"})
			ev["cwd"] = lt.root
			ev[hookEventKey] = PreToolUseEvent
			raw, _ := json.Marshal(ev)
			var out []byte
			switch format {
			case "codex":
				out = h.DecideCodex(raw)
			case agyHarness:
				out, _ = json.Marshal(decideAgyCmd(t, h, lt.root, "git worktree move "+lt.live+" /aside"))
			default:
				out = h.Decide(raw)
			}
			if !strings.Contains(string(out), "deny") || !strings.Contains(string(out), "liveWorktreeRefusal") {
				t.Fatalf("response: %s", out)
			}
			if check != "liveWorktreeRefusal" || !strings.Contains(stack, "TestGuardPanicNamesCheckAndLogsStack") || !strings.Contains(stack, "injected") {
				t.Fatalf("check=%s stack=%s", check, stack)
			}
			if strings.Contains(string(out), "goroutine") {
				t.Fatal("stack exposed")
			}
		})
	}
}
