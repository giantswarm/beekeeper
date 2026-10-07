package claude

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
)

// writeAged writes a file and dates it back past the racy window, as a
// file written on an earlier tick is.
func writeAged(t *testing.T, path, body string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-age)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// A record whose file did not change is not parsed again; a changed one is.
func TestReadRecordParsesOnlyAChangedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local_a.json")
	writeAged(t, path, `{"title":"one","isArchived":true}`, time.Minute)
	r, ok := readRecord(path)
	if !ok || r.Title != "one" || !r.IsArchived {
		t.Fatalf("first read: %+v %v", r, ok)
	}
	r.Title = "changed by its caller"

	// The same size and time: the cached record, the caller's change not in it.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"title":"two","isArchived":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	if r, _ := readRecord(path); r.Title != "one" {
		t.Errorf("an unchanged record was parsed again or shared: %q", r.Title)
	}

	writeAged(t, path, `{"title":"three, longer"}`, 30*time.Second)
	if r, _ := readRecord(path); r.Title != "three, longer" || r.IsArchived {
		t.Errorf("a changed record was not parsed again: %+v", r)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, ok := readRecord(path); ok {
		t.Error("a removed record still reads")
	}
}

// A record written within the racy window is read again even with the same
// size and time: its change may not have moved the time.
func TestReadRecordRereadsARacyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local_b.json")
	if err := os.WriteFile(path, []byte(`{"title":"aa"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if r, _ := readRecord(path); r.Title != "aa" {
		t.Fatalf("first read: %q", r.Title)
	}
	fi, _ := os.Stat(path)
	if err := os.WriteFile(path, []byte(`{"title":"bb"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	if r, _ := readRecord(path); r.Title != "bb" {
		t.Errorf("a racy record was taken from the cache: %q", r.Title)
	}
}

// The records are found by id in the folders listed, and a record added to
// a folder is found once the folder changed.
func TestRecordPathsFollowTheFolders(t *testing.T) {
	cfg := &config.Config{}
	cfg.Claude.DesktopDir = t.TempDir()
	dir := filepath.Join(cfg.Claude.DesktopDir, "account", "org")
	writeAged(t, filepath.Join(dir, "local_a.json"), `{"title":"a"}`, time.Minute)
	writeAged(t, filepath.Join(dir, "other.txt"), `x`, time.Minute)
	if err := os.Chtimes(dir, time.Now().Add(-time.Minute), time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := recordFiles(cfg); len(got) != 1 || filepath.Base(got[0]) != "local_a.json" {
		t.Fatalf("recordFiles = %v", got)
	}
	if r, ok := ReadRecord(cfg, "local_b"); ok {
		t.Fatalf("a missing record reads: %+v", r)
	}
	writeAged(t, filepath.Join(dir, "local_b.json"), `{"title":"b"}`, 0)
	if r, ok := ReadRecord(cfg, "local_b"); !ok || r.Title != "b" {
		t.Errorf("a new record is not found: %+v %v", r, ok)
	}
	if got := recordFiles(cfg); len(got) != 2 {
		t.Errorf("recordFiles after an add = %v", got)
	}
}

// A transcript is found in the project folder named after the session's
// folder with one look, anywhere else with a scan, and kept.
func TestTranscriptFindsAndKeepsThePath(t *testing.T) {
	cfg := &config.Config{}
	cfg.Claude.ProjectsDir = t.TempDir()
	cwd := "/home/me/.local/state/wt-1"
	if got := ProjectFolder(cwd); got != "-home-me--local-state-wt-1" {
		t.Fatalf("ProjectFolder = %q", got)
	}
	own := filepath.Join(cfg.Claude.ProjectsDir, ProjectFolder(cwd), "s1.jsonl")
	writeAged(t, own, "{}\n", 0)
	if got := Transcript(cfg, "s1", cwd); got != own {
		t.Errorf("by its folder: %q", got)
	}
	elsewhere := filepath.Join(cfg.Claude.ProjectsDir, "-somewhere-else", "s2.jsonl")
	writeAged(t, elsewhere, "{}\n", 0)
	if got := Transcript(cfg, "s2", cwd); got != elsewhere {
		t.Errorf("by a scan: %q", got)
	}
	if err := os.Remove(elsewhere); err != nil {
		t.Fatal(err)
	}
	if got := Transcript(cfg, "s2", cwd); got != "" {
		t.Errorf("a removed transcript is still found: %q", got)
	}
	if got := Transcript(cfg, "../x", cwd); got != "" {
		t.Errorf("an id with a path: %q", got)
	}
}

// A session with no transcript anywhere is not scanned for again within
// transcriptMiss while no project folder is added, but its own folder is
// looked at every time.
func TestTranscriptMissScansOnce(t *testing.T) {
	cfg := &config.Config{}
	cfg.Claude.ProjectsDir = t.TempDir()
	age := func(dir string) {
		t.Helper()
		at := time.Now().Add(-time.Minute)
		if err := os.Chtimes(dir, at, at); err != nil {
			t.Fatal(err)
		}
	}
	b := filepath.Join(cfg.Claude.ProjectsDir, "-b")
	if err := os.Mkdir(b, 0o700); err != nil {
		t.Fatal(err)
	}
	age(cfg.Claude.ProjectsDir)
	if got := Transcript(cfg, "s3", "/a"); got != "" {
		t.Fatalf("no transcript yet: %q", got)
	}
	writeAged(t, filepath.Join(b, "s3.jsonl"), "{}\n", 0)
	if got := Transcript(cfg, "s3", "/a"); got != "" {
		t.Errorf("scanned again within transcriptMiss: %q", got)
	}
	own := filepath.Join(cfg.Claude.ProjectsDir, ProjectFolder("/a"), "s3.jsonl")
	writeAged(t, own, "{}\n", 0)
	if got := Transcript(cfg, "s3", "/a"); got != own {
		t.Errorf("its own folder not looked at: %q", got)
	}

	// A project folder added: the next look scans again.
	if got := Transcript(cfg, "s4", ""); got != "" {
		t.Fatalf("no transcript yet: %q", got)
	}
	added := filepath.Join(cfg.Claude.ProjectsDir, "-c", "s4.jsonl")
	writeAged(t, added, "{}\n", 0)
	if got := Transcript(cfg, "s4", ""); got != added {
		t.Errorf("not scanned again after a folder was added: %q", got)
	}
}

// The context of a transcript is read again only once it changed.
func TestContextFollowsTheTranscript(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	line := func(id string, input int) string {
		return `{"type":"assistant","timestamp":"2026-10-07T08:00:00Z","message":{"id":"` + id +
			`","model":"m","usage":{"input_tokens":` + strconv.Itoa(input) + `,"output_tokens":1}}}` + "\n"
	}
	writeAged(t, path, line("a", 99), time.Minute)
	if got := Context(path); got != 100 {
		t.Fatalf("Context = %d", got)
	}
	writeAged(t, path, line("a", 99)+line("b", 199), 30*time.Second)
	if got := Context(path); got != 200 {
		t.Errorf("Context after a request = %d", got)
	}
	if got := Context(""); got != 0 {
		t.Errorf("Context without a transcript = %d", got)
	}
}

// A watch's transcript read is taken again while the transcript is unchanged
// and the read fresh; a change or an old read reads it again.
func TestTranscriptCacheRereadsOnlyWhatChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	turn := func(at string) string {
		return `{"type":"assistant","timestamp":"` + at + `","message":{"id":"` + at +
			`","model":"m","usage":{"input_tokens":9,"output_tokens":1},"content":[{"type":"tool_use","id":"t` + at + `","name":"Bash","input":{}}]}}` + "\n"
	}
	writeAged(t, path, turn("2026-10-07T08:00:00Z"), time.Minute)
	c := NewTranscriptCache(5 * time.Minute)
	now := time.Date(2026, 10, 7, 8, 30, 0, 0, time.UTC)
	if _, a := c.Read(path, now); a.LastHour.ToolCalls != 1 {
		t.Fatalf("first read: %+v", a.LastHour)
	}
	// Past the hour of its one call: within fresh, the read is taken again.
	if _, a := c.Read(path, now.Add(4*time.Minute)); a.LastHour.ToolCalls != 1 {
		t.Errorf("a fresh read was not taken: %+v", a.LastHour)
	}
	if _, a := c.Read(path, now.Add(31*time.Minute)); a.LastHour.ToolCalls != 0 {
		t.Errorf("an old read was taken: %+v", a.LastHour)
	}
	writeAged(t, path, turn("2026-10-07T08:00:00Z")+turn("2026-10-07T09:00:00Z"), 30*time.Second)
	if _, a := c.Read(path, now.Add(32*time.Minute)); a.LastHour.ToolCalls != 1 || a.Total.ToolCalls != 2 {
		t.Errorf("a changed transcript was not read again: %+v %+v", a.LastHour, a.Total)
	}
}
