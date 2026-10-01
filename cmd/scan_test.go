package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	scanValue = "planted-Test-Value-4f9c2e81"
	scanRef   = "op://Shared/test item/credential"
)

func scanApp(t *testing.T) *app {
	t.Helper()
	dir := t.TempDir()
	a := &app{out: &bytes.Buffer{}, now: relayNow, cfg: &config.Config{StateDir: dir, Guide: config.Guide{Person: notePerson}, Scan: config.Scan{MinLength: 12}}}
	if err := a.scanAdd(strings.NewReader(scanValue+"\n"), scanRef); err != nil {
		t.Fatal(err)
	}
	return a
}

func bashResult(t *testing.T, stdout string) []byte {
	t.Helper()
	resp, err := json.Marshal(map[string]any{"stdout": stdout, "stderr": "", "interrupted": false})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(guard.ToolResult{Event: guard.PostToolUseEvent, Session: "scan-session", Tool: tToolBash, Response: resp})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPostToolUseRedactsAndFilesOneRotationNote(t *testing.T) {
	a := scanApp(t)
	for range 2 {
		out := a.redactResult(bashResult(t, "token="+scanValue))
		if out == nil || bytes.Contains(out, []byte(scanValue)) || !bytes.Contains(out, []byte("[redacted: "+scanRef+"]")) {
			t.Fatalf("answer = %s", out)
		}
	}
	store, err := state.Open(a.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Notes) != 1 {
		t.Fatalf("notes = %+v, want one rotation note", st.Notes)
	}
	n := st.Notes[0]
	if n.For != notePerson || !strings.HasPrefix(n.Text, rotateNote+scanRef+":") || strings.Contains(n.Text, scanValue) {
		t.Fatalf("note = %+v", n)
	}
	evs, err := store.Events(0, func(e state.Event) bool { return e.Verb == "scan.redact" })
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || !strings.Contains(evs[0].Detail, scanRef+" (1)") || strings.Contains(evs[0].Detail, scanValue) {
		t.Fatalf("scan.redact events = %+v", evs)
	}
}

func TestPostToolUseFilesNoNoteForAPatternHit(t *testing.T) {
	a := scanApp(t)
	tok := "ghp_" + strings.Repeat("A1b2", 9)
	if out := a.redactResult(bashResult(t, tok)); out == nil || bytes.Contains(out, []byte(tok)) {
		t.Fatalf("answer = %s", out)
	}
	store, _ := state.Open(a.cfg.StateDir)
	st, _ := store.Read()
	if len(st.Notes) != 0 {
		t.Fatalf("a pattern hit filed %+v", st.Notes)
	}
	if out := a.redactResult(bashResult(t, "all clean")); out != nil {
		t.Fatalf("a clean result was answered: %s", out)
	}
}

func TestScanAddRefusesAShortValue(t *testing.T) {
	a := scanApp(t)
	if err := a.scanAdd(strings.NewReader("short"), "x"); err == nil {
		t.Fatal("a short value was indexed")
	}
	ix, err := guard.LoadIndex(a.scanDir())
	if err != nil {
		t.Fatal(err)
	}
	if refs := ix.Refs(); len(refs) != 1 || refs[0] != scanRef {
		t.Fatalf("refs = %q", refs)
	}
}
