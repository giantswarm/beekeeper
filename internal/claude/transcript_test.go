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
