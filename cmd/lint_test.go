package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLintBriefsCommandChecksFullURLs(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.md")
	if err := os.WriteFile(bad, []byte("Work #7 and o/r#8.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(dir, "good.md")
	goodText := "Work [o/r#7][issue] and https://github.com/o/r/pull/8#issuecomment-1.\n" +
		"Wait on note #3, timer #4, `beekeeper agents park --on o/r#7`, `multi\n#6`, color: #123; background-color: #123456; and session beekeeper#12.\n" +
		"````markdown\n```sh\ngh pr view #9 o/r#10\n```\n````\n\n" +
		"[issue]: https://github.com/o/r/issues/7\n"
	if err := os.WriteFile(good, []byte(goodText), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := &app{out: &out}
	run := func(path string) error {
		out.Reset()
		c := a.lintCmd()
		c.SetArgs([]string{"briefs", path})
		c.SetOut(&out)
		c.SetErr(&out)
		c.SilenceUsage, c.SilenceErrors = true, true
		return c.Execute()
	}
	err := run(bad)
	if Code(err) != ExitRefused {
		t.Fatalf("lint briefs bad.md = %v, want ExitRefused", err)
	}
	for _, want := range []string{
		"#7 without its full URL (https://github.com/o/r/issues/7)",
		"o/r#8 without its full URL (https://github.com/o/r/issues/8)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q lacks %q", out.String(), want)
		}
	}
	if err := run(good); err != nil || out.Len() != 0 {
		t.Fatalf("lint briefs good.md = %v, out %q", err, out.String())
	}
}

func TestLintBriefsCommandChecksShellRedirects(t *testing.T) {
	for _, tt := range []struct {
		code    string
		refused bool
	}{
		{"make test &>gate.log", true},
		{"make test >1.log", true},
		{"printf '%s\\n' 'a > b'", false},
		{"# use >out to capture output", false},
		{"cat <<'EOF'\na > b\nEOF", false},
		{"if true; then\nmake test >out", true},
	} {
		t.Run(tt.code, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "brief.md")
			if err := os.WriteFile(file, []byte("```sh\n"+tt.code+"\n```\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			a := &app{out: &out}
			c := a.lintCmd()
			c.SetArgs([]string{"briefs", file})
			c.SetOut(&out)
			c.SetErr(&out)
			c.SilenceUsage, c.SilenceErrors = true, true
			err := c.Execute()
			if tt.refused {
				if Code(err) != ExitRefused || !strings.Contains(out.String(), ":2: clobber:") && !strings.Contains(out.String(), ":3: clobber:") {
					t.Fatalf("got %v, output %q; want refusal with source line", err, out.String())
				}
			} else if err != nil || out.Len() != 0 {
				t.Fatalf("got %v, output %q; want clean", err, out.String())
			}
		})
	}
}
