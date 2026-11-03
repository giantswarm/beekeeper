package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
)

// writeTail keeps one tail under the app's state directory, as a failed
// run would, and answers its path. The header carries the given time, so
// the listing's order and window are the test's.
func writeTail(t *testing.T, a *app, name, head string, exit int, at time.Time, body string) string {
	t.Helper()
	dir := a.runTailsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+".log")
	content := "# beekeeper run tail: exit " + strconv.Itoa(exit) + " at " + at.UTC().Format(time.RFC3339) + ": " + head + "\n" + body
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// `run tail` lists the kept tails newest first with exit, time, size and
// command; prints one by a whole or partial name; refuses a name no tail
// matches and one two match; and says so when nothing was kept.
func TestRunTailListsAndPrints(t *testing.T) {
	a := &app{out: &bytes.Buffer{}, now: relayNow, cfg: &config.Config{StateDir: t.TempDir()}}
	writeTail(t, a, "memcap-2516344-492870", "go test ./...", 1, relayNow.Add(-2*time.Hour),
		"--- FAIL: TestFlaky (0.04s)\nFAIL\n")
	writeTail(t, a, "remote-11111111-2222-3333-4444-555555555555", "cargo test --all", 101, relayNow.Add(-time.Hour),
		"error: test failed, to rerun pass `--lib`\n")
	writeTail(t, a, "memcap-1-1", "make test", 2, relayNow.Add(-30*24*time.Hour), "old\n")
	list := func(args ...string) string {
		t.Helper()
		a.out = &bytes.Buffer{}
		c := a.runTailCmd()
		c.SetArgs(args)
		if err := c.Execute(); err != nil {
			t.Fatalf("run tail %v: %v", args, err)
		}
		return a.out.(*bytes.Buffer).String()
	}
	got := list()
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "remote-11111111") || !strings.Contains(lines[0], "exit 101") ||
		!strings.Contains(lines[0], "cargo test --all") || !strings.Contains(lines[2], "memcap-1-1") {
		t.Fatalf("listing:\n%s", got)
	}
	if !strings.Contains(got, " B") && !strings.Contains(got, " KiB") {
		t.Errorf("listing carries no size:\n%s", got)
	}
	if out := list("--since", "47h"); !strings.Contains(out, "remote-1111") || strings.Contains(out, "memcap-1-1") {
		t.Errorf("--since 47h:\n%s", out)
	}
	a.json = true
	a.out = &bytes.Buffer{}
	c := a.runTailCmd()
	c.SetArgs(nil)
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	var recs []tailRecord
	if err := json.Unmarshal(a.out.(*bytes.Buffer).Bytes(), &recs); err != nil || len(recs) != 3 || recs[0].Exit != 101 || recs[0].Name == "" {
		t.Fatalf("--json = %s (%v)", a.out.(*bytes.Buffer).String(), err)
	}
	a.json = false
	if out := list("492870"); !strings.Contains(out, "--- FAIL: TestFlaky") {
		t.Errorf("tail by partial name:\n%s", out)
	}
	a.out = &bytes.Buffer{}
	c = a.runTailCmd()
	c.SetArgs([]string{"memcap"})
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "matches") {
		t.Fatalf("ambiguous name: %v", err)
	}
	a.out = &bytes.Buffer{}
	c = a.runTailCmd()
	c.SetArgs([]string{"nosuchrun"})
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "no kept tail") {
		t.Fatalf("unknown name: %v", err)
	}
	empty := &app{out: &bytes.Buffer{}, now: relayNow, cfg: &config.Config{StateDir: t.TempDir()}}
	empty.out = &bytes.Buffer{}
	c = empty.runTailCmd()
	c.SetArgs(nil)
	if err := c.Execute(); err != nil || !strings.Contains(empty.out.(*bytes.Buffer).String(), "no kept tail") {
		t.Fatalf("empty state: %v %q", err, empty.out.(*bytes.Buffer).String())
	}
}

// redactOutput is the masking a kept tail goes through: the scan index's
// values and the outbound guard's token patterns, the same the PostToolUse
// hook applies before the model sees a tool result. An index that does not
// load still masks the patterns.
func TestRedactOutputMasksValuesAndTokens(t *testing.T) {
	a := scanApp(t)
	if got := a.redactOutput("PASS\n--- FAIL: TestX\ntoken=" + scanValue + "\n"); strings.Contains(got, scanValue) ||
		!strings.Contains(got, "[redacted: "+scanRef+"]") || !strings.Contains(got, "--- FAIL: TestX") {
		t.Errorf("masked = %q", got)
	}
	tok := "ghp_" + strings.Repeat("A1b2", 9)
	if got := a.redactOutput(tok); strings.Contains(got, tok) || !strings.Contains(got, "[redacted: github-pat]") {
		t.Errorf("token = %q", got)
	}
	broken := &app{cfg: &config.Config{StateDir: t.TempDir()}}
	broken.cfg.StateDir = filepath.Join(broken.cfg.StateDir, "unreadable-scan")
	if got := broken.redactOutput("auth: " + tok); strings.Contains(got, tok) {
		t.Errorf("without an index the token survives: %q", got)
	}
}

// runTail reads run.tail: off and an unreadable size keep nothing; a size
// makes the ring.
func TestRunTailSizing(t *testing.T) {
	for _, c := range []struct {
		tail  string
		keeps bool
	}{
		{"off", false}, {"", false}, {"nonsense", false}, {"256K", true}, {"1M", true},
	} {
		a := &app{cfg: &config.Config{StateDir: t.TempDir(), Run: config.RunConfig{Tail: c.tail}}}
		if got := a.runTail() != nil; got != c.keeps {
			t.Errorf("run.tail %q keeps: %v, want %v", c.tail, got, c.keeps)
		}
	}
}
