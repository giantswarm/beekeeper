package cmd

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A pipe and a device that is no terminal are read by a script; a regular
// file (a session's tool output) and a writer that is no file by the caller.
func TestScriptedReadsAPipeOrADevice(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = null.Close() }()
	file, err := os.Create(filepath.Join(t.TempDir(), "output"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	for _, c := range []struct {
		name string
		w    io.Writer
		want bool
	}{
		{"a pipe", w, true},
		{"/dev/null", null, true},
		{"a regular file", file, false},
		{"a buffer", &bytes.Buffer{}, false},
	} {
		if got := scripted(c.w); got != c.want {
			t.Errorf("scripted(%s) = %t, want %t", c.name, got, c.want)
		}
	}
}

// A script's read of a reading command prints the whole text, however
// often the caller has read it, and leaves the caller's mark where it was:
// a grep for a busy lane finds it busy, and the caller's next own read
// still says what changed.
func TestScriptedReadPrintsEverythingAndKeepsTheMark(t *testing.T) {
	needsPlatform(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("stateDir: "+dir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	piped := false
	defer func(orig func(io.Writer) bool) { scripted = orig }(scripted)
	scripted = func(io.Writer) bool { return piped }

	var out bytes.Buffer
	a := &app{cfg: cfg, store: store, now: time.Now(), out: &out, as: agentOne}
	read := func(lane string) string {
		out.Reset()
		facts := textFacts("lane", "ap: "+lane+"\n")
		if err := a.delta("test", false, facts, func() { out.WriteString("ap: " + lane + "\n") }); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	read("merging giantswarm/klaus#1")
	piped = true
	for range 2 {
		if got := read("merging giantswarm/klaus#1"); got != "ap: merging giantswarm/klaus#1\n" {
			t.Errorf("a script's read of a busy lane printed %q", got)
		}
	}
	if got := read("free"); got != "ap: free\n" {
		t.Errorf("a script's read of a quiet lane printed %q", got)
	}
	piped = false
	if got := read("free"); !strings.HasPrefix(got, "since ") || !strings.Contains(got, "lane: ap: free") {
		t.Errorf("the caller's read after a script's printed %q, want the change since its own last read", got)
	}
}
