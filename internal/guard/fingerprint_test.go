package guard

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// planted is a test value: what a credential looks like to the index.
const planted = "planted-Test-Value-4f9c2e81"

const plantedRef = "op://Shared/test item/credential"

func plantedIndex(t *testing.T) *Index {
	t.Helper()
	ix, err := OpenIndex(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if n := ix.Add(plantedRef, planted+"\n"); n == 0 {
		t.Fatal("the planted value added no fingerprint")
	}
	return ix
}

func TestRedactReplacesAnIndexedValueInEveryShape(t *testing.T) {
	ix := plantedIndex(t)
	b64 := base64.StdEncoding.EncodeToString([]byte(planted))
	for name, text := range map[string]string{
		"alone":         planted,
		"KEY=value":     "export TOKEN=" + planted + " && run",
		"key: value":    "password: " + planted,
		"JSON":          `{"password":"` + planted + `"}`,
		"URL":           "https://user:" + planted + "@example.com/x",
		"path":          "/run/secrets/" + planted + "/file",
		"sentence end":  "the value is " + planted + ".",
		"base64":        "data:\n  token: " + b64,
		"base64 inline": "TOKEN=" + strings.TrimRight(b64, "="),
	} {
		t.Run(name, func(t *testing.T) {
			out, found := ix.Redact(text)
			if strings.Contains(out, planted) || strings.Contains(out, strings.TrimRight(b64, "=")) {
				t.Fatalf("the value survived: %q", out)
			}
			if !strings.Contains(out, "[redacted: "+plantedRef+"]") {
				t.Fatalf("no marker naming the reference: %q", out)
			}
			if len(found) != 1 || found[0].Ref != plantedRef || found[0].Count != 1 {
				t.Fatalf("findings = %+v", found)
			}
		})
	}
}

func TestRedactLeavesOrdinaryTextAlone(t *testing.T) {
	ix := plantedIndex(t)
	text := "planted-Test-Value-4f9c2e8 is one character short, planted-Test-Value-4f9c2e811 one long, sha 0123456789abcdef"
	if out, found := ix.Redact(text); out != text || found != nil {
		t.Fatalf("Redact changed ordinary text: %q %+v", out, found)
	}
}

func TestRedactNamesATokenPatternWithoutAnIndex(t *testing.T) {
	tok := "ghp_" + strings.Repeat("A1b2", 9)
	out, found := (&Index{}).Redact("auth: " + tok + "\n")
	if strings.Contains(out, tok) || !strings.Contains(out, "[redacted: github-pat]") {
		t.Fatalf("the token survived: %q", out)
	}
	if rule := tokenRules[0].id; len(found) != 1 || found[0].Rule != rule || found[0].Name() != "pattern "+rule {
		t.Fatalf("findings = %+v", found)
	}
	allowedLine := "fixture " + tok + " // " + allowMarker
	if out, found := (&Index{}).Redact(allowedLine); out != allowedLine || found != nil {
		t.Fatalf("a %s line was redacted: %q", allowMarker, out)
	}
}

func TestAddRefusesAShortValue(t *testing.T) {
	ix := plantedIndex(t)
	if n := ix.Add("short", "abc123"); n != 0 {
		t.Fatalf("a 6-byte value added %d fingerprints", n)
	}
}

func TestIndexHoldsNoValueAndOnlyTheUserReadsIt(t *testing.T) {
	dir := t.TempDir()
	ix, err := OpenIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	ix.Add(plantedRef, planted)
	if err := ix.Save(time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{indexKeyFile, indexFile} {
		info, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, want 0600", f, info.Mode().Perm())
		}
	}
	raw, _ := os.ReadFile(filepath.Join(dir, indexFile)) //nolint:gosec // the test's own directory
	if strings.Contains(string(raw), planted) || strings.Contains(string(raw), base64.StdEncoding.EncodeToString([]byte(planted))) {
		t.Fatal("the index file holds the value")
	}
	again, err := LoadIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := again.Redact(planted); len(found) != 1 {
		t.Fatal("the saved index does not match the value")
	}
	again.Drop("op://")
	if again.Len() != 0 {
		t.Fatalf("Drop left %d fingerprints", again.Len())
	}
}

func TestLoadIndexWithoutKeyMatchesPatternsOnly(t *testing.T) {
	ix, err := LoadIndex(filepath.Join(t.TempDir(), "none"))
	if err != nil {
		t.Fatal(err)
	}
	if out, found := ix.Redact(planted); out != planted || found != nil {
		t.Fatalf("an index without key matched: %+v", found)
	}
}

func TestPostToolUseRedactsTheResultInItsShape(t *testing.T) {
	ix := plantedIndex(t)
	resp, _ := json.Marshal(map[string]any{"stdout": "user=me\npass=" + planted + "\n", "stderr": "", "interrupted": false, "isImage": false, "exitCode": 0})
	raw, _ := json.Marshal(ToolResult{Event: PostToolUseEvent, Session: "s1", Tool: "Bash", Response: resp})
	out, r, found := PostToolUse(raw, ix)
	if out == nil || r.Tool != "Bash" || r.Session != "s1" {
		t.Fatalf("no answer: %s %+v", out, r)
	}
	if len(found) != 1 || found[0].Ref != plantedRef {
		t.Fatalf("findings = %+v", found)
	}
	var ans struct {
		Out struct {
			Event  string         `json:"hookEventName"`
			Output map[string]any `json:"updatedToolOutput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &ans); err != nil {
		t.Fatal(err)
	}
	if ans.Out.Event != PostToolUseEvent {
		t.Fatalf("event %q", ans.Out.Event)
	}
	got := ans.Out.Output
	if got["stdout"] != "user=me\npass=[redacted: "+plantedRef+"]\n" || got["interrupted"] != false || got["exitCode"] != float64(0) || len(got) != 5 {
		t.Fatalf("updatedToolOutput = %#v", got)
	}
}

func TestPostToolUseAnswersNothingForACleanResult(t *testing.T) {
	ix := plantedIndex(t)
	for _, raw := range []string{
		`{"hook_event_name":"PostToolUse","tool_name":"Read","tool_response":{"type":"text","file":{"content":"nothing here"}}}`,
		`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_response":{"stdout":"` + planted + `"}}`,
		`not json`,
	} {
		if out, _, _ := PostToolUse([]byte(raw), ix); out != nil {
			t.Errorf("answered %s for %s", out, raw)
		}
	}
}

func TestSweepTranscriptsCountsReferencesNeverValues(t *testing.T) {
	ix := plantedIndex(t)
	dir := t.TempDir()
	session := filepath.Join(dir, "-home-me-project")
	write := func(rel, content string) {
		p := filepath.Join(session, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	line, _ := json.Marshal(map[string]any{"type": "user", "toolUseResult": map[string]any{"stdout": "a=\"" + planted + "\"\nb=" + planted}})
	write("abc.jsonl", string(line)+"\n"+`{"type":"assistant","message":"clean"}`+"\n")
	write("abc/subagents/agent-1.jsonl", string(line)+"\n")
	write("abc/tool-results/x.txt", "spilled "+planted+"\n")
	write("abc/tool-results/clean.txt", "nothing\n")
	write("abc/notes.md", planted) // not a transcript

	rep, err := SweepTranscripts(dir, ix)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Files != 4 {
		t.Errorf("files = %d, want 4", rep.Files)
	}
	if len(rep.Leaks) != 1 || rep.Leaks[0].Ref != plantedRef || rep.Leaks[0].Count != 5 || rep.Leaks[0].Files != 3 {
		t.Fatalf("leaks = %+v", rep.Leaks)
	}
	js, _ := json.Marshal(rep)
	if strings.Contains(string(js), planted) {
		t.Fatal("the report carries the value")
	}
}

// fakeTools puts sops and op scripts on PATH that answer like the real ones.
func fakeTools(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	scripts := map[string]string{
		"sops": `#!/bin/sh
case "$*" in
*broken.enc.yaml) echo "Failed to get the data key" >&2; exit 128;;
esac
printf '%s' '{"stringData":{"password":"` + planted + `","port":"8080"},"list":["another-long-secret-01"]}'
`,
		"op": `#!/bin/sh
case "$1 $2" in
"item list") printf '%s' '[{"id":"i1"},{"id":"i2"}]';;
"item get") cat >/dev/null
  printf '%s\n' '{"title":"test item","fields":[{"label":"username","type":"STRING","value":"someone-long-name"},{"label":"credential","type":"CONCEALED","value":"` + planted + `"}]}'
  printf '%s\n' '{"title":"ssh","fields":[{"label":"private key","type":"SSHKEY","value":"ssh-private-material-xyz"}]}';;
*) exit 1;;
esac
`,
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil { //nolint:gosec // a test script
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestIndexSOPSAndVaultReadTheirSourcesQuietly(t *testing.T) {
	fakeTools(t)
	ix, err := OpenIndex(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	n, err := IndexSOPS(t.Context(), ix, []string{"a.enc.yaml", "broken.enc.yaml"})
	if n == 0 || err == nil || !strings.Contains(err.Error(), "broken.enc.yaml: exit 128") {
		t.Fatalf("IndexSOPS = %d, %v", n, err)
	}
	if _, f := ix.Redact(planted); len(f) != 1 || f[0].Ref != "sops://a.enc.yaml#stringData.password" {
		t.Fatalf("the SOPS value is not indexed by its key: %+v", f)
	}
	if _, f := ix.Redact("another-long-secret-01"); len(f) != 1 || f[0].Ref != "sops://a.enc.yaml#list.0" {
		t.Fatalf("the list value is not indexed: %+v", f)
	}
	if _, f := ix.Redact("8080"); f != nil {
		t.Fatal("a short value was indexed")
	}
	if _, err := IndexVault(t.Context(), ix, "Shared"); err != nil {
		t.Fatal(err)
	}
	if _, f := ix.Redact(planted); len(f) != 1 || f[0].Ref != plantedRef {
		t.Fatalf("the vault's concealed field is not indexed under its reference: %+v", f)
	}
	if _, f := ix.Redact("ssh-private-material-xyz"); len(f) != 1 {
		t.Fatal("the SSH key field is not indexed")
	}
	if _, f := ix.Redact("someone-long-name"); f != nil {
		t.Fatal("a plain STRING field was indexed")
	}
}
