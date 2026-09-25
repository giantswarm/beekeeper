package claude

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFirst(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	lines := `{"type":"summary"}
{"type":"user","isMeta":true,"message":{"role":"user","content":"meta"}}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"<system-reminder>x</system-reminder>"}]}}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"the brief"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}}
{"type":"user","message":{"role":"user","content":"later"}}
`
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	turn, found, err := First(path)
	if err != nil || !found || turn.Text != "the brief" {
		t.Errorf("First = %+v, %v, %v", turn, found, err)
	}
	if err := os.WriteFile(path, []byte(`{"type":"summary"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, found, err := First(path); err != nil || found {
		t.Errorf("no user turn: found %v, err %v", found, err)
	}
}

func TestModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	for _, c := range []struct{ name, lines, want string }{
		{"no assistant message yet", `{"type":"custom-title","customTitle":"x"}
{"type":"user","message":{"role":"user","content":"the brief"}}
`, ""},
		{"the last assistant model", `{"type":"assistant","message":{"model":"claude-haiku-4-5-20251001"}}
{"type":"assistant","message":{"model":"<synthetic>"}}
{"type":"user","message":{"role":"user","content":"next"}}
{"type":"assistant","message":{"model":"claude-sonnet-5"}}
`, "claude-sonnet-5"},
		{"a partial last line", `{"type":"assistant","message":{"model":"claude-haiku-4-5-20251001"}}
{"type":"assistant","message":{"model":"claude-sonn`, "claude-haiku-4-5-20251001"},
	} {
		if err := os.WriteFile(path, []byte(c.lines), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := Model(path); err != nil || got != c.want {
			t.Errorf("%s: Model = %q, %v; want %q", c.name, got, err, c.want)
		}
	}
	if _, err := Model(filepath.Join(t.TempDir(), "missing.jsonl")); err == nil {
		t.Error("a missing transcript: no error")
	}
}
