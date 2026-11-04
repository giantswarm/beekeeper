package sandbox

import (
	"slices"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

func TestManagedHookPreservesLiteralExecutable(t *testing.T) {
	for _, exe := range []string{
		"/opt/beekeeper",
		"/opt/bee keeper/beekeeper",
		"/opt/bee'keeper/beekeeper",
		"/opt/$HOME/`literal`;keeper/beekeeper",
		"/opt/bee\nkeeper/beekeeper",
	} {
		t.Run(exe, func(t *testing.T) {
			p := Policy{Exe: exe}
			hooks := p.Settings()["hooks"].(map[string]any)["PreToolUse"].([]map[string]any)
			command := hooks[0]["hooks"].([]map[string]any)[0]["command"].(string)
			file, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(command), "hook")
			if err != nil {
				t.Fatal(err)
			}
			if len(file.Stmts) != 1 {
				t.Fatalf("hook contains shell statements: %s", command)
			}
			call, ok := file.Stmts[0].Cmd.(*syntax.CallExpr)
			if !ok {
				t.Fatalf("hook is not a literal command: %s", command)
			}
			// Expansion uses an empty synthetic environment and no command
			// substitution callback. No shell or executable is started.
			args, err := expand.Fields(&expand.Config{Env: expand.ListEnviron()}, call.Args...)
			if err != nil || !slices.Equal(args, []string{exe, "hook", "pretooluse"}) {
				t.Errorf("hook args = %q, %v", args, err)
			}
		})
	}
}
